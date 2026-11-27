// Package backups owns consent and receipts. Only the external worker pushes Git.
package backups

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"forgetmenote/internal/githubapp"
	"forgetmenote/internal/outgoing"
	"forgetmenote/internal/pg"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var ErrDenied = errors.New("backup access denied")
var ErrConflict = errors.New("backup configuration changed; reload and retry")
var ErrUnavailable = errors.New("managed backups unavailable")

const (
	maxStateBytes = 4 << 20
	stateTimeout  = 10 * time.Second
	// The shared state is this one document.
	stateKind, stateID = "backups", "bindings"
)

type Binding struct {
	Handoff            string               `json:"handoff,omitempty"`
	Drained            bool                 `json:"drained,omitempty"`
	AuthorityTarget    bool                 `json:"authorityTarget,omitempty"`
	NotebookGeneration uint64               `json:"notebookGeneration,omitempty"`
	Project            string               `json:"project"`
	Actor              string               `json:"actor"`
	Owner              string               `json:"owner"`
	Epoch              uint64               `json:"epoch"`
	Authority          uint64               `json:"authority"`
	Installation       int64                `json:"installation"`
	Repository         githubapp.Repository `json:"repository"`
	Branch             string               `json:"branch"`
	Generation         uint64               `json:"generation"`
	Connected          bool                 `json:"connected"`
	Enabled            bool                 `json:"enabled"`
	Request            uint64               `json:"request"`
	LastRevision       string               `json:"lastRevision,omitempty"`
	LastSuccess        time.Time            `json:"lastSuccess,omitempty"`
	LastAttempt        time.Time            `json:"lastAttempt,omitempty"`
	Error              string               `json:"error,omitempty"`
	Blocked            bool                 `json:"blocked,omitempty"`
	Creation           *Creation            `json:"creation,omitempty"`
}
type Creation struct {
	Owner      string `json:"owner"`
	Name       string `json:"name"`
	Marker     string `json:"marker"`
	Repository int64  `json:"repository,omitempty"`
}
type Setup struct {
	Binding
	UserToken string
}
type Lease struct {
	NotebookGeneration uint64    `json:"notebookGeneration,omitempty"`
	ID                 string    `json:"id"`
	Project            string    `json:"project"`
	Generation         uint64    `json:"generation"`
	Revision           string    `json:"revision"`
	Remote             string    `json:"remote"`
	Branch             string    `json:"branch"`
	Token              string    `json:"token"`
	Expires            time.Time `json:"expires"`
	Owner              string    `json:"-"`
}
type Receipt struct {
	Lease      string `json:"lease"`
	Project    string `json:"project"`
	Generation uint64 `json:"generation"`
	Revision   string `json:"revision"`
	Error      string `json:"error,omitempty"`
	Diverged   bool   `json:"diverged,omitempty"`
}

// sharedState is one instance's bindings and active leases, which every gateway
// shares as one versioned Postgres document.
type sharedState struct {
	Version  int                    `json:"version"`
	Bindings map[string]Binding     `json:"bindings"`
	Leases   map[string]storedLease `json:"leases,omitempty"`
}

// storedLease is a lease as gateways share it. Its installation token is sealed
// under the GitHub App key, so the database never holds it in the clear.
type storedLease struct {
	Lease
	Owner  string `json:"owner"`
	Sealed string `json:"sealedToken"`
}
type Manager struct {
	// Outgoing is installed before use. While it is paused nothing reaches
	// GitHub: copied leases hold the live instance's tokens.
	Outgoing *outgoing.Mode
	mu       sync.Mutex
	doc      *pg.Doc[sharedState]
	db       *pg.DB
	app      *githubapp.Client
	check    func(Binding) error
	state    sharedState
	leases   map[string]Lease
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// Open returns the manager of the backup bindings and leases that an instance's
// gateways share in db, so a worker may lease from one gateway and report to
// another.
func Open(db *pg.DB, app *githubapp.Client, check func(Binding) error) (*Manager, error) {
	if db == nil || app == nil || check == nil {
		return nil, ErrUnavailable
	}
	m := &Manager{doc: pg.NewDoc[sharedState](db, stateKind, stateID, maxStateBytes), db: db, app: app, check: check, state: emptyState(), leases: map[string]Lease{}}
	if err := m.lock(); err != nil {
		return nil, err
	}
	m.mu.Unlock()
	return m, nil
}

// Stored reads the bindings the instance's gateways share, by notebook,
// without a GitHub App and without opening a manager: for an instance that
// runs no managed backups yet still reports the ones people connected. A
// binding's authorization is not checked.
func Stored(ctx context.Context, db *pg.DB) (map[string]Binding, error) {
	ctx, cancel := context.WithTimeout(ctx, stateTimeout)
	defer cancel()
	state, _, err := pg.NewDoc[sharedState](db, stateKind, stateID, maxStateBytes).Load(ctx)
	if err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	return state.Bindings, nil
}

func emptyState() sharedState { return sharedState{Version: 1, Bindings: map[string]Binding{}} }

// lock takes the manager and adopts what another gateway stored since the last
// call. While the state cannot be read, nothing is granted or recorded.
func (m *Manager) lock() error {
	m.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), stateTimeout)
	defer cancel()
	next, changed, err := m.doc.Load(ctx)
	if err == nil && !changed {
		return nil
	}
	var leases map[string]Lease
	if err == nil {
		leases, err = m.adopt(&next)
	}
	if err != nil {
		m.doc.Forget()
		m.state, m.leases = emptyState(), map[string]Lease{}
		m.mu.Unlock()
		return errors.Join(ErrUnavailable, err)
	}
	m.state, m.leases = next, leases
	return nil
}

