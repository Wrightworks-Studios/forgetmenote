package notestore

import (
	"container/list"
	"context"
	"fmt"
	"maps"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"forgetmenote/internal/gitobj"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func toID(raw []byte) gitobj.ID {
	var id gitobj.ID
	copy(id[:], raw)
	return id
}

func idOrNil(id gitobj.ID) []byte {
	if id.IsZero() {
		return nil
	}
	return id[:]
}

// A model is a notebook's head in memory: every file, and the entries of
// every directory's tree, so a write re-hashes only the directories along its
// paths. It is never changed once installed.
type model struct {
	head    Head
	inc     int64 // the notebook's incarnation
	files   map[string]File
	paths   []string          // sorted
	byEntry map[string]string // entry ID to path
	dirs    map[string]*dirNode
	bytes   int64
}

type dirNode struct {
	id      gitobj.ID
	entries []gitobj.TreeEntry // in Git order
}

func (m *model) root() gitobj.ID { return m.dirs[""].id }

// at reports whether m is of the stored head of incarnation inc.
func (m *model) at(head Head, inc int64) bool { return m.head == head && m.inc == inc }

// newer reports whether m is of a later moment than o: a later incarnation,
// or a later revision of the same one.
func (m *model) newer(o *model) bool {
	return m.inc > o.inc || m.inc == o.inc && m.head.Seq > o.head.Seq
}

func emptyModel(head Head, inc int64) *model {
	m := &model{head: head, inc: inc, files: map[string]File{}, byEntry: map[string]string{}, dirs: map[string]*dirNode{"": {id: gitobj.EmptyTree}}}
	m.bytes = m.size()
	return m
}

// size estimates the memory m holds; derive keeps the same account.
func (m *model) size() int64 {
	n := int64(256)
	for p, f := range m.files {
		n += fileBytes(p, f)
	}
	for p, d := range m.dirs {
		n += dirBytes(p, d)
	}
	return n
}

func fileBytes(p string, f File) int64 { return int64(2*len(p)+2*len(f.EntryID)) + 160 }

func dirBytes(p string, d *dirNode) int64 {
	n := int64(len(p)) + 64
	for _, e := range d.entries {
		n += int64(len(e.Name)) + 48
	}
	return n
}

func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func parentDir(p string) string {
	if d := path.Dir(p); d != "." {
		return d
	}
	return ""
}

type atomicModel = atomic.Pointer[model]

// modelList bounds the read models of every notebook together.
type modelList struct {
	mu           sync.Mutex
	limit, bytes int64
	order        list.List // of *Notebook, most recently used first
	elems        map[*Notebook]*list.Element
}

// install makes m n's model unless the installed one is newer, and returns
// the model now installed. Callers hold n.loadMu.
func (n *Notebook) install(m *model) *model {
	l := &n.s.models
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.elems == nil {
		l.elems = map[*Notebook]*list.Element{}
	}
	// A local write that committed before a refresh read another gateway's
	// later revision must not put the model back a revision; a model of the
	// notebook created again replaces one of the deleted notebook, whatever
	// their seqs.
	if old := n.model.Load(); old != nil && old.newer(m) {
		return old
	}
	if old := n.model.Swap(m); old != nil {
		l.bytes -= old.bytes
	}
	l.bytes += m.bytes
	if e, ok := l.elems[n]; ok {
		l.order.MoveToFront(e)
	} else {
		l.elems[n] = l.order.PushFront(n)
	}
	for l.bytes > l.limit {
		e := l.order.Back()
		victim := e.Value.(*Notebook)
		if victim == n {
			break
		}
		l.order.Remove(e)
		delete(l.elems, victim)
		if old := victim.model.Swap(nil); old != nil {
			l.bytes -= old.bytes
		}
	}
	return m
}

func (n *Notebook) drop() {
	l := &n.s.models
	l.mu.Lock()
	defer l.mu.Unlock()
	if old := n.model.Swap(nil); old != nil {
		l.bytes -= old.bytes
	}
	if e, ok := l.elems[n]; ok {
		l.order.Remove(e)
		delete(l.elems, n)
	}
}

// current returns the read model, building it on first use.
func (n *Notebook) current(ctx context.Context) (*model, error) {
	if m := n.model.Load(); m != nil {
		l := &n.s.models
		l.mu.Lock()
		if e, ok := l.elems[n]; ok {
			l.order.MoveToFront(e)
		}
		l.mu.Unlock()
		return m, nil
	}
	n.loadMu.Lock()
	defer n.loadMu.Unlock()
	if m := n.model.Load(); m != nil {
		return m, nil
	}
	var m *model
	err := n.s.snapshot(ctx, func(tx pgx.Tx) (err error) {
		m, err = n.s.loadModel(ctx, tx, n.id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return n.install(m), nil
}

// snapshot runs fn in a read-only transaction that sees the database at one
// moment, so a model built from several queries is one revision even while
// writers commit.
func (s *Store) snapshot(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, s.db.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, fn)
}

// loadModel builds notebook's read model from its head, entries and trees. q
// must see one moment: a snapshot, or a transaction holding the head lock.
func (s *Store) loadModel(ctx context.Context, q querier, notebook string) (*model, error) {
	head, inc, err := readHead(ctx, q, s.db.Org, notebook)
	if err != nil {
		return nil, err
	}
	var tree []byte
	if err = q.QueryRow(ctx, `SELECT tree_sha FROM revisions WHERE org = $1 AND notebook = $2 AND seq = $3`, s.db.Org, notebook, head.Seq).Scan(&tree); err != nil {
		return nil, fmt.Errorf("notebook %s head revision: %w", notebook, err)
	}
	dirs, err := s.walkTrees(ctx, q, notebook, toID(tree))
	if err != nil {
		return nil, err
	}
	m := &model{head: head, inc: inc, files: map[string]File{}, byEntry: map[string]string{}, dirs: dirs}
	rows, err := q.Query(ctx, `SELECT path, entry_id, blob_sha, mode, size, version FROM entries WHERE org = $1 AND notebook = $2`, s.db.Org, notebook)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var f File
		var blob, version []byte
		var mode int32
		if err := rows.Scan(&f.Path, &f.EntryID, &blob, &mode, &f.Size, &version); err != nil {
			rows.Close()
			return nil, err
		}
		f.Blob, f.Mode, f.Version = toID(blob), uint32(mode), toID(version)
		m.files[f.Path] = f
		m.byEntry[f.EntryID] = f.Path
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	count := 0
	for dir, node := range dirs {
		for _, e := range node.entries {
			if e.IsTree() {
				continue
			}
			count++
			if f, ok := m.files[joinPath(dir, e.Name)]; !ok || f.Blob != e.ID || f.Mode != e.Mode {
				return nil, fmt.Errorf("notebook %s: entries disagree with tree at %s", notebook, joinPath(dir, e.Name))
			}
		}
	}
	if count != len(m.files) {
		return nil, fmt.Errorf("notebook %s: %d entries for %d files in its tree", notebook, len(m.files), count)
	}
	m.paths = slices.Sorted(maps.Keys(m.files))
	m.bytes = m.size()
	return m, nil
}

// walkTrees reads every directory of the tree root, one query per level.
func (s *Store) walkTrees(ctx context.Context, q querier, notebook string, root gitobj.ID) (map[string]*dirNode, error) {
	dirs := map[string]*dirNode{}
	level := map[gitobj.ID][]string{root: {""}}
	for len(level) > 0 {
		trees, err := s.treeEntries(ctx, q, notebook, slices.Collect(maps.Keys(level)))
		if err != nil {
			return nil, err
		}
		next := map[gitobj.ID][]string{}
		for id, paths := range level {
			for _, p := range paths {
				dirs[p] = &dirNode{id: id, entries: trees[id]}
				for _, e := range trees[id] {
					if e.IsTree() {
						next[e.ID] = append(next[e.ID], joinPath(p, e.Name))
					}
				}
			}
		}
		level = next
	}
	return dirs, nil
}

// treeEntries returns the entries of trees ids, from the cache or git_trees.
func (s *Store) treeEntries(ctx context.Context, q querier, notebook string, ids []gitobj.ID) (map[gitobj.ID][]gitobj.TreeEntry, error) {
	out, missing, err := s.findTrees(ctx, q, notebook, ids)
	if err == nil && len(missing) > 0 {
		err = fmt.Errorf("notebook %s: tree %s is missing", notebook, missing[0])
	}
	return out, err
}

// findTrees returns the entries of the trees among ids that the cache or
// notebook's git_trees has, and the others. The cache is shared by every
// notebook, so a tree found there may be another notebook's.
func (s *Store) findTrees(ctx context.Context, q querier, notebook string, ids []gitobj.ID) (map[gitobj.ID][]gitobj.TreeEntry, []gitobj.ID, error) {
	out := map[gitobj.ID][]gitobj.TreeEntry{}
	var missing [][]byte
	for _, id := range ids {
		if id == gitobj.EmptyTree {
			out[id] = nil
		} else if entries, ok := s.trees.get(id); ok {
			out[id] = entries
		} else {
			missing = append(missing, id[:])
		}
	}
	if len(missing) == 0 {
		return out, nil, nil
	}
	rows, err := q.Query(ctx, `SELECT tree_sha, body FROM git_trees WHERE org = $1 AND notebook = $2 AND tree_sha = ANY($3)`, s.db.Org, notebook, missing)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sha, body []byte
		if err := rows.Scan(&sha, &body); err != nil {
			return nil, nil, err
		}
		id := toID(sha)
		if gitobj.HashObject("tree", body) != id {
			return nil, nil, fmt.Errorf("notebook %s: tree %s is corrupt", notebook, id)
		}
		entries, err := gitobj.ParseTree(body)
		if err != nil {
			return nil, nil, fmt.Errorf("notebook %s: tree %s: %w", notebook, id, err)
		}
		out[id] = entries
		s.trees.put(id, entries, int64(len(body))+int64(len(entries))*48)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var absent []gitobj.ID
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			absent = append(absent, id)
		}
	}
	return out, absent, nil
}

// lookupTree finds the entry at p in the tree root, or reports it absent.
func (s *Store) lookupTree(ctx context.Context, notebook string, root gitobj.ID, p string) (gitobj.TreeEntry, bool, error) {
	entry := gitobj.TreeEntry{Mode: gitobj.ModeTree, ID: root}
	for _, name := range splitPath(p) {
		if !entry.IsTree() {
			return entry, false, nil
		}
		trees, err := s.treeEntries(ctx, s.db.Pool, notebook, []gitobj.ID{entry.ID})
		if err != nil {
			return entry, false, err
		}
		found := false
		for _, e := range trees[entry.ID] {
			if e.Name == name {
				entry, found = e, true
				break
			}
		}
		if !found {
			return entry, false, nil
		}
	}
	return entry, true, nil
}

// listTree returns every non-directory entry under root by path.
func (s *Store) listTree(ctx context.Context, q querier, notebook string, root gitobj.ID) (map[string]gitobj.TreeEntry, error) {
	dirs, err := s.walkTrees(ctx, q, notebook, root)
	if err != nil {
		return nil, err
	}
	files := map[string]gitobj.TreeEntry{}
	for dir, node := range dirs {
		for _, e := range node.entries {
			if !e.IsTree() {
				files[joinPath(dir, e.Name)] = e
			}
		}
	}
	return files, nil
}

func splitPath(p string) []string {
	var parts []string
	for p != "" {
		i := 0
		for i < len(p) && p[i] != '/' {
			i++
		}
		parts = append(parts, p[:i])
		if i == len(p) {
			break
		}
		p = p[i+1:]
	}
	return parts
}

// validPath reports whether a revision can write a file at p: every name
// in it is one Git accepts in a tree.
func validPath(p string) bool { return pathOf(p, writableName) }

// writableName is the rule for the names a revision writes. A test swaps in
// the rule of an earlier build, which accepted names git fsck refuses, to
// make the notebooks it wrote.
var writableName = gitobj.ValidName

// storedPath reports whether p can name a file a revision deletes or
// renames away, which must exist. An earlier build stored names git fsck
// refuses, such as git~1, and a notebook holding one can still move the
// file elsewhere or delete it.
func storedPath(p string) bool {
	return pathOf(p, func(name string) bool {
		return name != "" && name != "." && name != ".." && !strings.ContainsRune(name, 0)
	})
}

func pathOf(p string, valid func(string) bool) bool {
	if p == "" || len(p) > 4096 {
		return false
	}
	for _, name := range splitPath(p) {
		if !valid(name) {
			return false
		}
	}
	return p[len(p)-1] != '/'
}

// sortedFiles returns m's files in path order.
func (m *model) sortedFiles() []File {
	out := make([]File, len(m.paths))
	for i, p := range m.paths {
		out[i] = m.files[p]
	}
	return out
}

// derive returns the model after a write that set the files in changed
// (a nil value removes the path) and replaced the directories in dirs.
func (m *model) derive(head Head, changed map[string]*File, dirs map[string]*dirNode) *model {
	next := &model{head: head, inc: m.inc, files: maps.Clone(m.files), byEntry: maps.Clone(m.byEntry), dirs: maps.Clone(m.dirs), bytes: m.bytes}
	reshaped := false
	for p, f := range changed {
		if old, ok := m.files[p]; ok {
			next.bytes -= fileBytes(p, old)
		}
		if f != nil {
			next.bytes += fileBytes(p, *f)
		}
	}
	for p, d := range dirs {
		if old, ok := m.dirs[p]; ok {
			next.bytes -= dirBytes(p, old)
		}
		if d != nil {
			next.bytes += dirBytes(p, d)
		}
	}
	for p, f := range changed {
		if old, ok := next.files[p]; ok {
			if next.byEntry[old.EntryID] == p {
				delete(next.byEntry, old.EntryID)
			}
			if f == nil {
				delete(next.files, p)
				reshaped = true
			}
		} else if f != nil {
			reshaped = true
		}
	}
	for p, f := range changed {
		if f != nil {
			next.files[p] = *f
			next.byEntry[f.EntryID] = p
		}
	}
	for p, d := range dirs {
		if d == nil {
			delete(next.dirs, p)
		} else {
			next.dirs[p] = d
		}
	}
	if reshaped {
		next.paths = slices.Sorted(maps.Keys(next.files))
	} else {
		next.paths = m.paths
	}
	return next
}

func sortEntries(entries []gitobj.TreeEntry) {
	sort.Slice(entries, func(i, j int) bool { return gitobj.CompareEntries(entries[i], entries[j]) < 0 })
}
