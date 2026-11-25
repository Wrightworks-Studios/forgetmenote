// Package quicknotes owns personal capture workflow references. Note content stays
// in ordinary protected catalog notebooks and their existing Markdown/Git owners.
package quicknotes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/outgoing"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"
)

var ErrInput = workspace.ErrCaptureInput

type Destination struct {
	Project string `json:"project"`
	Folder  string `json:"folder"`
	Name    string `json:"name,omitempty"`
	Shared  bool   `json:"shared,omitempty"`
}
type Settings struct {
	Mode                 string        `json:"mode"`
	CloudConsent         bool          `json:"cloudConsent"`
	NotifyDestination    bool          `json:"notifyDestination"`
	ApprovedDestinations []Destination `json:"approvedDestinations"`
	Generation           uint64        `json:"generation"`
}
type Suggestion struct {
	Destination Destination `json:"destination"`
	Version     string      `json:"version"`
	Hash        string      `json:"hash"`
	Generation  uint64      `json:"generation"`
}
type Capture struct {
	ID           string      `json:"id"`
	Project      string      `json:"project"`
	Path         string      `json:"path"`
	Version      string      `json:"version"`
	Hash         string      `json:"hash"`
	Status       string      `json:"status"`
	CreatedAt    string      `json:"createdAt"`
	InboxProject string      `json:"inboxProject"`
	InboxPath    string      `json:"inboxPath"`
	Suggestion   *Suggestion `json:"suggestion,omitempty"`
	CanUndo      bool        `json:"canUndo"`
	Title        string      `json:"title,omitempty"`
	Excerpt      string      `json:"excerpt,omitempty"`
	Error        string      `json:"error,omitempty"`
}
type transfer struct {
	Intent             workspace.CaptureMove `json:"intent"`
	Undo               bool                  `json:"undo"`
	Automatic          bool                  `json:"automatic"`
	SettingsGeneration uint64                `json:"settingsGeneration"`
}
type record struct {
	RetryAfter        int64     `json:"retryAfter,omitempty"`
	RetryFailures     uint8     `json:"retryFailures,omitempty"`
	AwaitingClient    bool      `json:"awaitingClient,omitempty"`
	RoutingHoldHash   string    `json:"routingHoldHash,omitempty"`
	RoutingVersion    string    `json:"routingVersion,omitempty"`
	RoutingGeneration uint64    `json:"routingGeneration,omitempty"`
	RoutingOperation  string    `json:"routingOperation,omitempty"`
	RoutingState      string    `json:"routingState,omitempty"`
	Schema            int       `json:"schema"`
	Actor             string    `json:"actor"`
	Subject           string    `json:"subject"`
	OperationID       string    `json:"operationId"`
	SaveHash          string    `json:"saveHash"`
	FiledVersion      string    `json:"filedVersion,omitempty"`
	Pending           *transfer `json:"pending,omitempty"`
	// Holder is the gateway that last began a save, transfer or routing check,
	// which no other gateway recovers while it is present.
	Holder string `json:"holder,omitempty"`
	Capture
	version int64 // the stored version read, or 0 for a new capture
}
type Service struct {
	// Outgoing is installed before Start. While paused, the routing duty sends
	// no capture to Jev; recovery and explicit sorting continue.
	Outgoing        *outgoing.Mode
	lifecycleMu     sync.Mutex
	started, closed bool
	activeRouting   sync.Map
	judgeMu         sync.Mutex
	judge           *decision.Service
	localOnly       bool
	ctx             context.Context
	cancel          context.CancelFunc
	wake            chan struct{}
	done            chan struct{}
	closeOnce       sync.Once
	db              *pg.DB
	catalog         *catalog.Catalog
	locks           [256]sync.Mutex
	scanMu          sync.Mutex
	scanAfter       string

	// Recent is how many recent captures List returns; zero means 100. Tests
	// lower it to reach the cap with a few captures.
	Recent int
}

// Open returns the capture workflow over the records an instance's gateways
// share in db.
func Open(db *pg.DB, c *catalog.Catalog) (*Service, error) {
	if c == nil || db == nil {
		return nil, errors.New("Quicknotes needs the catalog and its database")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{db: db, catalog: c, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{})}, nil
}

