package workspace

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// dbNotebook keeps a notebook in the notes store, Postgres and the blob
// store, with no working tree. Every revision is still a Git commit with the
// message, trailers and identities the Git backend wrote, so the export and
// every revision string stay as they were.
//
// It embeds *Store for the pure policy, cache and live state, and reads and
// writes the notebook through the notes store alone; no Git process runs.
type dbNotebook struct {
	*Store
	locksBlob   gitobj.ID // the lock policy loadLocks last parsed
	trashBlob   gitobj.ID // the Trash index loadTrash last parsed
	observedSeq int64     // the newest head watchers were told of; guarded by mu
	// authorityVersion is the version of the authority record loadAuthority
	// last read, 0 for none; guarded by mu.
	authorityVersion int64
	// trashArchives are the records of the Trash archives trashArchived
	// last read, by blob; guarded by mu.
	trashArchives map[gitobj.ID][]TrashRecord
	// hints are the heads the notes store announced that observe has not
	// taken yet (hear).
	hints struct {
		sync.Mutex
		head    notestore.Head // the newest head announced, when heard
		heard   bool
		deleted bool // the notebook was deleted since the last delivery
		running bool // a goroutine delivers them
	}
}

var _ notebookBackend = (*dbNotebook)(nil)

// systemIdentity signs what no request does, as Git's default identity did.
var systemIdentity = attribution.Identity{Name: "ForgetMeNote", Email: "forgetmenote@localhost", Source: "system"}

// OpenDB opens notebook from the notes store. stateDir holds the notebook's
// derived state; Root is stateDir/workspace, a name and cache key that need
// not exist, never a Git repository.
func OpenDB(ctx context.Context, notes *notestore.Store, notebook, stateDir string, browseExcluded []string) (*Store, error) {
	if !ValidNotebookID(notebook) {
		return nil, errors.New("invalid notebook ID")
	}
	n := notes.Notebook(notebook)
	if _, err := n.Head(ctx); err != nil {
		return nil, err
	}
	return openDB(ctx, notes, n, stateDir, browseExcluded)
}

// CreateDB creates notebook in the notes store with Git's first commit,
// "Initialize notes notebook", and opens it. It fails with an fs.ErrExist
// error when the notebook exists.
func CreateDB(ctx context.Context, notes *notestore.Store, notebook, stateDir string, browseExcluded []string) (*Store, error) {
	if !ValidNotebookID(notebook) {
		return nil, errors.New("invalid notebook ID")
	}
	if _, err := notes.Create(ctx, notebook, signature(systemIdentity, time.Now())); err != nil {
		return nil, err
	}
	return openDB(ctx, notes, notes.Notebook(notebook), stateDir, browseExcluded)
}

// DeleteDB removes notebook and all its history from the notes store, as
// undoing a create that failed later needs.
func DeleteDB(ctx context.Context, notes *notestore.Store, notebook string) error {
	return notes.Delete(ctx, notebook)
}

func openDB(ctx context.Context, notes *notestore.Store, n *notestore.Notebook, stateDir string, browseExcluded []string) (*Store, error) {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, err
	}
	s := &Store{Root: filepath.Join(stateDir, "workspace"), notes: n, noteStore: notes, notebook: n.ID(), browseExcluded: slices.Clone(browseExcluded), live: map[string]*liveRecord{}}
	db := &dbNotebook{Store: s}
	s.backend = db
	// Refuse a notebook whose Trash index or archives are invalid.
	s.mu.Lock()
	index, err := db.loadTrash(ctx)
	if err == nil {
		err = db.checkTrashArchives(ctx, index)
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	watchNotebook(db)
	return s, nil
}

// observe tells watchers about head, or about the notebook's deletion with
// the zero head. A head older than one they heard of arrives late from the
// notes store and is dropped; a read of the current head is never stale.
// Callers hold mu.
func (n *dbNotebook) observe(head notestore.Head, current bool) {
	if head == (notestore.Head{}) {
		n.observedSeq = 0
		n.changed("")
		return
	}
	if !current && head.Seq < n.observedSeq {
		return
	}
	n.observedSeq = head.Seq
	n.changed(head.Commit.String())
}