// adopt checks stored state and opens its leases. A lease whose token cannot be
// opened, such as one sealed before the App key changed, keeps its notebook's
// place without a token until it lapses, so no second lease overlaps it.
func (m *Manager) adopt(s *sharedState) (map[string]Lease, error) {
	if s.Version == 0 && s.Bindings == nil && s.Leases == nil {
		*s = emptyState()
		return map[string]Lease{}, nil
	}
	if err := checkBindings(s.Version, s.Bindings); err != nil {
		return nil, err
	}
	leases := map[string]Lease{}
	for id, stored := range s.Leases {
		if id != stored.ID || stored.Owner == "" {
			return nil, errors.New("damaged managed backup lease")
		}
		l := stored.Lease
		l.Owner = stored.Owner
		l.Token, _ = m.app.OpenToken(stored.Sealed)
		leases[id] = l
	}
	s.Leases = nil
	return leases, nil
}

// checkBindings refuses bindings no gateway may serve, as the file-based
// release refused them when it opened.
func checkBindings(version int, bindings map[string]Binding) error {
	if (version != 1 && version != 2) || bindings == nil {
		return errors.New("unsupported or damaged managed backup state")
	}
	seen := map[string]bool{}
	for p, b := range bindings {
		if p != b.Project || !identifier.MatchString(p) || b.Owner == "" || b.Actor == "" || (b.Connected && (!githubapp.ValidRepo(b.Repository) || !b.Repository.Private || !validBranch(b.Branch) || b.Installation <= 0 || b.Generation == 0)) {
			return errors.New("damaged managed backup binding")
		}
		key := b.Repository.FullName + ":" + b.Branch
		if b.Connected && seen[key] {
			return errors.New("managed backup destination is bound twice")
		}
		if b.Connected {
			seen[key] = true
		}
	}
	return nil
}

// Status reports a notebook's binding as every gateway currently shares it.
func (m *Manager) Status(project string) (Binding, error) {
	if err := m.lock(); err != nil {
		return Binding{Project: project}, err
	}
	defer m.mu.Unlock()
	b := m.state.Bindings[project]
	b.Project = project
	if b.Connected && m.check(b) != nil {
		b.Error = "Backup authorization changed; reconnect as a current notebook Owner"
	}
	if b.Creation != nil {
		copy := *b.Creation
		b.Creation = &copy
	}
	return b, nil
}

func (m *Manager) Start() {
	m.stop = make(chan struct{})
	m.done = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-m.stop
		cancel()
	}()
	// One gateway expires leases, so one revokes each lapsed token.
	go func() {
		defer close(m.done)
		m.db.Duty(ctx, "backup-expiry", time.Minute, func(ctx context.Context, _ *pg.Lease) {
			timer := time.NewTicker(15 * time.Second)
			defer timer.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
					m.expire()
				}
			}
		})
	}()
}

// expire ends leases that lapsed or lost their authority, whichever gateway
// stored them.
func (m *Manager) expire() {
	if m.lock() != nil {
		return
	}
	defer m.mu.Unlock()
	now := time.Now()
	leases, tokens := m.without(func(l Lease) bool {
		return now.After(l.Expires) || !m.allowed(m.state.Bindings[l.Project], l.Owner, l.Generation)
	})
	// A lease another gateway changed first is expired on the next pass.
	if len(tokens) > 0 && m.save(leases) == nil {
		m.revoke(context.Background(), tokens...)
	}
}