// Start begins background routing only after the application's recovery boundary.
func (s *Service) Start() error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closed {
		return errors.New("Quicknotes is closed")
	}
	if !s.started {
		s.started = true
		go s.run()
	}
	return nil
}
func (s *Service) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.lifecycleMu.Lock()
		s.closed = true
		s.cancel()
		if !s.started {
			close(s.done)
		}
		s.lifecycleMu.Unlock()
		<-s.done
	})
}
func (s *Service) objectLock(key string) *sync.Mutex {
	// Fixed stripes retain neither user-supplied keys nor a mutex per missing ID.
	// No caller holds two object locks; collisions only serialize unrelated work.
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return &s.locks[h.Sum64()%uint64(len(s.locks))]
}
func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
func (r *record) clearRetry() {
	r.RetryAfter = 0
	r.RetryFailures = 0
	r.AwaitingClient = false
}
func (s *Service) actor(ctx context.Context, subject string) (context.Context, catalog.Account, error) {
	account, err := s.catalog.Account(subject)
	if err != nil {
		return ctx, account, err
	}
	prior := attribution.From(ctx)
	if prior.ID != "" && prior.ID != account.ID {
		return ctx, account, workspace.ErrForbidden
	}
	identity := account.Identity()
	identity.Source = prior.Source
	identity.Client = prior.Client
	if identity.Source == "" {
		identity.Source = "quicknotes"
	}
	return attribution.With(ctx, identity), account, nil
}
func defaultSettings() Settings {
	return Settings{Mode: "suggest", NotifyDestination: true, ApprovedDestinations: []Destination{}, Generation: 1}
}
func (s *Service) Settings(subject string) (Settings, error) {
	_, account, err := s.actor(context.Background(), subject)
	if err != nil {
		return Settings{}, err
	}
	lock := s.objectLock(account.ID + "/settings")
	lock.Lock()
	defer lock.Unlock()
	return s.settings(account.ID)
}
func sameDestination(a, b Destination) bool { return a.Project == b.Project && a.Folder == b.Folder }
func (s *Service) UpdateSettings(subject string, next Settings) (Settings, error) {
	ctx, account, err := s.actor(context.Background(), subject)
	if err != nil {
		return Settings{}, err
	}
	lock := s.objectLock(account.ID + "/settings")
	lock.Lock()
	defer lock.Unlock()
	old, err := s.settings(account.ID)
	if err != nil {
		return Settings{}, err
	}
	if old.Generation != next.Generation {
		return Settings{}, workspace.ErrConflict
	}
	if (next.Mode != "manual" && next.Mode != "suggest" && next.Mode != "automatic") || len(next.ApprovedDestinations) > 128 {
		return Settings{}, fmt.Errorf("%w: choose Manual, Suggest or Automatic", ErrInput)
	}
	available, err := s.Destinations(ctx, subject)
	if err != nil {
		return Settings{}, err
	}
	approved := []Destination{}
	seen := map[string]bool{}
	for _, wanted := range next.ApprovedDestinations {
		found := false
		for _, d := range available {
			if sameDestination(wanted, d) && wanted.Shared == d.Shared {
				key := d.Project + "\x00" + d.Folder
				if seen[key] {
					return Settings{}, fmt.Errorf("%w: duplicate approved destination", ErrInput)
				}
				seen[key] = true
				approved = append(approved, d)
				found = true
				break
			}
		}
		if !found {
			return Settings{}, fmt.Errorf("%w: approve a currently available destination and its sharing status", ErrInput)
		}
	}
	next.ApprovedDestinations = approved
	next.Generation++
	if err = s.writeSettings(account.ID, old.Generation, next); err != nil {
		return Settings{}, err
	}
	s.signal()
	return next, nil
}
func (s *Service) Destinations(ctx context.Context, subject string) ([]Destination, error) {
	ctx, account, err := s.actor(ctx, subject)
	if err != nil {
		return nil, err
	}
	_, projects, err := s.catalog.Administration(subject)
	if errors.Is(err, workspace.ErrForbidden) {
		return []Destination{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := []Destination{}
	for _, p := range projects {
		// Admin authority over another employee's assistant notebook is not a
		// reason to file captures there, and project memory holds agent items.
		if p.ProtectedInbox != "" || p.AssistantOwner != "" && p.ID != "assistant-"+account.ID {
			continue
		}
		store, e := s.catalog.Access(subject, p.ID, "owner")
		if e != nil {
			continue
		}
		entries, e := store.ListCollection(ctx, "notes")
		if e != nil {
			continue
		}
		folders := map[string]bool{"": true}
		for _, entry := range entries {
			if entry.Path == "AGENTS.md" || workspace.IsTrash(entry.Path) || !strings.HasSuffix(entry.Path, ".md") || !entry.Editable || entry.Metadata != nil && entry.Metadata.ContainerError != "" {
				continue
			}
			parent := filepath.ToSlash(filepath.Dir(entry.Path))
			for parent != "." && parent != "" {
				folders[parent] = true
				parent = filepath.ToSlash(filepath.Dir(parent))
			}
		}
		for folder := range folders {
			state, e := store.Locks(ctx, folder)
			if e != nil || folder != "" && len(state.Rules) > 0 {
				continue
			}
			result = append(result, Destination{Project: p.ID, Folder: folder, Name: p.Name, Shared: sharedWith(p, subject)})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Project == b.Project {
			return a.Folder < b.Folder
		}
		return a.Project < b.Project
	})
	return result, nil
}

// sharedWith reports whether anyone besides subject can open the notebook: a
// direct member, a member through a group grant, or a group grant that has no
// members yet but admits anyone later added to the group. Admin authority,
// including the bootstrap identity's membership of notebooks it created, is
// not sharing.
func sharedWith(p catalog.Project, subject string) bool {
	if len(p.GroupRoles) > 0 {
		return true
	}
	for id := range p.EffectiveMembers {
		if id != subject && id != catalog.Bootstrap {
			return true
		}
	}
	for id := range p.Members {
		if id != subject && id != catalog.Bootstrap {
			return true
		}
	}
	return false
}
func summarize(doc workspace.Document) (string, string) {
	body := string(workspace.MarkdownBody(doc.Content))
	title, _ := workspace.NoteSummary(doc.Content)
	metadata, err := workspace.ParseMetadata(doc.Content)
	if err == nil && metadata.Title != "" {
		title = metadata.Title
	}
	shorten := func(v string, n int) string {
		r := []rune(strings.Join(strings.Fields(v), " "))
		if len(r) > n {
			r = r[:n]
		}
		return string(r)
	}
	if title == "" {
		title = shorten(body, 80)
	}
	if title == "" {
		title = "Quicknote"
	}
	return shorten(title, 160), workspace.SearchExcerpt(doc.Content, "")
}
func (s *Service) verifyInbox(subject string, r record) error {
	p, err := s.catalog.Inbox(subject)
	if err != nil {
		return err
	}
	if p.ID != r.InboxProject {
		return workspace.ErrForbidden
	}
	return nil
}

// destinationPreview is request-local: List shares one lazy inventory across
// its captures, while every subsequent request rechecks the current policy.
type destinationPreview struct {
	current map[Destination]bool
}

func (p *destinationPreview) permits(ctx context.Context, s *Service, subject string, wanted Destination) bool {
	if p.current == nil {
		p.current = map[Destination]bool{}
		available, err := s.Destinations(ctx, subject)
		if err != nil {
			return false
		}
		for _, d := range available {
			p.current[d] = true
		}
	}
	// Match every displayed field, including the current name and sharing flag.
	return p.current[wanted]
}
func (s *Service) view(ctx context.Context, subject string, r record) (Capture, error) {
	return s.viewWithDestinations(ctx, subject, r, &destinationPreview{})
}
func (s *Service) viewWithDestinations(ctx context.Context, subject string, r record, destinations *destinationPreview) (Capture, error) {
	if err := s.verifyInbox(subject, r); err != nil {
		return Capture{}, err
	}
	store, err := s.catalog.Access(subject, r.Project, "read")
	if err != nil {
		return Capture{}, err
	}
	doc, err := store.Read(ctx, r.Path, "")
	if r.Pending != nil && (errors.Is(err, os.ErrNotExist) || err == nil && doc.Version != r.Pending.Intent.Version) {
		doc, err = store.Read(ctx, r.Pending.Intent.From, r.Pending.Intent.Version)
	}
	if err != nil {
		return Capture{}, err
	}
	out := r.Capture
	out.Title, out.Excerpt = summarize(doc)
	out.Version = doc.Version
	out.Hash = workspace.ContentHash(doc.Content)
	if out.Suggestion != nil {
		settings, e := s.settings(r.Actor)
		if e != nil || out.Suggestion.Version != doc.Version || out.Suggestion.Hash != out.Hash || out.Suggestion.Generation != settings.Generation || !destinations.permits(ctx, s, subject, out.Suggestion.Destination) {
			out.Suggestion = nil
			if out.Status == "suggested" {
				out.Status = "inbox"
			}
		}
	}
	if r.Status == "filed" && r.Pending == nil && doc.Version == r.FiledVersion {
		_, e1 := s.catalog.Access(subject, r.Project, "owner")
		_, e2 := s.catalog.Access(subject, r.InboxProject, "owner")
		out.CanUndo = e1 == nil && e2 == nil
	}
	return out, nil
}
func (s *Service) Capture(ctx context.Context, subject, operationID, content string) (Capture, error) {
	if operationID == "" || len(operationID) > 128 || strings.ContainsAny(operationID, "\x00\r\n") || len(content) > workspace.MaxLiveBytes || !utf8.ValidString(content) || strings.ContainsRune(content, 0) {
		return Capture{}, fmt.Errorf("%w: use an operation ID and up to 1 MiB of UTF-8 Markdown", ErrInput)
	}
	ctx, account, err := s.actor(ctx, subject)
	if err != nil {
		return Capture{}, err
	}
	inbox, err := s.catalog.EnsureInbox(subject)
	if err != nil {
		return Capture{}, err
	}
	id := workspace.ContentHash([]byte(account.ID + "\x00" + operationID))[:32]
	lock := s.objectLock(account.ID + "/" + id)
	lock.Lock()
	defer lock.Unlock()
	r, err := s.readRecord(account.ID, id)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Capture{}, err
	}
	hash := workspace.ContentHash([]byte(content))
	if err == nil {
		if r.OperationID != operationID || r.SaveHash != hash {
			return Capture{}, workspace.ErrConflict
		}
		if r.Status != "saving" {
			return s.view(ctx, subject, r)
		}
	} else {
		count, e := s.captureCount(account.ID)
		if e != nil {
			return Capture{}, e
		}
		if count >= captureLimit {
			return Capture{}, errors.New("capture state capacity reached")
		}
		path := "quicknote-" + id + ".md"
		r = record{Schema: 1, Actor: account.ID, Subject: subject, OperationID: operationID, SaveHash: hash, Holder: s.db.Holder, Capture: Capture{ID: id, Project: inbox.ID, Path: path, Hash: hash, Status: "saving", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), InboxProject: inbox.ID, InboxPath: path}}
		if err = s.writeRecord(&r); err != nil {
			return Capture{}, err
		}
	}
	store, err := s.catalog.Access(subject, inbox.ID, "owner")
	if err != nil {
		return Capture{}, err
	}
	doc, err := store.CaptureCreate(ctx, id, r.InboxPath, []byte(content))
	if err != nil {
		s.signal()
		return Capture{}, err
	}
	r.Version = doc.Version
	r.Status = "inbox"
	r.Error = ""
	r.clearRetry()
	if err = s.writeRecord(&r); err != nil {
		return Capture{}, err
	}
	out := r.Capture
	out.Title, out.Excerpt = summarize(doc)
	s.signal()
	return out, nil
}

