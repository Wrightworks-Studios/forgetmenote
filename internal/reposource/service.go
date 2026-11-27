package reposource

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/pg"
)

type State struct {
	LastHandoffOutcome string      `json:"lastHandoffOutcome,omitempty"`
	Handoff            string      `json:"handoff,omitempty"`
	HandoffPaused      bool        `json:"handoffPaused,omitempty"`
	LastHandoff        string      `json:"lastHandoff,omitempty"`
	NotebookLayout     bool        `json:"notebookLayout,omitempty"`
	HistoricalOnly     bool        `json:"historicalOnly,omitempty"`
	IndexConfig        IndexConfig `json:"indexConfig"`
	Index              IndexStatus `json:"index"`
	ID                 string      `json:"id"`
	Project            string      `json:"project"`
	Actor              string      `json:"actor"`
	MapActor           string      `json:"mapActor,omitempty"`
	Installation       int64       `json:"installation"`
	Repository         Repository  `json:"repository"`
	Connected          bool        `json:"connected"`
	Paused             bool        `json:"paused"`
	Generation         uint64      `json:"generation"`
	Downloaded         string      `json:"downloaded"`
	LastRefresh        time.Time   `json:"lastRefresh"`
	Pending            bool        `json:"pending"`
	NextAttempt        time.Time   `json:"nextAttempt"`
	Failures           int         `json:"failures"`
	Error              string      `json:"error"`
	BaselineRequired   bool        `json:"baselineRequired"`
	CacheBytes         int64       `json:"cacheBytes"`
	EvidenceBytes      int64       `json:"evidenceBytes"`
	Deliveries         []string    `json:"deliveries,omitempty"`
}
type diskState struct {
	Version int              `json:"version"`
	Active  string           `json:"active"`
	Sources map[string]State `json:"sources"`
}
type notebook struct {
	mu      sync.Mutex
	project string
	dir     string
	doc     *pg.Doc[diskState]
	state   diskState
	// seen counts the states read from other gateways' writes.
	seen     uint64
	evidence map[string]*evidenceListing
	mirrors  map[string]mirrored
}

// stateKind names the per-notebook source state documents. Each holds what
// state.json held. Each source's durable files are in blob storage and its
// caches in the notebook's directory under root; blobs.go lists which is which.
const stateKind = "repository-sources"

func storageContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// Manager is the sole owner of source caches and their durable provenance. The
// gateway supplies current notebook-owner authority; it is checked at external
// boundaries as well as before accepting fetched data.
type Manager struct {
	indexPolicy    func(State) (IndexConfig, string)
	indexMu        sync.Mutex
	lexical        *lexicalCache
	embed          EmbedFunc
	mapModel       func() string
	classify       MapClassifyFunc
	mu             sync.Mutex
	db             *pg.DB
	root           string
	blobs          blob.Store
	local          bool // blobs keeps its objects in root's own files.
	github         *GitHub
	check          func(project, actor string) error
	notebooks      map[string]*notebook
	work           chan struct{}
	running        map[string]context.CancelFunc
	processingJobs map[string]context.CancelFunc
	closed         bool
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	cacheFor       func(string) *cache
}

