// Package notestore keeps the notebooks of one org as Git's data model in
// Postgres and the blob store. A revision is the exact Git commit object, with
// its trees and the paths it changed; file contents are kept once by Git blob
// ID, inline when small and otherwise as blocks in the blob store. Commit and
// tree IDs are computed in this process with gitobj when a write happens, so
// the Git export reproduces them byte for byte.
//
// Every write is one transaction that first locks the notebook's head row,
// which makes each notebook single-writer across every gateway. Reads are
// answered from a per-notebook read model in memory, kept current by local
// writes, by NOTIFY on the notebook_revisions channel from other gateways and
// by a fallback poll.
package notestore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sync"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/pg"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Options bound the store's memory. Zero values take the defaults.
type Options struct {
	// BlockCacheBytes bounds the cache of content blocks, file manifests and
	// historical trees. Default 64 MiB.
	BlockCacheBytes int64
	// ReadModelBytes bounds the read models of every notebook together; the
	// least recently used are dropped and rebuilt on their next use. Default
	// 64 MiB.
	ReadModelBytes int64
	// ImportEntryID returns the identity an imported file's content declares,
	// such as a note's fmn-id, or "". Without one, an imported file keeps the
	// entry of the same path or of an identical file it was renamed from.
	ImportEntryID func(path string, content []byte) string
}

// A Head is a notebook's newest revision.
type Head struct {
	Seq    int64
	Commit gitobj.ID
}

// A File is one file of a notebook at a revision. Version is the last commit
// that changed its path.
type File struct {
	Path    string
	EntryID string
	Blob    gitobj.ID
	Mode    uint32
	Size    int64
	Version gitobj.ID
}

// A Change is one entry a revision added, modified, deleted or renamed
// against its first parent. Blob, Mode and Size describe the file after the
// change; a delete has the file's last path and no blob.
type Change struct {
	Op      string
	Path    string
	OldPath string
	EntryID string
	Blob    gitobj.ID
	Mode    uint32
	Size    int64
}

// Change operations.
const (
	OpAdd    = "add"
	OpModify = "modify"
	OpDelete = "delete"
	OpRename = "rename"
)

// A CommitInfo is a revision: its commit, trailers and changes.
type CommitInfo struct {
	Seq      int64
	ID       gitobj.ID
	Commit   gitobj.Commit
	Trailers []gitobj.Trailer
	Changes  []Change
}

var (
	// ErrConflict is a failed expectation of a change set or live update; the
	// error wrapping it says which.
	ErrConflict = errors.New("revision conflict")
	// ErrOperationMismatch is an operation ID repeated for a different request.
	ErrOperationMismatch = errors.New("operation ID was already used for a different request")
	// ErrInvalid is a change set, path or identity the store refuses.
	ErrInvalid = errors.New("invalid notebook change")
)

// A ConflictError describes a failed expectation. It matches ErrConflict.
type ConflictError struct {
	Path     string // "" for the notebook head
	Expected gitobj.ID
	Actual   gitobj.ID
	Reason   string
}

func (e *ConflictError) Error() string {
	subject := "notebook head"
	if e.Path != "" {
		subject = e.Path
	}
	if e.Reason != "" {
		return fmt.Sprintf("%v: %s: %s", ErrConflict, subject, e.Reason)
	}
	return fmt.Sprintf("%v: %s is at %s, not %s", ErrConflict, subject, e.Actual, e.Expected)
}

func (e *ConflictError) Unwrap() error { return ErrConflict }

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// The committer of every revision FMN writes.
const (
	CommitterName  = "ForgetMeNote"
	CommitterEmail = "forgetmenote@localhost"
)

// revisionChannel is the channel other gateways hear new heads on; its
// subject is the notebook ID, as the catalog already announces.
const revisionChannel = "notebook_revisions"

var (
	pollInterval  = 5 * time.Second
	receiptSweeps = time.Hour
)