// Close stops this store's hints; the notes store stays open for others.
func (n *dbNotebook) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.liveClosed {
		return nil
	}
	n.liveClosed = true
	n.notifyLive("")
	unwatchNotebook(n)
	return nil
}

// Watch supplies coalesced hints of new heads, from this process's commits
// and other gateways' alike; consumers read the notebook after each.
func (n *dbNotebook) Watch() (<-chan struct{}, func(), error) {
	return n.watchChanges()
}

func (n *dbNotebook) Read(ctx context.Context, path, revision string) (Document, error) {
	n.mu.Lock()
	generation := n.authorityGeneration.Load()
	if _, err := n.resolve(path, false); err != nil {
		n.mu.Unlock()
		return Document{}, err
	}
	if revision == "" {
		var err error
		if revision, err = n.head(ctx); err != nil {
			n.mu.Unlock()
			return Document{}, err
		}
	}
	n.mu.Unlock()
	doc, err := n.readRevision(ctx, path, revision)
	if err != nil {
		return Document{}, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if generation != n.authorityGeneration.Load() {
		return Document{}, ErrConflict
	}
	if _, err := n.resolve(path, false); err != nil {
		return Document{}, err
	}
	return doc, nil
}

func (n *dbNotebook) Save(ctx context.Context, path string, content []byte, expected, actor string) (Document, error) {
	ctx = n.mutationContext(ctx)
	if path == "AGENTS.md" {
		return Document{}, ErrForbidden
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if explicitCheckpoint(ctx) && n.hasLivePending() {
		return Document{}, ErrLivePending
	}
	if err := n.loadLocks(ctx); err != nil {
		return Document{}, err
	}
	if _, err := n.resolve(path, true); err != nil {
		return Document{}, err
	}
	return n.save(ctx, path, content, expected)
}

// save owns the revision checks and the commit of one file, a note or a
// service-managed policy file. Callers hold mu and validate the target.
func (n *dbNotebook) save(ctx context.Context, path string, content []byte, expected string) (Document, error) {
	if err := n.checkMutationChanges(ctx, map[string][]byte{path: content}); err != nil {
		return Document{}, err
	}
	if len(content) > MaxFileBytes() {
		return Document{}, invalidInput("file exceeds FMN_LIMIT_FILE_BYTES")
	}
	if strings.HasSuffix(strings.ToLower(path), ".md") && (!utf8.Valid(content) || strings.ContainsRune(string(content), 0)) {
		return Document{}, invalidInput("notes must be UTF-8 text")
	}
	if path != "AGENTS.md" && strings.HasSuffix(path, ".md") {
		if err := validateContainerWrite(content); err != nil {
			return Document{}, err
		}
	}
	if err := n.clean(ctx); err != nil {
		return Document{}, err
	}
	head, err := n.head(ctx)
	if err != nil {
		return Document{}, err
	}
	rev, err := parseRevision(head)
	if err != nil {
		return Document{}, err
	}
	old, file, err := n.notes.Read(ctx, rev, path)
	exists := err == nil
	if errors.Is(err, notestore.ErrInvalid) || exists && file.Mode != gitobj.ModeFile && file.Mode != gitobj.ModeExecutable {
		return Document{}, ErrForbidden // a submodule or symbolic link is not content
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Document{}, err
	}
	version, err := n.fileVersion(ctx, path, head)
	if err != nil {
		return Document{}, err
	}
	if expected == "" {
		if exists {
			return Document{}, ErrConflict
		}
	} else if expected != head {
		ancestor, err := n.isAncestor(ctx, expected, head)
		if err != nil {
			return Document{}, err
		}
		if !ancestor {
			return Document{}, ErrConflict
		}
		previous, err := n.fileVersion(ctx, path, expected)
		if err != nil {
			return Document{}, err
		}
		if previous != version {
			return Document{}, ErrConflict
		}
	}
	if exists && string(old) == string(content) {
		return Document{path, old, head, version}, nil
	}
	result, err := n.applyChanges(ctx, head, "Update "+path, []pathChange{{Path: path, Content: content}})
	if err != nil {
		return Document{}, err
	}
	head = result.Head.Commit.String()
	return Document{path, content, head, head}, nil
}

func (n *dbNotebook) Status(ctx context.Context) (Status, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	head, err := n.head(ctx)
	if err != nil {
		return Status{}, err
	}
	n.checkpointing = true
	dirty := n.clean(ctx)
	n.checkpointing = false
	if dirty != nil && !errors.Is(dirty, ErrDirty) && !errors.Is(dirty, ErrLivePending) {
		return Status{}, dirty
	}
	return Status{Revision: head, Dirty: dirty != nil}, nil
}

// notebookWatches has, for each notes store, the workspace stores open on
// each of its notebooks. A notes store's watcher cannot be removed, so each
// gets one that dispatches to whichever stores are open.
var notebookWatches struct {
	sync.Mutex
	stores map[*notestore.Store]*watchRegistry
}

type watchRegistry struct {
	mu     sync.Mutex
	stores map[string]map[*dbNotebook]bool
}

func watchNotebook(n *dbNotebook) {
	notebookWatches.Lock()
	if notebookWatches.stores == nil {
		notebookWatches.stores = map[*notestore.Store]*watchRegistry{}
	}
	r := notebookWatches.stores[n.noteStore]
	if r == nil {
		r = &watchRegistry{stores: map[string]map[*dbNotebook]bool{}}
		notebookWatches.stores[n.noteStore] = r
		n.noteStore.Watch(r.changed)
	}
	notebookWatches.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stores[n.notebook] == nil {
		r.stores[n.notebook] = map[*dbNotebook]bool{}
	}
	r.stores[n.notebook][n] = true
}

func unwatchNotebook(n *dbNotebook) {
	notebookWatches.Lock()
	r := notebookWatches.stores[n.noteStore]
	notebookWatches.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.stores[n.notebook], n)
	if len(r.stores[n.notebook]) == 0 {
		delete(r.stores, n.notebook)
	}
}

// changed tells the notebook's stores of a new head, or of its deletion
// with the zero head. It runs on the notes store's one dispatch goroutine,
// which serves every notebook, so it never waits for a store's lock.
func (r *watchRegistry) changed(notebook string, head notestore.Head) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for n := range r.stores[notebook] {
		n.hear(head)
	}
}

