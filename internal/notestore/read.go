package notestore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"slices"
	"strings"

	"forgetmenote/internal/gitobj"

	"github.com/jackc/pgx/v5"
)

// Head returns the notebook's newest revision.
func (n *Notebook) Head(ctx context.Context) (Head, error) {
	m, err := n.current(ctx)
	if err != nil {
		return Head{}, err
	}
	return m.head, nil
}

// Incarnation returns the incarnation of the notebook's history this
// gateway reads. A purge, which rewrites the history, starts a new one, so
// what a revision held is the same while the incarnation is.
func (n *Notebook) Incarnation(ctx context.Context) (int64, error) {
	m, err := n.current(ctx)
	if err != nil {
		return 0, err
	}
	return m.inc, nil
}

type revision struct {
	seq  int64
	id   gitobj.ID
	tree gitobj.ID
}

// resolve finds rev, the zero ID meaning the head; at the head it also
// returns the read model.
func (n *Notebook) resolve(ctx context.Context, rev gitobj.ID) (*model, revision, error) {
	m, err := n.current(ctx)
	if err != nil {
		return nil, revision{}, err
	}
	if rev.IsZero() || rev == m.head.Commit {
		return m, revision{seq: m.head.Seq, id: m.head.Commit, tree: m.root()}, nil
	}
	r := revision{id: rev}
	var tree []byte
	err = n.s.db.Pool.QueryRow(ctx, `SELECT seq, tree_sha FROM revisions WHERE org = $1 AND notebook = $2 AND commit_sha = $3`, n.s.db.Org, n.id, rev[:]).Scan(&r.seq, &tree)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, r, fmt.Errorf("revision %s: %w", rev, fs.ErrNotExist)
	}
	r.tree = toID(tree)
	return nil, r, err
}

type fileAt struct {
	entry   string
	size    int64
	version gitobj.ID
}

// versionsAt returns, for each of paths at revision seq, the entry, size and
// Version of the newest change on seq's first-parent line that put a file
// there. Every query here reads one table by key, so even a plan made without
// statistics, as for a notebook just imported, is one pass over the
// notebook's rows.
func (n *Notebook) versionsAt(ctx context.Context, q querier, seq int64, paths []string) (map[string]fileAt, error) {
	out := map[string]fileAt{}
	if len(paths) == 0 {
		return out, nil
	}
	line, err := n.firstParentLine(ctx, q, seq)
	if err != nil {
		return nil, err
	}
	query := `SELECT DISTINCT ON (path) path, entry_id, size, seq FROM revision_changes
		WHERE org = $1 AND notebook = $2 AND path = ANY($3) AND seq <= $4 AND op <> 'delete'`
	args := []any{n.s.db.Org, n.id, paths, seq}
	if line != "" {
		query += ` AND seq <@ $5::text::int8multirange`
		args = append(args, line)
	}
	rows, err := q.Query(ctx, query+` ORDER BY path, seq DESC`, args...)
	if err != nil {
		return nil, err
	}
	seqs := map[string]int64{}
	for rows.Next() {
		var p string
		var f fileAt
		var at int64
		if err := rows.Scan(&p, &f.entry, &f.size, &at); err != nil {
			rows.Close()
			return nil, err
		}
		out[p], seqs[p] = f, at
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	commits, err := n.commitIDs(ctx, q, slices.Collect(maps.Values(seqs)))
	if err != nil {
		return nil, err
	}
	for p, at := range seqs {
		f := out[p]
		f.version = commits[at]
		out[p] = f
	}
	return out, nil
}

// firstParentLine returns the revisions on seq's first-parent line, whose
// changes make its files, as a multirange of seqs; or "" when that is every
// revision up to seq, as in every notebook FMN wrote. It reads only the
// branch points: roots, and revisions whose first parent is not the revision
// before them, as an imported branch has.
func (n *Notebook) firstParentLine(ctx context.Context, q querier, seq int64) (string, error) {
	rows, err := q.Query(ctx, `SELECT seq, parent_seq FROM revisions WHERE org = $1 AND notebook = $2 AND seq <= $3 AND parent_seq IS DISTINCT FROM seq - 1 ORDER BY seq DESC`,
		n.s.db.Org, n.id, seq)
	if err != nil {
		return "", err
	}
	type point struct {
		seq    int64
		parent *int64
	}
	points, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (point, error) {
		var p point
		return p, row.Scan(&p.seq, &p.parent)
	})
	if err != nil {
		return "", err
	}
	var ranges []string
	low := int64(1)
	for cur, i := seq, 0; ; {
		for i < len(points) && points[i].seq > cur {
			i++
		}
		if i == len(points) {
			ranges = append(ranges, fmt.Sprintf("[1,%d]", cur))
			break
		}
		// Every revision from this branch point to cur follows the one
		// before it.
		p := points[i]
		ranges, low = append(ranges, fmt.Sprintf("[%d,%d]", p.seq, cur)), p.seq
		if p.parent == nil {
			break
		}
		cur = *p.parent
	}
	if len(ranges) == 1 && low == 1 {
		return "", nil
	}
	return "{" + strings.Join(ranges, ",") + "}", nil
}