var notebookID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// A Store holds every notebook of db's org.
type Store struct {
	db    *pg.DB
	blobs blob.Store
	opts  Options

	blocks    *lru[[32]byte, []byte]
	manifests *lru[gitobj.ID, *manifest]
	trees     *lru[gitobj.ID, []gitobj.TreeEntry]

	mu        sync.Mutex
	notebooks map[string]*Notebook
	models    modelList
	watchers  []func(string, Head)
	live      []func(string, LiveChange) // WatchLive's watchers
	pending   map[string]Head
	delivered map[string]Head
	wake      chan struct{}
	sweepSoon chan struct{}   // a delete asks for a block sweep
	stale     map[string]bool // notebooks other gateways announced
	staleAll  bool            // the listener (re)connected
	heardWake chan struct{}

	stop context.CancelFunc
	done sync.WaitGroup
}

// Open returns the store of db's org, keeping content blocks in blobs. It
// listens for other gateways' revisions until Close.
func Open(ctx context.Context, db *pg.DB, blobs blob.Store, o Options) (*Store, error) {
	if o.BlockCacheBytes <= 0 {
		o.BlockCacheBytes = 64 << 20
	}
	if o.ReadModelBytes <= 0 {
		o.ReadModelBytes = 64 << 20
	}
	var exists bool
	if err := db.Pool.QueryRow(ctx, "SELECT to_regclass('notebook_heads') IS NOT NULL").Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.New("database lacks the notebook store; run forgetmenote migrate")
	}
	s := &Store{
		db: db, blobs: blobs, opts: o,
		blocks:    newLRU[[32]byte, []byte](o.BlockCacheBytes * 3 / 4),
		manifests: newLRU[gitobj.ID, *manifest](o.BlockCacheBytes / 8),
		trees:     newLRU[gitobj.ID, []gitobj.TreeEntry](o.BlockCacheBytes / 8),
		notebooks: map[string]*Notebook{},
		models:    modelList{limit: o.ReadModelBytes},
		pending:   map[string]Head{},
		delivered: map[string]Head{},
		wake:      make(chan struct{}, 1),
		sweepSoon: make(chan struct{}, 1),
		stale:     map[string]bool{},
		heardWake: make(chan struct{}, 1),
	}
	background, cancel := context.WithCancel(context.Background())
	s.stop = cancel
	s.done.Add(6)
	go func() { defer s.done.Done(); db.Listen(background, revisionChannel, s.heard) }()
	go func() { defer s.done.Done(); db.Listen(background, liveChannel, s.heardLive) }()
	go func() { defer s.done.Done(); s.refresher(background) }()
	go func() { defer s.done.Done(); s.poll(background) }()
	go func() { defer s.done.Done(); s.dispatch(background) }()
	go func() { defer s.done.Done(); s.sweeper(background) }()
	return s, nil
}

// Close stops listening and waits for the store's background work.
func (s *Store) Close() {
	s.stop()
	s.done.Wait()
}

// Notebook returns the handle of notebook id, which need not exist yet.
func (s *Store) Notebook(id string) *Notebook {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.notebooks[id]
	if n == nil {
		n = &Notebook{s: s, id: id}
		s.notebooks[id] = n
	}
	return n
}

// Watch calls fn with each new head of any notebook, from this gateway's
// writes and from other gateways', and with the zero Head when a notebook it
// reported is deleted. Calls are serialized and may skip heads that a newer
// one replaced before delivery.
func (s *Store) Watch(fn func(notebook string, head Head)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watchers = append(s.watchers, fn)
}

func (s *Store) publish(notebook string, head Head) {
	s.mu.Lock()
	s.pending[notebook] = head
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Store) dispatch(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
		s.mu.Lock()
		pending, watchers := s.pending, s.watchers
		s.pending = map[string]Head{}
		var deliver []string
		for notebook, head := range pending {
			if s.delivered[notebook] != head {
				if head == (Head{}) {
					delete(s.delivered, notebook)
				} else {
					s.delivered[notebook] = head
				}
				deliver = append(deliver, notebook)
			}
		}
		s.mu.Unlock()
		for _, notebook := range deliver {
			for _, fn := range watchers {
				fn(notebook, pending[notebook])
			}
		}
	}
}