func Open(db *pg.DB, root string, blobs blob.Store, g *GitHub, check func(string, string) error) (*Manager, error) {
	if check == nil {
		return nil, ErrDenied
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	m := &Manager{db: db, root: root, blobs: blobs, local: holds(blobs, root), github: g, check: check, notebooks: map[string]*notebook{}, work: make(chan struct{}, 2), running: map[string]context.CancelFunc{}, processingJobs: map[string]context.CancelFunc{}}
	m.cacheFor = func(dir string) *cache { return &cache{dir: dir, limits: g.Config.Limits} }
	projects, err := m.stored()
	if err != nil {
		return nil, err
	}
	for _, project := range projects {
		if _, err = m.book(project); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// stored lists the notebooks with source state, on any gateway.
func (m *Manager) stored() ([]string, error) {
	ctx, cancel := storageContext()
	defer cancel()
	projects, err := m.db.DocumentIDs(ctx, m.db.Pool, stateKind)
	if err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	if len(projects) > 10000 {
		return nil, ErrLimit
	}
	return projects, nil
}
func (m *Manager) book(project string) (*notebook, error) {
	if !identifier.MatchString(project) {
		return nil, ErrDenied
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrUnavailable
	}
	if n := m.notebooks[project]; n != nil {
		return n, nil
	}
	n := &notebook{project: project, dir: filepath.Join(m.root, project), doc: pg.NewDoc[diskState](m.db, stateKind, project, 4<<20)}
	if err := n.refresh(); err != nil {
		return nil, err
	}
	m.notebooks[project] = n
	return n, nil
}

// lock holds the notebook's state, reloaded when another gateway changed it.
// A state that cannot be read or validated fails closed.
func (n *notebook) lock() error {
	n.mu.Lock()
	if err := n.refresh(); err != nil {
		n.mu.Unlock()
		return err
	}
	return nil
}
func (n *notebook) refresh() error {
	ctx, cancel := storageContext()
	defer cancel()
	state, changed, err := n.doc.Load(ctx)
	if err == nil && !changed {
		return nil
	}
	if err == nil {
		if state.Version == 0 && state.Sources == nil && state.Active == "" {
			state = diskState{Version: 1, Sources: map[string]State{}}
		}
		err = validState(n.project, state)
	}
	if err != nil {
		n.doc.Forget()
		n.state = diskState{Version: 1, Sources: map[string]State{}}
		return errors.Join(ErrUnavailable, err)
	}
	n.state = state
	n.seen++
	return nil
}

// cache returns source id's cache. Its durable files are in blob storage,
// unless that storage keeps them in the cache's own files. The caller holds
// n's lock.
func (m *Manager) cache(n *notebook, id string) *cache {
	c := m.cacheFor(filepath.Join(n.dir, id))
	if !m.local {
		c.blobs = m.blobs
		c.key = sourcesDir + "/" + n.project + "/" + id
		c.evidence = func(ctx context.Context) (map[string]bool, error) {
			return n.storedEvidence(ctx, c.blobs, c.key, id)
		}
	}
	return c
}
func validState(project string, state diskState) error {
	if (state.Version != 1 && state.Version != 2) || state.Sources == nil || len(state.Sources) > 64 {
		return errors.New("invalid repository source state")
	}
	for id, s := range state.Sources {
		if !identifier.MatchString(id) || s.ID != id || s.Project != project || !validRepo(s.Repository) || s.Actor == "" || s.Installation <= 0 {
			return errors.New("invalid repository source state")
		}
	}
	if state.Active != "" {
		if _, ok := state.Sources[state.Active]; !ok {
			return errors.New("invalid repository source state")
		}
	}
	return nil
}

// save writes the notebook's state over the version it last read. A failed
// save restores the previous state here, and the next lock rereads the stored
// one, so a write that committed despite an error stays visible.
func (n *notebook) save(s State) error {
	old, had := n.state.Sources[s.ID]
	n.state.Sources[s.ID] = s
	oldVersion := n.state.Version
	if s.Handoff != "" || s.LastHandoff != "" || s.NotebookLayout || s.HistoricalOnly {
		n.state.Version = 2
	}
	ctx, cancel := storageContext()
	defer cancel()
	err := n.doc.Save(ctx, n.state)
	if err == nil {
		return nil
	}
	n.state.Version = oldVersion
	if had {
		n.state.Sources[s.ID] = old
	} else {
		delete(n.state.Sources, s.ID)
	}
	switch {
	case errors.Is(err, pg.ErrConflict):
		return errors.Join(ErrConflict, err)
	case errors.Is(err, pg.ErrTooLarge):
		return errors.Join(ErrLimit, err)
	}
	return errors.Join(ErrUnavailable, err)
}
func (m *Manager) Status(project string) (State, error) {
	n, err := m.book(project)
	if err != nil {
		return State{}, err
	}
	if err := n.lock(); err != nil {
		return State{}, err
	}
	defer n.mu.Unlock()
	s := n.state.Sources[n.state.Active]
	if err := m.syncIndexPolicy(n, &s); err != nil {
		return s, err
	}
	s.Deliveries = nil
	return s, nil
}
func (m *Manager) Connect(ctx context.Context, project, actor, userToken string, installation, repository int64) (State, error) {
	if m.github == nil {
		return State{}, ErrUnavailable
	}
	if err := m.check(project, actor); err != nil {
		return State{}, ErrDenied
	}
	choices, err := m.github.Choices(ctx, userToken)
	if err != nil {
		return State{}, err
	}
	var repo Repository
	for _, c := range choices {
		if c.ID == repository && c.Installation == installation {
			repo = c.Repository
			break
		}
	}
	if repo.ID == 0 {
		return State{}, ErrDenied
	}
	return m.connectResolved(project, actor, installation, repo)
}

// ConnectInstallation is for the instance operator boundary, not ordinary
// notebook owners. The caller must verify instance Admin and explicit consent.
func (m *Manager) ConnectInstallation(ctx context.Context, project, actor string, installation, repository int64) (State, error) {
	if m.github == nil {
		return State{}, ErrUnavailable
	}
	if err := m.check(project, actor); err != nil {
		return State{}, ErrDenied
	}
	repo, _, _, err := m.github.Resolve(ctx, installation, repository)
	if err != nil {
		return State{}, err
	}
	return m.connectResolved(project, actor, installation, repo)
}

func (m *Manager) connectResolved(project, actor string, installation int64, repo Repository) (State, error) {
	n, err := m.book(project)
	if err != nil {
		return State{}, err
	}
	if err := n.lock(); err != nil {
		return State{}, err
	}
	defer n.mu.Unlock()
	if n.state.Sources[n.state.Active].Handoff != "" {
		return State{}, ErrConflict
	}
	if s := n.state.Sources[n.state.Active]; s.Connected {
		return State{}, ErrConflict
	}
	if len(n.state.Sources) >= 64 {
		return State{}, ErrLimit
	}
	if err = m.check(project, actor); err != nil {
		return State{}, ErrDenied
	}
	var b [16]byte
	rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	oldActive := n.state.Active
	n.state.Active = id
	s := State{ID: id, Project: project, Actor: actor, Installation: installation, Repository: repo, Connected: true, Generation: 1, Pending: true, BaselineRequired: true}
	// Layout belongs to the verified repository's content format. A new grant
	// must not turn its Drafts, Trash, or policy files into ordinary source hits.
	for _, previous := range n.state.Sources {
		if previous.Repository.ID == repo.ID && previous.NotebookLayout {
			s.NotebookLayout = true
			break
		}
	}
	if err = n.save(s); err != nil {
		if _, saved := n.state.Sources[id]; !saved {
			n.state.Active = oldActive
		}
	}
	return s, err
}
func (m *Manager) Control(project, actor, action string) (State, error) {
	if action != "pause" && action != "resume" && action != "disconnect" {
		return State{}, ErrDenied
	}
	if err := m.check(project, actor); err != nil {
		return State{}, ErrDenied
	}
	m.mu.Lock()
	if cancel := m.processingJobs[project]; cancel != nil {
		cancel()
	}
	if cancel := m.running[project]; cancel != nil {
		cancel()
	}
	m.mu.Unlock()
	n, err := m.book(project)
	if err != nil {
		return State{}, err
	}
	if err := n.lock(); err != nil {
		return State{}, err
	}
	defer n.mu.Unlock()
	s := n.state.Sources[n.state.Active]
	if s.Handoff != "" || s.HistoricalOnly && action != "disconnect" {
		return s, ErrConflict
	}
	if s.ID == "" {
		return s, ErrUnavailable
	}
	switch action {
	case "pause":
		s.Paused = true
	case "resume":
		if !s.Connected {
			return s, ErrDenied
		}
		s.Paused = false
		s.Pending = true
		s.Actor = actor
		s.Error = ""
		s.NextAttempt = time.Time{}
	case "disconnect":
		s.Connected = false
		s.Paused = true
		s.Pending = false
	default:
		return s, ErrDenied
	}
	s.Generation++
	return s, n.save(s)
}
func (m *Manager) Refresh(ctx context.Context, project string) (State, error) {
	if m.github == nil {
		return State{}, ErrUnavailable
	}
	select {
	case m.work <- struct{}{}:
		defer func() { <-m.work }()
	case <-ctx.Done():
		return State{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(m.github.Config.Limits.Seconds+60)*time.Second)
	defer cancel()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return State{}, ErrUnavailable
	}
	if _, exists := m.running[project]; exists {
		m.mu.Unlock()
		return State{}, ErrBusy
	}
	m.running[project] = cancel
	m.wg.Add(1)
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.running, project); m.mu.Unlock(); m.wg.Done() }()
	n, err := m.book(project)
	if err != nil {
		return State{}, err
	}
	if err := n.lock(); err != nil {
		return State{}, err
	}
	defer n.mu.Unlock()
	s := n.state.Sources[n.state.Active]
	if err := m.syncIndexPolicy(n, &s); err != nil {
		return s, err
	}
	if !s.Connected || s.Paused || s.Handoff != "" || s.HistoricalOnly {
		return s, ErrDenied
	}
	fail := func(e error) (State, error) {
		s.Error = sourceError(e)
		s.Failures++
		delay := time.Minute * time.Duration(1<<min(s.Failures, 8))
		s.NextAttempt = time.Now().Add(delay)
		var rate *rateLimitError
		if errors.As(e, &rate) && rate.RetryAt.After(s.NextAttempt) {
			s.NextAttempt = rate.RetryAt
		}
		if errors.Is(e, ErrDenied) || errors.Is(e, ErrLimit) {
			s.Paused = true
		}
		if saveErr := n.save(s); saveErr != nil {
			return s, saveErr
		}
		return s, e
	}
	if err = m.check(project, s.Actor); err != nil {
		return fail(ErrDenied)
	}
	// Resolving upstream metadata does not mutate the cache. Keep the last
	// verified snapshot readable, then recheck the binding before using the reply.
	n.mu.Unlock()
	repo, sha, token, err := m.github.Resolve(ctx, s.Installation, s.Repository.ID)
	n.mu.Lock()
	if refreshErr := n.refresh(); refreshErr != nil {
		return State{}, refreshErr
	}
	current := n.state.Sources[n.state.Active]
	if current.ID != s.ID || current.Generation != s.Generation {
		return current, ErrConflict
	}
	pending := !slices.Equal(s.Deliveries, current.Deliveries)
	s = current
	if err != nil {
		return fail(err)
	}
	if err = m.check(project, s.Actor); err != nil {
		return fail(ErrDenied)
	}
	c := m.cache(n, s.ID)
	oldRevision := s.Downloaded
	_, cacheErr := os.Stat(filepath.Join(c.dir, "source.git"))
	if cacheErr == nil {
		_, cacheErr = c.git(ctx, filepath.Join(c.dir, "source.git"), "", 128, "cat-file", "-e", sha+"^{tree}")
	}
	if cacheErr != nil {
		if err = c.refresh(ctx, repo, sha, token); err != nil {
			return fail(err)
		}
	}
	_ = n.remember(c, s.ID, sha)
	if err = m.check(project, s.Actor); err != nil {
		return fail(ErrDenied)
	}
	if oldRevision != "" && oldRevision != sha {
		_, e := c.git(ctx, filepath.Join(c.dir, "source.git"), "", 1024, "merge-base", "--is-ancestor", oldRevision, sha)
		if e != nil {
			s.BaselineRequired = true
		}
	}
	if repo.DefaultBranch != s.Repository.DefaultBranch {
		s.BaselineRequired = true
	}
	s.Repository = repo
	s.Downloaded = sha
	s.LastRefresh = time.Now().UTC()
	s.Error = ""
	s.Failures = 0
	s.Pending = pending
	s.NextAttempt = time.Time{}
	s.CacheBytes, _ = diskBytes(filepath.Join(c.dir, "source.git"))
	if c.blobs == nil {
		// This directory holds every retained file. Elsewhere the state's
		// running total counts them.
		s.EvidenceBytes, _ = diskBytes(filepath.Join(c.dir, "evidence"))
	}

	if err = n.save(s); err != nil {
		return s, err
	}
	replaced := s.Index.Generation
	m.indexMu.Lock()
	idx, indexErr := c.buildIndex(ctx, s, func() error { return m.check(project, s.Actor) })
	m.indexMu.Unlock()
	if indexErr != nil {
		s.Index.Error = sourceError(indexErr)
		if errors.Is(indexErr, ErrLimit) {
			s.Index.Error = "Source index limit reached; narrow the included paths and refresh."
		}
	} else {
		s.Index = indexStatus(c, idx, s.Index, s.IndexConfig)
	}
	return s, n.saveIndexed(ctx, c, s, replaced)
}
func sourceError(err error) string {
	var rate *rateLimitError
	if errors.As(err, &rate) {
		return rate.Error()
	}
	switch {
	case errors.Is(err, ErrDenied):
		return "Source access revoked or unsupported file; reconnect or check permissions."
	case errors.Is(err, ErrLimit):
		return "Source storage, file, or execution limit reached. Work is paused; adjust operator limits."
	case errors.Is(err, context.Canceled):
		return "Source operation interrupted; retry is safe."
	default:
		return "Source unavailable. Previous data is preserved; retry or check the GitHub installation."
	}
}
func (m *Manager) read(ctx context.Context, project, id string, fn func(*cache, State) error) error {
	if m.github == nil {
		return ErrUnavailable
	}
	n, err := m.book(project)
	if err != nil {
		return err
	}
	if err := n.lock(); err != nil {
		return err
	}
	defer n.mu.Unlock()
	if id == "" {
		id = n.state.Active
		if n.state.Sources[id].HistoricalOnly {
			return ErrDenied
		}
	}
	s, ok := n.state.Sources[id]
	if !ok || !s.Connected {
		return ErrDenied
	}
	if err = m.check(project, s.Actor); err != nil {
		return ErrDenied
	}
	token, err := m.github.Token(ctx, s.Installation, s.Repository.ID)
	if err != nil {
		return err
	}
	if err = m.check(project, s.Actor); err != nil {
		return ErrDenied
	}
	ctxErr := ctx.Err()
	if ctxErr != nil {
		return ctxErr
	}
	c := m.cache(n, id)
	if c.blobs != nil {
		// This directory is this gateway's alone, and another gateway's
		// refresh advances the source without its mirror.
		c.fetch = func(ctx context.Context) error { return n.mirror(ctx, c, s, token) }
	}
	if err = fn(c, s); err != nil {
		return err
	}
	if err = m.check(project, s.Actor); err != nil {
		return ErrDenied
	}
	return ctx.Err()
}
func (m *Manager) Tree(ctx context.Context, project, id, sha string) ([]Entry, error) {
	var out []Entry
	err := m.read(ctx, project, id, func(c *cache, s State) error {
		if sha == "" {
			sha = s.Downloaded
		}
		if sha == "" {
			return ErrUnavailable
		}
		var e error
		out, e = c.tree(ctx, sha)
		return e
	})
	return out, err
}
func (m *Manager) File(ctx context.Context, project, id, sha, p string) (File, error) {
	var out File
	err := m.read(ctx, project, id, func(c *cache, s State) error {
		if sha == "" {
			sha = s.Downloaded
		}
		var e error
		out, e = c.file(ctx, s.Repository, sha, p)
		out.Project = s.Project
		out.Source = s.ID
		return e
	})
	return out, err
}

// Citation supplies coordinates only. File rechecks source access before
// returning any original bytes to the caller.
func (m *Manager) Citation(project, address string) (source, sha, p string) {
	n, err := m.book(project)
	if err != nil {
		return "", "", ""
	}
	if n.lock() != nil {
		return "", "", ""
	}
	defer n.mu.Unlock()
	s := n.state.Sources[n.state.Active]
	if !s.Connected || m.check(project, s.Actor) != nil {
		return "", "", ""
	}
	ctx, cancel := storageContext()
	defer cancel()
	sha, p = m.cache(n, s.ID).citation(ctx, s.Repository, address)
	if sha == "" {
		return "", "", ""
	}
	return s.ID, sha, p
}
func (m *Manager) Capture(ctx context.Context, project, id, sha string, paths []string) ([]File, error) {
	out := []File{}
	if len(paths) > 100 {
		return nil, ErrLimit
	}
	n, err := m.book(project)
	if err != nil {
		return nil, err
	}
	err = m.read(ctx, project, id, func(c *cache, s State) error {
		var added int64
		for _, p := range paths {
			f, err := c.file(ctx, s.Repository, sha, p)
			if err == nil {
				var stored int64
				stored, err = c.retain(ctx, s.Repository, f, s.EvidenceBytes+added)
				added += stored
			}
			if err != nil {
				return errors.Join(err, n.addEvidence(s.ID, added))
			}
			f.Retained = true
			out = append(out, f)
		}
		return n.addEvidence(s.ID, added)
	})
	return out, err
}

type Changes struct {
	From             string   `json:"from"`
	To               string   `json:"to"`
	Paths            []string `json:"paths"`
	Removed          []string `json:"removed,omitempty"`
	BaselineRequired bool     `json:"baselineRequired"`
}

func (m *Manager) Changes(ctx context.Context, project, id, from, to string) (Changes, error) {
	return m.changes(ctx, project, id, from, to, false)
}

// IndexedChanges limits metadata to files admitted by the current processing
// policy. Deleted paths are checked using their previous tree entry.
func (m *Manager) IndexedChanges(ctx context.Context, project, id, from, to string) (Changes, error) {
	return m.changes(ctx, project, id, from, to, true)
}

func (m *Manager) changes(ctx context.Context, project, id, from, to string, processing bool) (Changes, error) {
	out := Changes{From: from, To: to, Paths: []string{}}
	err := m.read(ctx, project, id, func(c *cache, s State) error {
		if to == "" {
			to = s.Downloaded
			out.To = to
		}
		if !revision.MatchString(to) {
			return ErrDenied
		}
		if processing && s.Downloaded != to {
			return ErrDenied
		}
		if !revision.MatchString(from) {
			out.BaselineRequired = true
			return nil
		}
		mirror, err := c.mirror(ctx)
		if err != nil {
			return err
		}
		if _, err := c.git(ctx, mirror, "", 1024, "merge-base", "--is-ancestor", from, to); err != nil {
			out.BaselineRequired = true
			return nil
		}
		b, err := c.git(ctx, mirror, "", c.limits.Files*1100, "diff-tree", "--no-commit-id", "--name-only", "--no-renames", "-r", "-z", from, to)
		if err != nil {
			return err
		}
		current, err := c.tree(ctx, to)
		if err != nil {
			return err
		}
		previous, err := c.tree(ctx, from)
		if err != nil {
			return err
		}
		visible, present := map[string]bool{}, map[string]bool{}
		for _, entry := range previous {
			visible[entry.Path] = entry.Supported && excluded(entry, s.IndexConfig) == ""
		}
		for _, entry := range current {
			present[entry.Path] = true
			visible[entry.Path] = entry.Supported && excluded(entry, s.IndexConfig) == ""
		}
		for _, p := range strings.Split(string(b), "\x00") {
			if p != "" && (!processing || visible[p]) {
				out.Paths = append(out.Paths, p)
				if !present[p] {
					out.Removed = append(out.Removed, p)
				}
			}
		}
		if len(out.Paths) > c.limits.Files {
			return ErrLimit
		}
		return nil
	})
	return out, err
}

// Wake persists a coalesced signal before acknowledging a signed delivery. The
// authoritative branch is resolved during refresh, never trusted from payloads.
func (m *Manager) Wake(repository, installation int64, delivery string) error {
	if delivery == "" || len(delivery) > 200 {
		return ErrDenied
	}
	projects, err := m.stored()
	if err != nil {
		return err
	}
	for _, project := range projects {
		n, err := m.book(project)
		if err != nil {
			return err
		}
		if err := n.lock(); err != nil {
			return err
		}
		s := n.state.Sources[n.state.Active]
		if s.Connected && (s.Repository.ID == repository || repository == 0) && s.Installation == installation {
			found := false
			for _, d := range s.Deliveries {
				if d == delivery {
					found = true
				}
			}
			if !found {
				s.Deliveries = append(s.Deliveries, delivery)
				if len(s.Deliveries) > 128 {
					s.Deliveries = s.Deliveries[len(s.Deliveries)-128:]
				}
				s.Pending = true
				if err := n.save(s); err != nil {
					n.mu.Unlock()
					return err
				}
			}
		}
		n.mu.Unlock()
	}
	return nil
}

// Projects lists the notebooks with source state, including those another
// gateway connected; while the database is unreachable, those this one knows.
func (m *Manager) Projects() []string {
	if projects, err := m.stored(); err == nil {
		return projects
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.notebooks))
	for p := range m.notebooks {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
func (m *Manager) Start() {
	m.mu.Lock()
	if m.cancel != nil || m.closed {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.wg.Add(1)
	m.mu.Unlock()
	// One gateway refreshes sources and embeds and enriches their indexes, so
	// none pays for another's work.
	go func() {
		defer m.wg.Done()
		m.db.Duty(ctx, "repository-refresh", 2*time.Minute, m.refreshLoop)
	}()
}

func (m *Manager) refreshLoop(ctx context.Context, _ *pg.Lease) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		progressed := false
		for _, p := range m.Projects() {
			s, err := m.Status(p)
			if err == nil && s.Connected && !s.Paused && time.Now().After(s.NextAttempt) && (s.Pending || time.Since(s.LastRefresh) >= 5*time.Minute) {
				_, _ = m.Refresh(ctx, p)
			}
			if err == nil && s.Connected && !s.Paused && s.IndexConfig.Embeddings && s.IndexConfig.ExternalProcessing && s.Index.EmbeddingError == "" {
				more, e := m.EmbedPending(ctx, p)
				if e == nil && more {
					progressed = true
				}
			}
			if err == nil && s.Connected && !s.Paused && s.IndexConfig.MapJudgments && s.IndexConfig.ExternalProcessing {
				more, e := m.EnrichMapPending(ctx, p)
				if e == nil && more {
					progressed = true
				}
			}
		}
		if progressed && ctx.Err() == nil {
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	if m.cancel != nil {
		m.cancel()
	}
	for _, cancel := range m.processingJobs {
		cancel()
	}
	for _, cancel := range m.running {
		cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// Authorize checks source-sharing and upstream installation authority without
// materializing content. Callers separately enforce the reading user's grant.
func (m *Manager) Authorize(ctx context.Context, project, id string) error {
	return m.read(ctx, project, id, func(_ *cache, _ State) error { return nil })
}

// PackageFiles reads only regular, policy-allowed source files at an exact
// revision. Retained source evidence keeps activated packages available after
// the rolling Git cache drops an old commit; it does not activate anything.
func (m *Manager) PackageFiles(ctx context.Context, project, id, sha string, paths []string, retain bool) (map[string]string, error) {
	out := map[string]string{}
	n, err := m.book(project)
	if err != nil {
		return nil, err
	}
	err = m.read(ctx, project, id, func(c *cache, s State) error {
		if s.Paused || !revision.MatchString(sha) {
			return ErrDenied
		}
		var added int64
		for _, p := range paths {
			f, e := c.file(ctx, s.Repository, sha, p)
			if e == nil {
				if reason := excluded(Entry{Path: p, Mode: "100644", BlobID: sha, Bytes: int64(len(f.Content))}, s.IndexConfig); reason != "" {
					e = fmt.Errorf("package file excluded: %s (%s)", p, reason)
				}
			}
			if e == nil && retain {
				var stored int64
				stored, e = c.retain(ctx, s.Repository, f, s.EvidenceBytes+added)
				added += stored
			}
			if e != nil {
				return errors.Join(e, n.addEvidence(s.ID, added))
			}
			out[p] = f.Content
		}
		return n.addEvidence(s.ID, added)
	})
	return out, err
}