func (n *Notebook) commitIDs(ctx context.Context, q querier, seqs []int64) (map[int64]gitobj.ID, error) {
	out := map[int64]gitobj.ID{}
	if len(seqs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT seq, commit_sha FROM revisions WHERE org = $1 AND notebook = $2 AND seq = ANY($3)`, n.s.db.Org, n.id, seqs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var sha []byte
		if err := rows.Scan(&seq, &sha); err != nil {
			return nil, err
		}
		out[seq] = toID(sha)
	}
	return out, rows.Err()
}

// Files returns every file at rev, the zero ID meaning the head, by path.
func (n *Notebook) Files(ctx context.Context, rev gitobj.ID) ([]File, error) {
	m, r, err := n.resolve(ctx, rev)
	if err != nil {
		return nil, err
	}
	if m != nil {
		return m.sortedFiles(), nil
	}
	tree, err := n.s.listTree(ctx, n.s.db.Pool, n.id, r.tree)
	if err != nil {
		return nil, err
	}
	paths := slices.Sorted(maps.Keys(tree))
	at, err := n.versionsAt(ctx, n.s.db.Pool, r.seq, paths)
	if err != nil {
		return nil, err
	}
	files := make([]File, len(paths))
	for i, p := range paths {
		e, f := tree[p], at[p]
		files[i] = File{Path: p, EntryID: f.entry, Blob: e.ID, Mode: e.Mode, Size: f.size, Version: f.version}
	}
	return files, nil
}

// HasUnder reports whether a file at rev, the zero ID meaning the head, lies
// under the folder dir. At the head it searches the read model's paths,
// which it does not copy, so a caller may ask for many folders.
func (n *Notebook) HasUnder(ctx context.Context, rev gitobj.ID, dir string) (bool, error) {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	m, r, err := n.resolve(ctx, rev)
	if err != nil {
		return false, err
	}
	if m != nil {
		i, _ := slices.BinarySearch(m.paths, prefix)
		return i < len(m.paths) && strings.HasPrefix(m.paths[i], prefix), nil
	}
	tree, err := n.s.listTree(ctx, n.s.db.Pool, n.id, r.tree)
	if err != nil {
		return false, err
	}
	for p := range tree {
		if strings.HasPrefix(p, prefix) {
			return true, nil
		}
	}
	return false, nil
}

// Stat returns the file at path in rev, or an fs.ErrNotExist error.
func (n *Notebook) Stat(ctx context.Context, rev gitobj.ID, path string) (File, error) {
	m, r, err := n.resolve(ctx, rev)
	if err != nil {
		return File{}, err
	}
	if m != nil {
		if f, ok := m.files[path]; ok {
			return f, nil
		}
		return File{}, fmt.Errorf("%s: %w", path, fs.ErrNotExist)
	}
	e, ok, err := n.s.lookupTree(ctx, n.id, r.tree, path)
	if err != nil {
		return File{}, err
	}
	if !ok || e.IsTree() {
		return File{}, fmt.Errorf("%s at %s: %w", path, rev, fs.ErrNotExist)
	}
	at, err := n.versionsAt(ctx, n.s.db.Pool, r.seq, []string{path})
	if err != nil {
		return File{}, err
	}
	f := at[path]
	return File{Path: path, EntryID: f.entry, Blob: e.ID, Mode: e.Mode, Size: f.size, Version: f.version}, nil
}

// Read returns the content of path at rev and its file.
func (n *Notebook) Read(ctx context.Context, rev gitobj.ID, path string) ([]byte, File, error) {
	f, err := n.Stat(ctx, rev, path)
	if err != nil {
		return nil, f, err
	}
	if f.Mode == gitobj.ModeGitlink {
		return nil, f, invalid("%s is a submodule, not a file", path)
	}
	content, err := n.s.readContent(ctx, n.s.db.Pool, f.Blob)
	return content, f, err
}

// uses reports whether the notebook has ever held blob, so one notebook's
// reader cannot read another's content by its ID.
func (n *Notebook) uses(ctx context.Context, blob gitobj.ID) error {
	if m, err := n.current(ctx); err != nil {
		return err
	} else {
		for _, f := range m.files {
			if f.Blob == blob {
				return nil
			}
		}
	}
	var found bool
	err := n.s.db.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT FROM revision_changes WHERE org = $1 AND blob_sha = $2 AND notebook = $3)`, n.s.db.Org, blob[:], n.id).Scan(&found)
	if err == nil && !found {
		err = fmt.Errorf("content %s: %w", blob, fs.ErrNotExist)
	}
	return err
}

