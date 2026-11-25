// Package gitexport mirrors each notebook of a notestore.Store into a bare
// Git repository, <exports>/<notebook>.git with the one branch main, and
// imports Git history and bundles into the store.
//
// The export writes the store's objects in this process, as loose objects
// (gitobj.WriteLoose) or, for a first export of many revisions and a purge's
// rebuild, as one pack that git index-pack indexes, so it reproduces every
// commit and tree byte for byte: the same revisions always give the same
// repository. It runs after every commit the store reports and on a periodic
// catch-up, never on a request path; Git itself runs only to index those
// packs, for maintenance (repack and fsck), bundles and imports, each bounded
// by a timeout through internal/procgroup. The external backup worker
// (deploy/git-backup) pushes these repositories; this package never pushes.
//
// History only ever grows. An export whose branch names a commit the
// notebook no longer has is left alone until Rebuild, which a purge calls
// after rewriting history, replaces it and writes the purge flag that lets
// the backup worker replace the remote branch once. When a purge the
// notebook recorded rewrote that commit and stopped before its Rebuild, the
// next export rebuilds it for that purge instead.
package gitexport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"forgetmenote/internal/durable"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// Options tune an Exporter. Zero values take the defaults.
type Options struct {
	// CatchUp is how often every known notebook is compared with its export.
	// Default five minutes.
	CatchUp time.Duration
	// Maintenance is the least time between repacks of one repository with
	// new objects. Default one hour.
	Maintenance time.Duration
	// Notebooks lists notebooks the catch-up includes besides those with an
	// export and those the store reported changes for, such as the catalog's.
	Notebooks func(context.Context) ([]string, error)
	// Logf reports background failures. Default log.Printf.
	Logf func(format string, args ...any)
}

var (
	// ErrDiverged is an export whose branch holds history the notebook no
	// longer has. Only Rebuild, for an authorized purge, replaces it.
	ErrDiverged = errors.New("the notebook's history no longer extends its Git export; only a purge may rebuild it")
	// ErrBundleLimit is a bundle larger than Bundle's limit. Its text is the
	// notebook export's.
	ErrBundleLimit = errors.New("MCP export exceeds 1 GiB; use operator backup")
	// ErrExporting is another process exporting the same directory.
	ErrExporting = errors.New("another process is exporting these notebooks")
)

// Timeouts of the Git commands the exporter runs.
var (
	bundleTimeout = 2 * time.Minute
	repackTimeout = 15 * time.Minute
	fsckTimeout   = 15 * time.Minute
)

// An Exporter keeps the export repositories of one store's notebooks in one
// directory. One process at a time may hold a directory.
type Exporter struct {
	store *notestore.Store
	dir   string
	opts  Options
	lock  *os.File

	mu      sync.Mutex
	repos   map[string]*repo
	dirty   map[string]bool
	purging map[string]int // purges of each notebook this process runs (Purging)
	wake    chan struct{}
	started bool
	closed  bool
	stop    context.CancelFunc
	done    sync.WaitGroup
}

// repo is one notebook's export: its lock and what this process knows of it.
type repo struct {
	// mu is held to write the repository (export, maintenance, rebuild,
	// removal) and shared to read it for a bundle.
	mu         sync.RWMutex
	pending    bool // objects written since the last maintenance
	maintained time.Time
}

// New returns the exporter of store's notebooks into dir, which it creates,
// and locks dir against other processes until Close.
func New(store *notestore.Store, dir string, o Options) (*Exporter, error) {
	if o.CatchUp <= 0 {
		o.CatchUp = 5 * time.Minute
	}
	if o.Maintenance <= 0 {
		o.Maintenance = time.Hour
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := durable.MkdirAll(dir, 0o700, nil); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrExporting, dir)
		}
		return nil, err
	}
	// Directories an interrupted creation or removal left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		lock.Close()
		return nil, err
	}
	for _, entry := range entries {
		if name := entry.Name(); entry.IsDir() && strings.HasPrefix(name, ".") && (strings.Contains(name, ".git.init-") || strings.Contains(name, ".removed-")) {
			os.RemoveAll(filepath.Join(dir, name))
		}
	}
	return &Exporter{store: store, dir: dir, opts: o, lock: lock, repos: map[string]*repo{}, dirty: map[string]bool{}, purging: map[string]int{}, wake: make(chan struct{}, 1)}, nil
}