// heard notes another gateway's announcement, or "" after (re)connecting,
// when anything may have been missed, for the refresher. It never waits, so a
// slow reload cannot hold up the notifications after it.
func (s *Store) heard(subject string) {
	s.mu.Lock()
	if subject == "" {
		s.staleAll = true
	} else if notebookID.MatchString(subject) {
		s.stale[subject] = true
	}
	s.mu.Unlock()
	select {
	case s.heardWake <- struct{}{}:
	default:
	}
}

// refresher reloads the notebooks other gateways announced, a few at a time;
// announcements of one notebook that arrive while it waits count once.
func (s *Store) refresher(ctx context.Context) {
	slots := make(chan struct{}, 4)
	var running sync.WaitGroup
	defer running.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.heardWake:
		}
		s.mu.Lock()
		all, stale := s.staleAll, s.stale
		s.staleAll, s.stale = false, map[string]bool{}
		s.mu.Unlock()
		if all {
			every, cancel := context.WithTimeout(ctx, 10*time.Second)
			s.refreshAll(every)
			cancel()
		}
		for id := range stale {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			running.Add(1)
			go func() {
				defer func() { <-slots; running.Done() }()
				one, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				s.refresh(one, s.Notebook(id))
			}()
		}
	}
}

func (s *Store) poll(ctx context.Context) {
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	sweep := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		s.refreshAll(ctx)
		if time.Since(sweep) >= receiptSweeps {
			sweep = time.Now()
			s.SweepReceipts(ctx, time.Now())
		}
	}
}