// ReadBlob returns the content of a blob the notebook holds or held.
func (n *Notebook) ReadBlob(ctx context.Context, blob gitobj.ID) ([]byte, error) {
	if err := n.uses(ctx, blob); err != nil {
		return nil, err
	}
	return n.s.readContent(ctx, n.s.db.Pool, blob)
}

// ReadRange returns length bytes of blob from off, reading only the blocks
// that hold them.
func (n *Notebook) ReadRange(ctx context.Context, blob gitobj.ID, off, length int64) ([]byte, error) {
	if err := n.uses(ctx, blob); err != nil {
		return nil, err
	}
	return n.s.readRange(ctx, blob, off, length)
}

// Commit returns the revision with commit id.
func (n *Notebook) Commit(ctx context.Context, id gitobj.ID) (CommitInfo, error) {
	infos, err := n.commits(ctx, `SELECT seq FROM revisions WHERE org = $1 AND notebook = $2 AND commit_sha = $3`, false, id[:])
	if err != nil {
		return CommitInfo{}, err
	}
	if len(infos) == 0 {
		return CommitInfo{}, fmt.Errorf("revision %s: %w", id, fs.ErrNotExist)
	}
	return infos[0], nil
}

// A HistoryQuery selects revisions, newest first: those that changed one
// entry, or with neither EntryID nor Path, every revision of the notebook or
// those Prefix and MessageContains select.
type HistoryQuery struct {
	// EntryID names the entry; or Path names the entry at that path at the
	// head, or the last one there, and its renames are followed.
	EntryID, Path string
	// Prefix keeps the revisions that changed a file at or under this path,
	// as "git log -- prefix" does, renames from there included.
	Prefix string
	// MessageContains keeps the revisions with a message line containing
	// this text, as "git log --fixed-strings --grep" does.
	MessageContains string
	// Before keeps only revisions before this seq; 0 keeps all.
	Before int64
	// Limit caps the revisions; 0 returns all.
	Limit int
}

// History returns the revisions q selects, newest first.
func (n *Notebook) History(ctx context.Context, q HistoryQuery) ([]CommitInfo, error) {
	if q.EntryID == "" && q.Path == "" {
		return n.historyRange(ctx, q)
	}
	if q.Prefix != "" || q.MessageContains != "" {
		return nil, invalid("a history of one entry takes no prefix or message search")
	}
	entry := q.EntryID
	before := q.Before
	if before <= 0 {
		before = math.MaxInt64
	}
	if entry == "" {
		m, err := n.current(ctx)
		if err != nil {
			return nil, err
		}
		if f, ok := m.files[q.Path]; ok && q.Before == 0 {
			entry = f.EntryID
		} else {
			err := n.s.db.Pool.QueryRow(ctx, `SELECT entry_id FROM revision_changes WHERE org = $1 AND notebook = $2 AND path = $3 AND seq < $4
				ORDER BY seq DESC, op = 'delete' LIMIT 1`, n.s.db.Org, n.id, q.Path, before).Scan(&entry)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
		}
	}
	var limit any
	if q.Limit > 0 {
		limit = int64(q.Limit)
	}
	return n.commits(ctx, `SELECT DISTINCT seq FROM revision_changes WHERE org = $1 AND notebook = $2 AND entry_id = $3 AND seq < $4 ORDER BY seq DESC LIMIT $5`,
		true, entry, before, limit)
}