// Purging marks notebook as being purged by this process until end runs.
// Meanwhile an export that finds the notebook's history rewritten leaves
// the export to that purge's own Rebuild instead of rebuilding it as well.
func (e *Exporter) Purging(notebook string) (end func()) {
	e.mu.Lock()
	e.purging[notebook]++
	e.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.purging[notebook]--; e.purging[notebook] <= 0 {
				delete(e.purging, notebook)
			}
		})
	}
}

// resumePurge finishes a purge that rewrote notebook's history and stopped
// before it rebuilt the export, whose branch, ref, names a commit the
// notebook no longer has: when a recorded purge rewrote ref, it rebuilds the
// export for the newest such purge, as that purge would have, and reports
// true. A branch no recorded purge rewrote stays diverged: only a purge
// authorizes replacing exported history. r.mu is held.
func (e *Exporter) resumePurge(ctx context.Context, nb *notestore.Notebook, notebook string, ref gitobj.ID, r *repo) (bool, error) {
	e.mu.Lock()
	running := e.purging[notebook] > 0
	e.mu.Unlock()
	if running {
		return false, nil
	}
	record, ok, err := nb.Rewritten(ctx, ref)
	if err != nil || !ok {
		return false, err
	}
	purge := record.Purges[len(record.Purges)-1]
	// As a purge's own rebuild, it runs to its end: one stopped midway can
	// leave the old objects behind a branch that no longer diverges.
	if err := e.rebuild(context.WithoutCancel(ctx), notebook, purge, r); err != nil {
		return false, &purgeRebuildError{notebook: notebook, purge: purge, err: err}
	}
	e.opts.Logf("git export of notebook %s: rebuilt for purge %s, which stopped before it did", notebook, purge)
	return true, nil
}

// A purgeRebuildError is a rebuild of notebook's export for a purge that
// stopped before it rebuilt it, which failed.
type purgeRebuildError struct {
	notebook, purge string
	err             error
}

func (e *purgeRebuildError) Error() string {
	return fmt.Sprintf("notebook %s: rebuilding the export for purge %s, which stopped before it did: %v", e.notebook, e.purge, e.err)
}

func (e *purgeRebuildError) Unwrap() error { return e.err }

// Path is the export repository of notebook.
func (e *Exporter) Path(notebook string) string { return filepath.Join(e.dir, notebook+".git") }

// Branch returns the commit notebook's export branch names, or the zero ID
// before its first export, as a purge plan asks whether the export holds
// commits the purge replaces.
func (e *Exporter) Branch(notebook string) (gitobj.ID, error) {
	if !notebookID.MatchString(notebook) {
		return gitobj.ID{}, fmt.Errorf("invalid notebook ID %q", notebook)
	}
	return readRef(e.Path(notebook))
}

func (e *Exporter) repo(notebook string) (*repo, error) {
	if !notebookID.MatchString(notebook) {
		return nil, fmt.Errorf("invalid notebook ID %q", notebook)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, errors.New("the exporter is closed")
	}
	r := e.repos[notebook]
	if r == nil {
		r = &repo{}
		e.repos[notebook] = r
	}
	return r, nil
}

// Start exports in the background: each notebook the store reports a new
// head for, and every known notebook on the catch-up interval, the first
// time right away; then it maintains repositories that gained objects.
func (e *Exporter) Start() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started || e.closed {
		return
	}
	e.started = true
	ctx, cancel := context.WithCancel(context.Background())
	e.stop = cancel
	e.store.Watch(e.changed)
	e.done.Add(1)
	go func() {
		defer e.done.Done()
		e.run(ctx)
	}()
}