// hear queues head, or the zero head of the notebook's deletion, for
// observe, which runs under mu on a goroutine of its own while hints wait:
// an operation that holds this store's lock delays only this store's
// watchers. Of the heads announced meanwhile only the newest is observed;
// a deletion is never passed over, because the notebook created again
// starts over from a lower sequence, which observe would drop as stale.
func (n *dbNotebook) hear(head notestore.Head) {
	n.hints.Lock()
	defer n.hints.Unlock()
	if head == (notestore.Head{}) {
		n.hints.deleted, n.hints.heard = true, false
	} else if !n.hints.heard || head.Seq > n.hints.head.Seq {
		n.hints.head, n.hints.heard = head, true
	}
	if !n.hints.running {
		n.hints.running = true
		go n.deliverHints()
	}
}

// deliverHints observes the hints heard until none waits.
func (n *dbNotebook) deliverHints() {
	for {
		n.hints.Lock()
		deleted, heard, head := n.hints.deleted, n.hints.heard, n.hints.head
		n.hints.deleted, n.hints.heard = false, false
		if !deleted && !heard {
			n.hints.running = false
			n.hints.Unlock()
			return
		}
		n.hints.Unlock()
		n.mu.Lock()
		if !n.liveClosed {
			if deleted {
				n.observe(notestore.Head{}, false)
			}
			if heard {
				n.observe(head, false)
			}
		}
		n.mu.Unlock()
	}
}