// refreshAll compares every loaded notebook with its stored head in one query.
func (s *Store) refreshAll(ctx context.Context) {
	s.mu.Lock()
	loaded := map[string]*Notebook{}
	var ids []string
	for id, n := range s.notebooks {
		if n.model.Load() != nil {
			loaded[id] = n
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	rows, err := s.db.Pool.Query(ctx, `SELECT notebook, seq, commit_sha, incarnation FROM notebook_heads WHERE org = $1 AND notebook = ANY($2)`, s.db.Org, ids)
	if err != nil {
		return
	}
	type stored struct {
		head Head
		inc  int64
	}
	heads := map[string]stored{}
	for rows.Next() {
		var id string
		var h stored
		var sha []byte
		if rows.Scan(&id, &h.head.Seq, &sha, &h.inc) == nil {
			h.head.Commit = toID(sha)
			heads[id] = h
		}
	}
	rows.Close()
	for id, n := range loaded {
		if m := n.model.Load(); m != nil && !m.at(heads[id].head, heads[id].inc) {
			s.refresh(ctx, n)
		}
	}
}

// refresh reloads n's read model when its stored head moved, and publishes
// the stored head, or the zero Head when the notebook is gone. It holds
// n.loadMu throughout, as every install does, so each refresh reads a later
// moment than the install before it and heads are published in order.
func (s *Store) refresh(ctx context.Context, n *Notebook) {
	n.loadMu.Lock()
	defer n.loadMu.Unlock()
	head, inc, err := readHead(ctx, s.db.Pool, s.db.Org, n.id)
	if m := n.model.Load(); err == nil && m != nil && !m.at(head, inc) {
		var fresh *model
		if err = s.snapshot(ctx, func(tx pgx.Tx) (err error) {
			fresh, err = s.loadModel(ctx, tx, n.id)
			return err
		}); err == nil {
			head = n.install(fresh).head
		} else if !errors.Is(err, fs.ErrNotExist) {
			n.drop() // the next read rebuilds it
			err = nil
		}
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		n.drop()
		s.publish(n.id, Head{})
	case err == nil:
		s.publish(n.id, head)
	}
}

// readHead returns notebook's stored head and its incarnation, which a
// notebook created again after a delete gets anew and larger.
func readHead(ctx context.Context, q querier, org, notebook string) (Head, int64, error) {
	return scanHead(q.QueryRow(ctx, `SELECT seq, commit_sha, incarnation FROM notebook_heads WHERE org = $1 AND notebook = $2`, org, notebook), notebook)
}

func scanHead(row pgx.Row, notebook string) (Head, int64, error) {
	var h Head
	var inc int64
	var sha []byte
	err := row.Scan(&h.Seq, &sha, &inc)
	if errors.Is(err, pgx.ErrNoRows) {
		return h, 0, fmt.Errorf("notebook %s: %w", notebook, fs.ErrNotExist)
	}
	h.Commit = toID(sha)
	return h, inc, err
}

// committed installs m, the model after this gateway's write, publishes its
// head and tells other gateways; a lost announcement only delays them until
// their next poll.
func (n *Notebook) committed(m *model) {
	n.loadMu.Lock()
	n.s.publish(n.id, n.install(m).head)
	n.loadMu.Unlock()
	n.s.announce(n.id)
}

func (s *Store) announce(notebook string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.db.Announce(ctx, revisionChannel, notebook)
}

// Create starts notebook with the empty-tree commit "Initialize notes
// notebook" by init, committed by ForgetMeNote at the same time.
func (s *Store) Create(ctx context.Context, notebook string, init gitobj.Signature) (Head, error) {
	if !notebookID.MatchString(notebook) {
		return Head{}, invalid("notebook ID %q", notebook)
	}
	author, err := cleanSignature(init)
	if err != nil {
		return Head{}, err
	}
	n := s.Notebook(notebook)
	n.write.Lock()
	defer n.write.Unlock()
	commit := gitobj.Commit{Tree: gitobj.EmptyTree, Author: author, Committer: committer(author), Message: "Initialize notes notebook\n"}
	body := gitobj.EncodeCommit(commit)
	head := Head{Seq: 1, Commit: gitobj.HashObject("commit", body)}
	var inc int64
	err = pgx.BeginFunc(ctx, s.db.Pool, func(tx pgx.Tx) (err error) {
		if inc, err = insertHead(ctx, tx, s.db.Org, notebook, head.Commit); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO git_trees (org, notebook, tree_sha, body) VALUES ($1, $2, $3, '')`, s.db.Org, notebook, gitobj.EmptyTree[:]); err != nil {
			return err
		}
		return insertRevision(ctx, tx, s.db.Org, notebook, 1, 0, head.Commit, commit, body)
	})
	if err != nil {
		return Head{}, err
	}
	n.committed(emptyModel(head, inc))
	return head, nil
}

// insertHead starts notebook at revision 1, commit, and returns its new
// incarnation, or an fs.ErrExist error.
func insertHead(ctx context.Context, tx pgx.Tx, org, notebook string, commit gitobj.ID) (int64, error) {
	var inc int64
	err := tx.QueryRow(ctx, `INSERT INTO notebook_heads (org, notebook, seq, commit_sha) VALUES ($1, $2, 1, $3) ON CONFLICT DO NOTHING RETURNING incarnation`,
		org, notebook, commit[:]).Scan(&inc)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("notebook %s: %w", notebook, fs.ErrExist)
	}
	return inc, err
}

// Delete removes notebook's revisions, files, live documents and receipts.
// File contents no other notebook uses go too; their blocks go with the block
// sweep it asks for, once they are past the sweep's grace period
// (sweepAfterDelete).
func (s *Store) Delete(ctx context.Context, notebook string) error {
	return s.deleteNotebook(ctx, notebook, 0)
}

// DeleteIncarnation is Delete of the notebook only while it is incarnation
// inc, the one its creator read with StoredIncarnation: a notebook deleted
// and created again since is someone else's, and one purged since was
// published, so either is left as it is with an fs.ErrNotExist error, as a
// notebook already deleted is.
func (s *Store) DeleteIncarnation(ctx context.Context, notebook string, inc int64) error {
	if inc <= 0 {
		return fmt.Errorf("notebook %s: no incarnation to delete", notebook)
	}
	return s.deleteNotebook(ctx, notebook, inc)
}

// StoredIncarnation returns the notebook's head and incarnation as the
// database holds them now, not as this gateway last read them. The
// incarnation grows each time the notebook ID is created, so a caller that
// created the notebook can later delete only that one (DeleteIncarnation).
func (n *Notebook) StoredIncarnation(ctx context.Context) (Head, int64, error) {
	return readHead(ctx, n.s.db.Pool, n.s.db.Org, n.id)
}

func (s *Store) deleteNotebook(ctx context.Context, notebook string, inc int64) error {
	n := s.Notebook(notebook)
	n.write.Lock()
	defer n.write.Unlock()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = s.delete(ctx, notebook, inc); !isSerialization(err) {
			break
		}
	}
	if err != nil {
		return err
	}
	n.loadMu.Lock()
	n.drop()
	s.publish(notebook, Head{})
	n.loadMu.Unlock()
	s.announce(notebook)
	s.sweepAfterDelete()
	return nil
}

// delete removes notebook, or only its incarnation inc unless inc is 0.
func (s *Store) delete(ctx context.Context, notebook string, inc int64) error {
	org := s.db.Org
	return pgx.BeginFunc(ctx, s.db.Pool, func(tx pgx.Tx) error {
		_, current, err := lockHead(ctx, tx, org, notebook)
		if err != nil {
			return err
		}
		if inc != 0 && current != inc {
			return fmt.Errorf("notebook %s was deleted and created again: %w", notebook, fs.ErrNotExist)
		}
		var blobs [][]byte
		rows, err := tx.Query(ctx, `SELECT DISTINCT blob_sha FROM revision_changes WHERE org = $1 AND notebook = $2 AND blob_sha IS NOT NULL`, org, notebook)
		if err != nil {
			return err
		}
		blobs, err = pgx.CollectRows(rows, pgx.RowTo[[]byte])
		if err != nil {
			return err
		}
		for _, table := range []string{"live_docs", "operation_receipts", "entries", "revisions", "git_trees", "revision_rewrites", "purges", "notebook_heads"} {
			if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE org = $1 AND notebook = $2`, org, notebook); err != nil {
				return err
			}
		}
		return deleteUnusedVersions(ctx, tx, org, blobs)
	})
}