// changed notes a notebook's new head for the worker. It never waits, so the
// store's other watchers are not held up by an export.
func (e *Exporter) changed(notebook string, head notestore.Head) {
	if head == (notestore.Head{}) || !notebookID.MatchString(notebook) {
		return // a deleted notebook keeps its export until Remove
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.dirty[notebook] = true
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *Exporter) run(ctx context.Context) {
	tick := time.NewTicker(e.opts.CatchUp)
	defer tick.Stop()
	catchUp := true
	for {
		if catchUp {
			e.markAll(ctx)
		}
		e.mu.Lock()
		dirty := e.dirty
		e.dirty = map[string]bool{}
		e.mu.Unlock()
		for notebook := range dirty {
			if ctx.Err() != nil {
				return
			}
			e.background(ctx, notebook)
		}
		select {
		case <-ctx.Done():
			return
		case <-e.wake:
			catchUp = false
		case <-tick.C:
			catchUp = true
		}
	}
}

// background exports notebook and maintains its repository when due,
// reporting failures; the next catch-up retries them.
func (e *Exporter) background(ctx context.Context, notebook string) {
	if _, err := e.Sync(ctx, notebook); err != nil {
		if !errors.Is(err, fs.ErrNotExist) && ctx.Err() == nil {
			e.opts.Logf("git export of notebook %s: %v", notebook, err)
		}
		return
	}
	r, err := e.repo(notebook)
	if err != nil {
		return
	}
	r.mu.RLock()
	due := r.pending && time.Since(r.maintained) >= e.opts.Maintenance
	r.mu.RUnlock()
	if due {
		if err := e.Maintain(ctx, notebook); err != nil && ctx.Err() == nil {
			e.opts.Logf("git export maintenance of notebook %s: %v", notebook, err)
		}
	}
}

// markAll marks every known notebook for the catch-up: those with an export
// repository, those this process has seen, and the configured list.
func (e *Exporter) markAll(ctx context.Context) {
	names := map[string]bool{}
	entries, err := os.ReadDir(e.dir)
	if err != nil {
		e.opts.Logf("git export catch-up: %v", err)
	}
	for _, entry := range entries {
		if id, ok := strings.CutSuffix(entry.Name(), ".git"); ok && entry.IsDir() && notebookID.MatchString(id) {
			names[id] = true
		}
	}
	if e.opts.Notebooks != nil {
		listed, err := e.opts.Notebooks(ctx)
		if err != nil {
			e.opts.Logf("git export catch-up: %v", err)
		}
		for _, id := range listed {
			names[id] = true
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for id := range e.repos {
		names[id] = true
	}
	for id := range names {
		if notebookID.MatchString(id) {
			e.dirty[id] = true
		}
	}
}

// Close stops the background work, waits for it and releases the directory.
func (e *Exporter) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	stop := e.stop
	e.mu.Unlock()
	if stop != nil {
		stop()
	}
	e.done.Wait()
	e.lock.Close()
}

// Sync brings notebook's export up to its head, creating the repository on
// the first export, and returns the head the branch now names. It writes only
// the objects of revisions after the one the branch named, then moves the
// branch. A missing notebook is an fs.ErrNotExist error; an export of history
// the notebook no longer has is ErrDiverged.
func (e *Exporter) Sync(ctx context.Context, notebook string) (notestore.Head, error) {
	r, err := e.repo(notebook)
	if err != nil {
		return notestore.Head{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return e.sync(ctx, notebook, r)
}

func (e *Exporter) sync(ctx context.Context, notebook string, r *repo) (notestore.Head, error) {
	nb := e.store.Notebook(notebook)
	head, err := nb.Head(ctx)
	if err != nil {
		return notestore.Head{}, err
	}
	path := e.Path(notebook)
	if err := initRepo(ctx, path); err != nil {
		return notestore.Head{}, err
	}
	ref, err := readRef(path)
	if err != nil {
		return notestore.Head{}, err
	}
	if ref == head.Commit {
		return head, nil
	}
	// The branch's commit is looked up on every export rather than
	// remembered: a purge can rewrite what follows it.
	var after int64
	if !ref.IsZero() {
		info, err := nb.Commit(ctx, ref)
		if errors.Is(err, fs.ErrNotExist) {
			resumed, err := e.resumePurge(ctx, nb, notebook, ref, r)
			if err != nil {
				return notestore.Head{}, err
			}
			if !resumed {
				return notestore.Head{}, fmt.Errorf("notebook %s: %w: the export's %s is not one of its revisions", notebook, ErrDiverged, ref)
			}
			// The rebuild exported every revision there was; any since
			// follow as usual.
			return e.sync(ctx, notebook, r)
		}
		if err != nil {
			return notestore.Head{}, err
		}
		after = info.Seq
	}
	// Revisions come parents first, and every descendant of ref comes after
	// it, so a commit descends from ref exactly when one of its parents does.
	descends := map[gitobj.ID]bool{ref: true}
	write := writeObjects
	packed := ref.IsZero() && head.Seq-after >= packRevisions
	if packed {
		write = writePack
	}
	last, err := write(ctx, nb, path, after, func(id gitobj.ID, body []byte) {
		c, err := gitobj.ParseCommit(body)
		for _, p := range c.Parents {
			if err == nil && descends[p] {
				descends[id] = true
			}
		}
	})
	if err != nil {
		return notestore.Head{}, err
	}
	if last.IsZero() {
		return notestore.Head{}, fmt.Errorf("notebook %s: no revisions follow the export's %s", notebook, ref)
	}
	if !ref.IsZero() && !descends[last] {
		// Imported history with branches passes through heads that are not
		// each other's descendants; the branch waits for one that is.
		return notestore.Head{}, fmt.Errorf("notebook %s: %w: %s does not descend from the export's %s", notebook, ErrDiverged, last, ref)
	}
	if err := writeRef(path, last); err != nil {
		return notestore.Head{}, err
	}
	r.pending = true
	if packed {
		// The export is complete; only its deltas are missing, which no
		// later maintenance computes.
		if err := repackDeltas(ctx, path); err != nil {
			e.opts.Logf("git export of notebook %s: computing the first export's deltas: %v", notebook, err)
		}
	}
	if last == head.Commit {
		return head, nil
	}
	// Revisions committed during the export came along.
	info, err := nb.Commit(ctx, last)
	if err != nil {
		return notestore.Head{}, err
	}
	return notestore.Head{Seq: info.Seq, Commit: last}, nil
}

// looseWriters is how many objects an export writes, or compresses into a
// pack, at once. Each loose object waits for two fsyncs, so writing several
// overlaps the waits.
const looseWriters = 4

// packRevisions is how many revisions a first export needs to write them as
// one pack, which waits for one fsync, instead of loose objects. Later
// exports stay loose: a repack keeps a pack's whole objects as they are, so
// each pack writePack makes needs its deltas computed over the whole
// repository (repackDeltas).
var packRevisions int64 = 100

// repackDeltas packs everything the branch reaches into one pack, computing
// its deltas afresh (-f), and drops the other packs and the loose objects
// they hold. A pack writePack made holds every object whole: F2's export was
// 932 MB that way, and 727 MB once this repack, which took 22 s, computed its
// deltas.
func repackDeltas(ctx context.Context, path string) error {
	_, err := runGit(ctx, path, repackTimeout, 1<<20, "repack", "-a", "-d", "-f", "-q")
	return err
}

// writeObjects writes the objects of nb's revisions after seq after into the
// repository at path and returns the last commit, calling commit with each.
// Every object is durable when it returns without error, so the branch may
// then name the last commit.
func writeObjects(ctx context.Context, nb *notestore.Notebook, path string, after int64, commit func(id gitobj.ID, body []byte)) (gitobj.ID, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	type object struct {
		kind string
		body []byte
	}
	work := make(chan object, 2*looseWriters)
	var writers sync.WaitGroup
	for range looseWriters {
		writers.Go(func() {
			for o := range work {
				if ctx.Err() != nil {
					continue // drain; the export failed
				}
				if _, err := gitobj.WriteLoose(path, o.kind, o.body); err != nil {
					cancel(err)
				}
			}
		})
	}
	var last gitobj.ID
	err := nb.Objects(ctx, after, func(kind string, id gitobj.ID, body []byte) error {
		if kind == "commit" {
			last = id
			if commit != nil {
				commit(id, body)
			}
		}
		select {
		case work <- object{kind, body}:
			return nil
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	})
	close(work)
	writers.Wait()
	if cause := context.Cause(ctx); err == nil && cause != nil {
		err = cause
	}
	return last, err
}

// Maintain packs the loose objects of notebook's export (git repack -d) and
// checks the repository (git fsck --strict), each within a timeout. First it
// deletes the temporary files that an export, rebuild or repack stopped
// halfway left in the repository, such as a first export's staged pack (0.9
// GB for F2), which nothing else removes until a purge's rebuild. They are
// stale: Maintain holds the repository's lock, which every export, rebuild
// and bundle of this process holds while it writes or reads, and the
// exporter's lock on its directory keeps other processes out.
func (e *Exporter) Maintain(ctx context.Context, notebook string) error {
	r, err := e.repo(notebook)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	path := e.Path(notebook)
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil {
		return err
	}
	if err := removeTemporaries(path); err != nil {
		return err
	}
	// Geometric repacking keeps the pack count logarithmic in the history,
	// so frequent small repacks never pile up packs.
	if _, err := runGit(ctx, path, repackTimeout, 1<<20, "repack", "-d", "-q", "--geometric=2"); err != nil {
		return err
	}
	if err := fsck(ctx, path); err != nil {
		return err
	}
	r.pending = false
	r.maintained = time.Now()
	return nil
}

func fsck(ctx context.Context, path string) error {
	_, err := runGit(ctx, path, fsckTimeout, 1<<20, "fsck", "--strict", "--no-dangling", "--no-reflogs", "--no-progress")
	return err
}

// Bundle writes a bundle of notebook's whole export (git bundle create -
// --all) to w after bringing the export up to the head. More than limit bytes
// stop Git and fail with ErrBundleLimit; Git has two minutes.
func (e *Exporter) Bundle(ctx context.Context, notebook string, w io.Writer, limit int64) error {
	if _, err := e.Sync(ctx, notebook); err != nil {
		return err
	}
	r, err := e.repo(notebook)
	if err != nil {
		return err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	ctx, cancel := context.WithTimeout(ctx, bundleTimeout)
	defer cancel()
	cmd := gitCommand(ctx, e.Path(notebook), "bundle", "create", "-", "--all")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	count, copyErr := io.Copy(w, io.LimitReader(pipe, limit))
	if copyErr == nil && count == limit {
		var extra [1]byte
		if n, _ := pipe.Read(extra[:]); n > 0 {
			copyErr = ErrBundleLimit
		}
	}
	if copyErr != nil {
		cancel()
	}
	return errors.Join(copyErr, cmd.Wait())
}

// Remove deletes notebook's export repository, for a deleted notebook.
func (e *Exporter) Remove(ctx context.Context, notebook string) error {
	r, err := e.repo(notebook)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return e.remove(notebook, r)
}

func (e *Exporter) remove(notebook string, r *repo) error {
	path := e.Path(notebook)
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	// Rename first, so a crash never leaves a partial repository at path.
	doomed, err := os.MkdirTemp(e.dir, "."+notebook+".removed-")
	if err != nil {
		return err
	}
	if err := os.Rename(path, filepath.Join(doomed, "repo")); err != nil {
		os.Remove(doomed)
		return err
	}
	r.pending, r.maintained = false, time.Time{}
	if err := durable.SyncDir(e.dir); err != nil {
		return err
	}
	return os.RemoveAll(doomed)
}

// Recreate replaces notebook's export with a fresh export of its history and
// returns the head, for an operator repairing an export that is ErrDiverged
// or fails its checks, which Sync never touches. The new repository is a new
// directory, so a running backup worker must restart, and a remote holding
// commits the notebook no longer has blocks as diverged; so an export whose
// branch a recorded purge rewrote is rebuilt for that purge instead, as Sync
// does, and its flag lets the worker replace them. A purge flag the
// backup worker has not spent carries over, written before the branch, as
// long as the notebook still has the purge's rewritten commit; an invalid
// flag, which authorizes nothing, does not. A missing notebook keeps its
// export and is an fs.ErrNotExist error.
func (e *Exporter) Recreate(ctx context.Context, notebook string) (notestore.Head, error) {
	r, err := e.repo(notebook)
	if err != nil {
		return notestore.Head{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	nb := e.store.Notebook(notebook)
	if _, err := nb.Head(ctx); err != nil {
		return notestore.Head{}, err
	}
	path := e.Path(notebook)
	// An export a purge rewrote and did not rebuild is rebuilt for that
	// purge, with its flag: a new export without one would leave the
	// remote's old history blocking every push as diverged.
	if ref, err := readRef(path); err == nil && !ref.IsZero() {
		if _, err := nb.Commit(ctx, ref); errors.Is(err, fs.ErrNotExist) {
			resumed, err := e.resumePurge(ctx, nb, notebook, ref, r)
			if failed := (*purgeRebuildError)(nil); errors.As(err, &failed) {
				// An export an operator recreates may be too damaged to
				// rebuild in place: a new one is rebuilt for the purge.
				if err = e.rebuildAnew(ctx, notebook, failed.purge, ref, r); err != nil {
					err = errors.Join(failed, err)
				}
				resumed = err == nil
			}
			if err != nil {
				return notestore.Head{}, err
			}
			if resumed {
				return e.sync(ctx, notebook, r)
			}
		}
	}
	// The old repository's purge lock is held until the flag is in the new
	// one, so a backup worker spending it waits and then finds it there.
	unlock := func() {}
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err == nil {
		if unlock, err = lockPurge(ctx, path); err != nil {
			return notestore.Head{}, err
		}
	}
	err = e.recreate(ctx, nb, path, notebook, r)
	unlock()
	if err != nil {
		return notestore.Head{}, err
	}
	return e.sync(ctx, notebook, r)
}

// rebuildAnew replaces notebook's export, which a rebuild for purge could
// not repair, with a new repository rebuilt for that purge. Its flag names
// ref, the old export's branch, the commits the old flag named and every
// commit the notebook's purges rewrote, so the backup worker replaces, once,
// a remote at any commit of the old export that a purge rewrote, however far
// behind ref the worker last pushed it, as the rebuild in place would.
//
// The new repository is built beside the export and takes its place only
// once complete: a rebuild that fails leaves the old export, and its flag,
// as they were. A process that stops between the two renames of that swap
// leaves neither, as a removal stopped midway would. r.mu is held.
func (e *Exporter) rebuildAnew(ctx context.Context, notebook, purge string, ref gitobj.ID, r *repo) error {
	path := e.Path(notebook)
	replaced := []gitobj.ID{ref}
	rewritten, err := e.store.Notebook(notebook).RewrittenAll(ctx)
	if err != nil {
		return err
	}
	for old := range rewritten {
		replaced = append(replaced, old)
	}
	// The old repository's purge lock is held until the new one, with its
	// flag, is in place, as Recreate holds it.
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err == nil {
		unlock, err := lockPurge(ctx, path)
		if err != nil {
			return err
		}
		defer unlock()
	}
	if flag, ok, _ := ReadPurge(path); ok {
		replaced = append(replaced, flag.Replaces...)
	}
	// New removes the staging directory a process that stopped left.
	staging, err := os.MkdirTemp(e.dir, "."+notebook+".git.init-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	fresh := filepath.Join(staging, "repo")
	if err := e.rebuildAt(context.WithoutCancel(ctx), notebook, purge, fresh, r, replaced...); err != nil {
		return err
	}
	doomed, err := os.MkdirTemp(e.dir, "."+notebook+".removed-")
	if err != nil {
		return err
	}
	moved := filepath.Join(doomed, "repo")
	if err := os.Rename(path, moved); err != nil && !errors.Is(err, fs.ErrNotExist) {
		os.Remove(doomed)
		return err
	}
	if err := os.Rename(fresh, path); err != nil {
		if os.Rename(moved, path) == nil {
			os.Remove(doomed)
		}
		return err
	}
	if err := errors.Join(durable.SyncDir(e.dir), os.RemoveAll(doomed)); err != nil {
		return err
	}
	e.opts.Logf("git export of notebook %s: recreated and rebuilt for purge %s, which stopped before it did", notebook, purge)
	return nil
}

// recreate removes notebook's export, carrying its purge flag, if valid and
// still about the notebook's history, into a new empty repository.
func (e *Exporter) recreate(ctx context.Context, nb *notestore.Notebook, path, notebook string, r *repo) error {
	flag, flagged, _ := ReadPurge(path)
	if flagged {
		if _, err := nb.Commit(ctx, flag.Head); errors.Is(err, fs.ErrNotExist) {
			flagged = false
		} else if err != nil {
			return err
		}
	}
	if err := e.remove(notebook, r); err != nil || !flagged {
		return err
	}
	if err := initRepo(ctx, path); err != nil {
		return err
	}
	unlock, err := lockPurge(ctx, path)
	if err != nil {
		return err
	}
	defer unlock()
	return writePurge(path, flag)
}

// Rebuild replaces notebook's export after purgeID rewrote its history: it
// writes every object of every revision again, as one pack, writes the purge
// flag, moves the branch to the rewritten head, then drops every object the
// branch no longer reaches (git repack -a -d -f, git prune --expire=now) and
// checks the result. The repository keeps its place and identity, so a
// running backup worker keeps watching it.
//
// The purge must have marked its rewritten commits with the trailer
// "Gitnotes-Purge: <purgeID>"; the newest such commit goes into the flag, with
// every commit the rewrite dropped from the export. A rebuild interrupted
// after the flag was written can run again: the flag keeps the commits an
// earlier attempt or an earlier unpushed purge dropped.
func (e *Exporter) Rebuild(ctx context.Context, notebook, purge string) error {
	if !purgeID.MatchString(purge) {
		return fmt.Errorf("invalid purge ID %q", purge)
	}
	r, err := e.repo(notebook)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return e.rebuild(ctx, notebook, purge, r)
}

// rebuild is Rebuild with r.mu held. The flag also names replaced, commits
// of an earlier export the remote may hold.
func (e *Exporter) rebuild(ctx context.Context, notebook, purge string, r *repo, replaced ...gitobj.ID) error {
	return e.rebuildAt(ctx, notebook, purge, e.Path(notebook), r, replaced...)
}

// rebuildAt is rebuild of the repository at path, which becomes notebook's
// export.
func (e *Exporter) rebuildAt(ctx context.Context, notebook, purge, path string, r *repo, replaced ...gitobj.ID) error {
	nb := e.store.Notebook(notebook)
	if _, err := nb.Head(ctx); err != nil {
		return err
	}
	if err := initRepo(ctx, path); err != nil {
		return err
	}
	old, err := readRef(path)
	if err != nil {
		return err
	}
	var marked gitobj.ID
	last, err := writePack(ctx, nb, path, 0, func(id gitobj.ID, body []byte) {
		if c, err := gitobj.ParseCommit(body); err == nil {
			for _, t := range gitobj.Trailers(c.Message) {
				if strings.EqualFold(t.Key, "Gitnotes-Purge") && strings.TrimSpace(t.Value) == purge {
					marked = id
				}
			}
		}
	})
	if err != nil {
		return err
	}
	if marked.IsZero() {
		return fmt.Errorf("notebook %s has no revision with the trailer Gitnotes-Purge: %s", notebook, purge)
	}
	flag := Purge{Purge: purge, Head: marked, Replaces: slices.Clone(replaced)}
	if !old.IsZero() && old != last {
		dropped, err := droppedCommits(ctx, path, old, last)
		if err != nil {
			// A damaged old export still names its branch commit.
			e.opts.Logf("git export rebuild of notebook %s: listing the replaced commits: %v", notebook, err)
			dropped = []gitobj.ID{old}
		}
		flag.Replaces = append(flag.Replaces, dropped...)
	}
	unlock, err := lockPurge(ctx, path)
	if err != nil {
		return err
	}
	previous, ok, err := ReadPurge(path)
	if ok {
		flag.Replaces = append(flag.Replaces, previous.Replaces...)
	}
	if err == nil {
		err = writePurge(path, flag)
	}
	unlock()
	if err != nil {
		return err
	}
	if err := writeRef(path, last); err != nil {
		return err
	}
	if err := pruneRefs(path); err != nil {
		return err
	}
	// The repack keeps only what the branch reaches, with deltas, and prune
	// deletes the loose objects it does not. Refs stay loose for readRef's
	// reader.
	if err := repackDeltas(ctx, path); err != nil {
		return err
	}
	if _, err := runGit(ctx, path, repackTimeout, 1<<20, "prune", "--expire=now"); err != nil {
		return err
	}
	if err := removeTemporaries(path); err != nil {
		return err
	}
	out, err := runGit(ctx, path, fsckTimeout, 1<<20, "fsck", "--strict", "--unreachable", "--no-reflogs", "--no-progress")
	if err != nil {
		return err
	}
	if strings.Contains(out, "unreachable ") {
		return fmt.Errorf("notebook %s: the rebuilt export still holds unreachable objects", notebook)
	}
	r.pending = false
	r.maintained = time.Now()
	return nil
}

// droppedCommits lists the commits old reaches that last does not: those the
// rewrite removed from the export.
func droppedCommits(ctx context.Context, path string, old, last gitobj.ID) ([]gitobj.ID, error) {
	var out []gitobj.ID
	err := streamGit(ctx, path, fsckTimeout, func(line string) error {
		id, err := gitobj.ParseID(line)
		out = append(out, id)
		return err
	}, "rev-list", old.String(), "--not", last.String())
	return out, err
}