// InboxCount uses the saved notes, not the capped history of captures and filings.
func (s *Service) InboxCount(ctx context.Context, subject string) (int, error) {
	ctx, account, err := s.actor(ctx, subject)
	if err != nil {
		return 0, err
	}
	for _, project := range s.catalog.List(subject) {
		if project.ProtectedInbox != account.ID {
			continue
		}
		store, err := s.catalog.Access(subject, project.ID, "read")
		if err != nil {
			return 0, err
		}
		counts, err := store.Collections(ctx)
		return counts.Notes + counts.Drafts + counts.Archives, err
	}
	return 0, nil
}

func (s *Service) List(ctx context.Context, subject string) ([]Capture, error) {
	ctx, account, err := s.actor(ctx, subject)
	if err != nil {
		return nil, err
	}
	limit := s.Recent
	if limit <= 0 {
		limit = 100
	}
	records, err := s.recentRecords(account.ID, limit)
	if err != nil {
		return nil, err
	}
	result := []Capture{}
	destinations := &destinationPreview{}
	for _, r := range records {
		if r.Status == "saving" {
			if r.AwaitingClient {
				continue
			}
			recovered, e := s.Recover(ctx, subject, r.ID)
			if e == nil {
				result = append(result, recovered)
			}
			continue
		}
		value, e := s.viewWithDestinations(ctx, subject, r, destinations)
		if errors.Is(e, workspace.ErrForbidden) || errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return nil, e
		}
		result = append(result, value)
	}
	return result, nil
}
func newOperation() (string, error) {
	var raw [16]byte
	_, err := rand.Read(raw[:])
	return hex.EncodeToString(raw[:]), err
}
func (s *Service) destination(ctx context.Context, subject string, wanted Destination) (Destination, error) {
	values, err := s.Destinations(ctx, subject)
	if err != nil {
		return Destination{}, err
	}
	for _, d := range values {
		if sameDestination(d, wanted) {
			return d, nil
		}
	}
	return Destination{}, fmt.Errorf("destination is unavailable: %w", workspace.ErrForbidden)
}