func isSerialization(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
}

// A Notebook is one notebook of the store.
type Notebook struct {
	s     *Store
	id    string
	write sync.Mutex // serializes this gateway's writers before the row lock

	loadMu sync.Mutex // held to build, install or drop the model
	model  atomicModel
}

// ID is the notebook's ID.
func (n *Notebook) ID() string { return n.id }

func committer(author gitobj.Signature) gitobj.Signature {
	return gitobj.Signature{Name: CommitterName, Email: CommitterEmail, When: author.When, TZ: author.TZ}
}

// cleanSignature makes an author as Git records one, stamped now when it
// carries no time.
func cleanSignature(s gitobj.Signature) (gitobj.Signature, error) {
	s.Name, s.Email = gitobj.CleanIdent(s.Name), gitobj.CleanIdent(s.Email)
	if s.Name == "" {
		return s, invalid("author name is empty")
	}
	if s.When == 0 && s.TZ == "" {
		now := gitobj.NewSignature(s.Name, s.Email, time.Now().UTC())
		s.When, s.TZ = now.When, now.TZ
	}
	if s.TZ == "" {
		s.TZ = "+0000"
	}
	if s.When < 0 || !validZone(s.TZ) {
		return s, invalid("author time %d %s", s.When, s.TZ)
	}
	return s, nil
}

func validZone(tz string) bool {
	if len(tz) != 5 || tz[0] != '+' && tz[0] != '-' {
		return false
	}
	for _, c := range tz[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