// Revisions returns up to limit revisions after afterSeq, oldest first.
func (n *Notebook) Revisions(ctx context.Context, afterSeq int64, limit int) ([]CommitInfo, error) {
	if limit <= 0 {
		limit = 1000
	}
	return n.commits(ctx, `SELECT seq FROM revisions WHERE org = $1 AND notebook = $2 AND seq > $3 ORDER BY seq LIMIT $4`, false, afterSeq, int64(limit))
}

// commits loads the revisions whose seqs a query selects, newest first when
// newest is set, with their changes. $1 and $2 are the org and notebook. Like
// versionsAt, it joins nothing, so no plan can rescan the notebook per row.
func (n *Notebook) commits(ctx context.Context, seqQuery string, newest bool, args ...any) ([]CommitInfo, error) {
	rows, err := n.s.db.Pool.Query(ctx, seqQuery, append([]any{n.s.db.Org, n.id}, args...)...)
	if err != nil {
		return nil, err
	}
	seqs, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil || len(seqs) == 0 {
		return nil, err
	}
	rows, err = n.s.db.Pool.Query(ctx, `SELECT seq, commit_sha, commit_body FROM revisions WHERE org = $1 AND notebook = $2 AND seq = ANY($3)`, n.s.db.Org, n.id, seqs)
	if err != nil {
		return nil, err
	}
	var infos []CommitInfo
	for rows.Next() {
		var info CommitInfo
		var sha, body []byte
		if err := rows.Scan(&info.Seq, &sha, &body); err != nil {
			rows.Close()
			return nil, err
		}
		info.ID = toID(sha)
		if info.Commit, err = gitobj.ParseCommit(body); err != nil {
			rows.Close()
			return nil, fmt.Errorf("revision %s: %w", info.ID, err)
		}
		info.Trailers = gitobj.Trailers(info.Commit.Message)
		infos = append(infos, info)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(infos) == 0 {
		return infos, err
	}
	slices.SortFunc(infos, func(a, b CommitInfo) int {
		if newest {
			return cmp.Compare(b.Seq, a.Seq)
		}
		return cmp.Compare(a.Seq, b.Seq)
	})
	index := map[int64]int{}
	for i, info := range infos {
		index[info.Seq] = i
	}
	rows, err = n.s.db.Pool.Query(ctx, `SELECT seq, op, path, coalesce(old_path, ''), entry_id, blob_sha, mode, size FROM revision_changes
		WHERE org = $1 AND notebook = $2 AND seq = ANY($3) ORDER BY seq, path, op`, n.s.db.Org, n.id, seqs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var c Change
		var blob []byte
		var mode int32
		if err := rows.Scan(&seq, &c.Op, &c.Path, &c.OldPath, &c.EntryID, &blob, &mode, &c.Size); err != nil {
			return nil, err
		}
		c.Blob, c.Mode = toID(blob), uint32(mode)
		info := &infos[index[seq]]
		info.Changes = append(info.Changes, c)
	}
	return infos, rows.Err()
}

type treeDiff struct {
	path     string
	old, new *gitobj.TreeEntry
}

// diffTrees lists the files that differ between trees a and b, skipping
// subtrees they share.
func diffTrees(ctx context.Context, get func(context.Context, []gitobj.ID) (map[gitobj.ID][]gitobj.TreeEntry, error), a, b gitobj.ID, prefix string, out *[]treeDiff) error {
	if a == b {
		return nil
	}
	var ids []gitobj.ID
	for _, id := range []gitobj.ID{a, b} {
		if !id.IsZero() {
			ids = append(ids, id)
		}
	}
	trees, err := get(ctx, ids)
	if err != nil {
		return err
	}
	byName := map[string][2]*gitobj.TreeEntry{}
	for side, id := range []gitobj.ID{a, b} {
		if id.IsZero() {
			continue
		}
		for _, e := range trees[id] {
			pair := byName[e.Name]
			pair[side] = &e
			byName[e.Name] = pair
		}
	}
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		pair := byName[name]
		old, new := pair[0], pair[1]
		p := joinPath(prefix, name)
		var oldTree, newTree gitobj.ID
		if old != nil && old.IsTree() {
			oldTree, old = old.ID, nil
		}
		if new != nil && new.IsTree() {
			newTree, new = new.ID, nil
		}
		if !oldTree.IsZero() || !newTree.IsZero() {
			if err := diffTrees(ctx, get, oldTree, newTree, p, out); err != nil {
				return err
			}
		}
		if (old != nil || new != nil) && (old == nil || new == nil || *old != *new) {
			*out = append(*out, treeDiff{path: p, old: old, new: new})
		}
	}
	return nil
}