// Close stops this gateway's expiry loop. Leases are shared, so they stay valid
// for a worker reporting to another gateway, or to this one after a restart,
// and lapse within three minutes.
func (m *Manager) Close() {
	if m.stop != nil {
		m.stopOnce.Do(func() { close(m.stop) })
		<-m.done
	}
}
func (m *Manager) Configure(ctx context.Context, in Setup) (Binding, error) {
	if err := m.Outgoing.Check(); err != nil {
		return Binding{}, err
	}
	if err := m.lock(); err != nil {
		return Binding{}, err
	}
	defer m.mu.Unlock()
	old := m.state.Bindings[in.Project]
	if old.Generation != in.Generation || old.Handoff != "" || old.AuthorityTarget {
		return Binding{}, ErrConflict
	}
	if !identifier.MatchString(in.Project) || !validBranch(in.Branch) || in.Owner == "" || in.Actor == "" {
		return Binding{}, ErrDenied
	}
	if err := m.check(in.Binding); err != nil {
		return Binding{}, err
	}
	for p, b := range m.state.Bindings {
		if p != in.Project && (b.Connected || b.AuthorityTarget) && b.Repository.ID == in.Repository.ID && b.Branch == in.Branch {
			return Binding{}, errors.New("another notebook already uses this backup destination")
		}
	}
	repo, err := m.app.BackupDestination(ctx, in.UserToken, in.Installation, in.Repository.ID, old.Repository.ID == in.Repository.ID && old.Branch == in.Branch && old.Generation > 0)
	if err != nil {
		return Binding{}, err
	}
	if err = m.check(in.Binding); err != nil {
		return Binding{}, err
	}
	b := in.Binding
	b.Repository = repo
	b.Connected = true
	b.Enabled = true
	b.Generation++
	b.Request = old.Request + 1
	b.Creation = old.Creation
	if old.Repository.ID == repo.ID && old.Branch == b.Branch {
		b.LastRevision = old.LastRevision
		b.LastSuccess = old.LastSuccess
	}
	leases, tokens := m.without(func(l Lease) bool { return l.Project == b.Project })
	if err = m.save(leases, b); err != nil {
		return Binding{}, err
	}
	m.revoke(ctx, tokens...)
	return b, nil
}