func (n *Notebook) treeGetter(q querier) func(context.Context, []gitobj.ID) (map[gitobj.ID][]gitobj.TreeEntry, error) {
	return func(ctx context.Context, ids []gitobj.ID) (map[gitobj.ID][]gitobj.TreeEntry, error) {
		return n.s.treeEntries(ctx, q, n.id, ids)
	}
}

// Changes returns what changed between revisions from and to, by entry, as
// one change per entry. The zero from is the empty notebook and the zero to
// the head.
func (n *Notebook) Changes(ctx context.Context, from, to gitobj.ID) ([]Change, error) {
	m, newer, err := n.resolve(ctx, to)
	if err != nil {
		return nil, err
	}
	older := revision{}
	if !from.IsZero() {
		if _, older, err = n.resolve(ctx, from); err != nil {
			return nil, err
		}
	}
	var diff []treeDiff
	if err := diffTrees(ctx, n.treeGetter(n.s.db.Pool), older.tree, newer.tree, "", &diff); err != nil {
		return nil, err
	}
	var oldPaths, newPaths []string
	for _, d := range diff {
		if d.old != nil {
			oldPaths = append(oldPaths, d.path)
		}
		if d.new != nil {
			newPaths = append(newPaths, d.path)
		}
	}
	before, err := n.versionsAt(ctx, n.s.db.Pool, older.seq, oldPaths)
	if err != nil {
		return nil, err
	}
	after := map[string]fileAt{}
	if m != nil {
		for _, p := range newPaths {
			f := m.files[p]
			after[p] = fileAt{entry: f.EntryID, size: f.Size, version: f.Version}
		}
	} else if after, err = n.versionsAt(ctx, n.s.db.Pool, newer.seq, newPaths); err != nil {
		return nil, err
	}
	type side struct {
		path  string
		entry gitobj.TreeEntry
		size  int64
	}
	was, is := map[string]side{}, map[string]side{}
	for _, d := range diff {
		if d.old != nil {
			was[before[d.path].entry] = side{d.path, *d.old, before[d.path].size}
		}
		if d.new != nil {
			is[after[d.path].entry] = side{d.path, *d.new, after[d.path].size}
		}
	}
	var changes []Change
	for entry := range unionKeys(was, is) {
		b, hadB := was[entry]
		a, hasA := is[entry]
		c := Change{EntryID: entry, Path: a.path, Blob: a.entry.ID, Mode: a.entry.Mode, Size: a.size}
		switch {
		case !hadB:
			c.Op = OpAdd
		case !hasA:
			c = Change{Op: OpDelete, EntryID: entry, Path: b.path, Mode: b.entry.Mode}
		case a.path != b.path:
			c.Op, c.OldPath = OpRename, b.path
		default:
			c.Op = OpModify
		}
		changes = append(changes, c)
	}
	slices.SortFunc(changes, func(x, y Change) int {
		if c := strings.Compare(x.Path, y.Path); c != 0 {
			return c
		}
		return strings.Compare(x.Op, y.Op)
	})
	return changes, nil
}