var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
var revision = regexp.MustCompile(`^[a-f0-9]{40}$`)
var branch = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,200}$`)
var repoDestination = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func validBranch(v string) bool {
	if !branch.MatchString(v) || strings.Contains(v, "..") || strings.Contains(v, "//") || strings.HasSuffix(v, "/") || strings.HasSuffix(v, ".") {
		return false
	}
	for _, p := range strings.Split(v, "/") {
		if strings.HasPrefix(p, ".") || strings.HasSuffix(p, ".lock") {
			return false
		}
	}
	return true
}

// save stores the changed bindings with leases as the shared state, only over
// the version this gateway last read, and then adopts it. After a failed save
// nothing here changed, and the next call rereads what the database holds.
func (m *Manager) save(leases map[string]Lease, changed ...Binding) error {
	next := sharedState{Version: 1, Bindings: map[string]Binding{}, Leases: map[string]storedLease{}}
	for p, v := range m.state.Bindings {
		next.Bindings[p] = v
	}
	for _, b := range changed {
		next.Bindings[b.Project] = b
	}
	for _, binding := range next.Bindings {
		if binding.Handoff != "" || binding.AuthorityTarget || binding.NotebookGeneration > 0 || m.state.Version >= 2 {
			next.Version = 2
		}
	}
	for id, l := range leases {
		sealed, err := m.app.SealToken(l.Token)
		if err != nil {
			return err
		}
		stored := storedLease{Lease: l, Owner: l.Owner, Sealed: sealed}
		stored.Token = ""
		next.Leases[id] = stored
	}
	ctx, cancel := context.WithTimeout(context.Background(), stateTimeout)
	defer cancel()
	switch err := m.doc.Save(ctx, next); {
	case errors.Is(err, pg.ErrConflict):
		return errors.Join(ErrConflict, err)
	case errors.Is(err, pg.ErrTooLarge):
		return errors.New("managed backup configuration exceeds the state size limit")
	case err != nil && !errors.Is(err, pg.ErrNUL):
		return errors.Join(ErrUnavailable, err)
	case err != nil:
		return err
	}
	next.Leases = nil
	m.state, m.leases = next, leases
	return nil
}

// without returns the leases drop does not select and the tokens of those it
// does.
func (m *Manager) without(drop func(Lease) bool) (map[string]Lease, []string) {
	kept, tokens := map[string]Lease{}, []string{}
	for id, l := range m.leases {
		if drop(l) {
			tokens = append(tokens, l.Token)
		} else {
			kept[id] = l
		}
	}
	return kept, tokens
}

// drop ends a lease that can no longer be used, recording any changed binding.
// Its token is revoked even when the record fails, as nothing needs it again.
func (m *Manager) drop(ctx context.Context, id string, changed ...Binding) {
	leases, tokens := m.without(func(l Lease) bool { return l.ID == id })
	_ = m.save(leases, changed...)
	m.revoke(ctx, tokens...)
}
func (m *Manager) List(owner string) ([]Binding, error) {
	if err := m.lock(); err != nil {
		return nil, err
	}
	defer m.mu.Unlock()
	out := []Binding{}
	for _, b := range m.state.Bindings {
		if b.Owner == owner && b.Handoff != "" && !b.Drained {
			out = append(out, b)
			continue
		}
		if b.Owner == owner && b.Connected && b.Enabled && !b.Blocked && m.check(b) == nil {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Project < out[j].Project })
	return out, nil
}
func (m *Manager) Control(ctx context.Context, project string, generation uint64, action string) (Binding, error) {
	if err := m.Outgoing.Check(); err != nil {
		return Binding{}, err
	}
	if err := m.lock(); err != nil {
		return Binding{}, err
	}
	defer m.mu.Unlock()
	b := m.state.Bindings[project]
	if !b.Connected || b.Generation != generation || b.Handoff != "" || b.AuthorityTarget {
		return b, ErrConflict
	}
	switch action {
	case "pause":
		b.Enabled = false
	case "disconnect":
		b.Enabled = false
		b.Connected = false
	case "resume":
		if err := m.check(b); err != nil {
			return b, err
		}
		b.Enabled = true
		b.Blocked = false
		b.Error = ""
	case "now":
		if err := m.check(b); err != nil {
			return b, err
		}
		if !b.Enabled || b.Blocked {
			return b, errors.New("resume backups before requesting an upload")
		}
		b.Request++
		return b, m.save(m.leases, b)
	default:
		return b, ErrDenied
	}
	b.Generation++
	leases, tokens := m.without(func(l Lease) bool { return l.Project == project })
	err := m.save(leases, b)
	if err == nil {
		m.revoke(ctx, tokens...)
	}
	return b, err
}

// Lease grants one notebook's worker a token for one upload. Gateways grant
// leases over the shared state, so at most one lease per notebook is stored:
// a gateway that loses the race to store its lease revokes its token.
func (m *Manager) Lease(ctx context.Context, owner, project string, generation uint64, sha string) (Lease, error) {
	if err := m.Outgoing.Check(); err != nil {
		return Lease{}, err
	}
	if err := m.lock(); err != nil {
		return Lease{}, err
	}
	defer m.mu.Unlock()
	b := m.state.Bindings[project]
	if !revision.MatchString(sha) || !m.allowed(b, owner, generation) {
		return Lease{}, ErrDenied
	}
	now := time.Now()
	leases, expired := m.without(func(l Lease) bool { return l.Expires.Before(now) })
	for _, l := range leases {
		if l.Project == project {
			return Lease{}, ErrConflict
		}
	}
	token, err := m.app.BackupToken(ctx, b.Installation, b.Repository)
	if err != nil {
		b.Error = "GitHub access unavailable; check installation access and repository privacy"
		_ = m.save(m.leases, b)
		return Lease{}, err
	}
	if !m.allowed(b, owner, generation) {
		m.revoke(ctx, token)
		return Lease{}, ErrDenied
	}
	var key [32]byte
	rand.Read(key[:])
	l := Lease{NotebookGeneration: b.NotebookGeneration, ID: hex.EncodeToString(key[:]), Project: project, Owner: owner, Generation: generation, Revision: sha, Remote: "https://github.com/" + b.Repository.FullName + ".git", Branch: b.Branch, Token: token, Expires: time.Now().Add(3 * time.Minute)}
	b.LastAttempt = time.Now().UTC()
	leases[l.ID] = l
	if err = m.save(leases, b); err != nil {
		m.revoke(ctx, token)
		return Lease{}, err
	}
	m.revoke(ctx, expired...)
	return l, nil
}
func (m *Manager) allowed(b Binding, owner string, generation uint64) bool {
	return b.Owner == owner && b.Connected && b.Enabled && !b.Blocked && b.Generation == generation && m.check(b) == nil
}
func (m *Manager) revoke(ctx context.Context, tokens ...string) {
	// A paused copy's tokens are the live instance's; they expire on their own.
	if m.Outgoing.Paused() {
		return
	}
	for _, token := range tokens {
		if token == "" {
			continue
		}
		revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = m.app.Revoke(revokeCtx, token)
		cancel()
	}
}
func (m *Manager) checked(ctx context.Context, owner, id string) (Lease, Binding, error) {
	if err := m.Outgoing.Check(); err != nil {
		return Lease{}, Binding{}, err
	}
	l, ok := m.leases[id]
	if !ok || l.Owner != owner {
		return Lease{}, Binding{}, ErrDenied
	}
	b := m.state.Bindings[l.Project]
	if l.Expires.Before(time.Now()) || !m.allowed(b, owner, l.Generation) {
		m.drop(ctx, id)
		return Lease{}, b, ErrDenied
	}
	if err := m.app.ValidateBackupToken(ctx, l.Token, b.Installation, b.Repository); err != nil {
		b.Error = "Backup stopped: GitHub access or repository privacy changed"
		m.drop(ctx, id, b)
		return Lease{}, b, err
	}
	// The catalog can revoke authority while GitHub validation is in flight.
	if l.Expires.Before(time.Now()) || !m.allowed(b, owner, l.Generation) {
		m.drop(ctx, id)
		return Lease{}, b, ErrDenied
	}
	return l, b, nil
}
func (m *Manager) CheckLease(ctx context.Context, owner, id string) error {
	if err := m.lock(); err != nil {
		return err
	}
	defer m.mu.Unlock()
	_, _, err := m.checked(ctx, owner, id)
	return err
}
func (m *Manager) Complete(ctx context.Context, owner string, in Receipt) (Binding, error) {
	if err := m.lock(); err != nil {
		return Binding{}, err
	}
	defer m.mu.Unlock()
	l, b, err := m.checked(ctx, owner, in.Lease)
	if err != nil {
		return Binding{}, err
	}
	if in.Project != l.Project || in.Generation != l.Generation || in.Revision != l.Revision {
		return Binding{}, ErrDenied
	}
	if in.Error == "" && !in.Diverged {
		remote, err := m.app.RemoteRevision(ctx, l.Token, b.Repository, b.Branch)
		if err != nil {
			return b, err
		}
		if remote != l.Revision {
			return b, errors.New("remote revision does not match the leased commit")
		}
		// Authority can change during the remote read.
		if !m.allowed(b, owner, l.Generation) {
			return b, ErrDenied
		}
		b.LastRevision = l.Revision
		b.LastSuccess = time.Now().UTC()
		b.Error = ""
	} else if in.Diverged {
		b.Blocked = true
		b.Error = "Backup branch diverged. Resolve the destination, then explicitly resume backups."
	} else {
		b.Error = "Backup upload failed; the worker will retry"
	}
	leases, tokens := m.without(func(other Lease) bool { return other.ID == l.ID })
	if err = m.save(leases, b); err != nil {
		return b, err
	}
	m.revoke(ctx, tokens...)
	return b, nil
}

// Create records an intent before the external request. Retries reconcile that
// exact private repository; a failed or disconnected setup never deletes it.
func (m *Manager) Create(ctx context.Context, in Setup, owner, name string) (githubapp.Repository, error) {
	if err := m.Outgoing.Check(); err != nil {
		return githubapp.Repository{}, err
	}
	if err := m.lock(); err != nil {
		return githubapp.Repository{}, err
	}
	defer m.mu.Unlock()
	b := m.state.Bindings[in.Project]
	if !identifier.MatchString(in.Project) || len(owner) > 100 || len(name) > 100 || !repoDestination.MatchString(owner+"/"+name) || strings.Contains(owner+"/"+name, "..") || b.Generation != in.Generation || m.check(in.Binding) != nil || b.Connected {
		return githubapp.Repository{}, ErrDenied
	}
	if b.Creation == nil || b.Creation.Owner != owner || b.Creation.Name != name {
		var key [32]byte
		rand.Read(key[:])
		b = in.Binding
		b.Creation = &Creation{Owner: owner, Name: name, Marker: "ForgetMeNote backup " + hex.EncodeToString(key[:])}
		if err := m.save(m.leases, b); err != nil {
			return githubapp.Repository{}, err
		}
	}
	repo, err := m.app.CreatePrivate(ctx, in.UserToken, owner, name, b.Creation.Marker)
	if err != nil {
		return repo, err
	}
	creation := *b.Creation
	creation.Repository = repo.ID
	b.Creation = &creation
	return repo, m.save(m.leases, b)
}
