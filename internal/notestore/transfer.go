package notestore

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"

	"forgetmenote/internal/gitobj"

	"github.com/jackc/pgx/v5"
)

// ImportCommit appends the Git commit raw as the notebook's next revision and
// head, keeping the commit and its trees byte for byte. The commit's parents
// must already be imported, so commits arrive in topological order; the first
// imported commit, a root, creates the notebook. trees holds the bodies of
// trees the store does not have yet, and blob returns a blob's content; every
// object is checked against its ID. Changes are computed against the first
// parent, so a merge keeps all its parents and records what it changed on the
// first-parent line. Importing a commit again returns the head unchanged.
//
// renamed, if given, maps paths the commit adds to the paths of files it
// deletes that Git's rename detection pairs them with (--find-renames against
// the first parent). A file added at such a path keeps the deleted file's
// entry, as Git's history follows it, unless its content declares another
// identity; without a pair, only an identical deleted file passes its entry on.
func (n *Notebook) ImportCommit(ctx context.Context, raw []byte, trees map[gitobj.ID][]byte, blob func(gitobj.ID) ([]byte, error), renamed ...map[string]string) (Head, error) {
	renames := map[string]string{}
	for _, m := range renamed {
		maps.Copy(renames, m)
	}
	id := gitobj.HashObject("commit", raw)
	c, err := gitobj.ParseCommit(raw)
	if err != nil {
		return Head{}, fmt.Errorf("%w: commit %s: %v", ErrInvalid, id, err)
	}
	if !notebookID.MatchString(n.id) {
		return Head{}, invalid("notebook ID %q", n.id)
	}
	s, org := n.s, n.s.db.Org
	n.write.Lock()
	defer n.write.Unlock()
	var head Head
	var next *model
	var stored []*version
	versions := map[gitobj.ID]*version{} // kept across attempts
	staged := map[[32]byte]bool{}
	attempt := func(tx pgx.Tx) error {
		head, next, stored = Head{}, nil, nil
		cur, inc, err := lockHead(ctx, tx, org, n.id)
		fresh := errors.Is(err, fs.ErrNotExist)
		if err != nil && !fresh {
			return err
		}
		if fresh {
			if len(c.Parents) > 0 {
				return invalid("the first imported commit %s has parents", id)
			}
			if inc, err = insertHead(ctx, tx, org, n.id, id); err != nil {
				return err
			}
		} else {
			var seq int64
			err := tx.QueryRow(ctx, `SELECT seq FROM revisions WHERE org = $1 AND notebook = $2 AND commit_sha = $3`, org, n.id, id[:]).Scan(&seq)
			if err == nil {
				head = cur
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		parents := map[gitobj.ID]revision{}
		if len(c.Parents) > 0 {
			keys := make([][]byte, len(c.Parents))
			for i, p := range c.Parents {
				keys[i] = bytesOf(p)
			}
			rows, err := tx.Query(ctx, `SELECT seq, commit_sha, tree_sha FROM revisions WHERE org = $1 AND notebook = $2 AND commit_sha = ANY($3)`, org, n.id, keys)
			if err != nil {
				return err
			}
			for rows.Next() {
				var r revision
				var sha, tree []byte
				if err := rows.Scan(&r.seq, &sha, &tree); err != nil {
					rows.Close()
					return err
				}
				r.id, r.tree = toID(sha), toID(tree)
				parents[r.id] = r
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, p := range c.Parents {
				if _, ok := parents[p]; !ok {
					return invalid("commit %s: parent %s is not imported", id, p)
				}
			}
		}
		first := revision{tree: gitobj.EmptyTree}
		if len(c.Parents) > 0 {
			first = parents[c.Parents[0]]
		}
		m := emptyModel(Head{}, inc)
		if !fresh {
			if m = n.model.Load(); m == nil || !m.at(cur, inc) {
				if m, err = s.loadModel(ctx, tx, n.id); err != nil {
					return err
				}
			}
		}
		linear := fresh || len(c.Parents) > 0 && c.Parents[0] == cur.Commit

		newTrees := map[gitobj.ID][]byte{}
		kept := map[gitobj.ID]bool{} // trees read from the store, not the import
		get := func(ctx context.Context, ids []gitobj.ID) (map[gitobj.ID][]gitobj.TreeEntry, error) {
			out := map[gitobj.ID][]gitobj.TreeEntry{}
			var rest []gitobj.ID
			for _, tid := range ids {
				body, ok := trees[tid]
				if !ok {
					rest = append(rest, tid)
					continue
				}
				if gitobj.HashObject("tree", body) != tid {
					return nil, invalid("tree %s does not match its ID", tid)
				}
				entries, err := gitobj.ParseTree(body)
				if err != nil {
					return nil, invalid("tree %s: %v", tid, err)
				}
				for _, e := range entries {
					if !gitobj.ValidName(e.Name) {
						return nil, invalid("tree %s has entry %q", tid, e.Name)
					}
				}
				out[tid], newTrees[tid] = entries, body
			}
			if len(rest) > 0 {
				found, missing, err := s.findTrees(ctx, tx, n.id, rest)
				if err != nil {
					return nil, err
				}
				if len(missing) > 0 {
					return nil, invalid("commit %s needs tree %s, which the import did not send", id, missing[0])
				}
				maps.Copy(out, found)
				for _, tid := range rest {
					kept[tid] = tid != gitobj.EmptyTree
				}
			}
			return out, nil
		}
		if c.Tree == gitobj.EmptyTree {
			newTrees[gitobj.EmptyTree] = []byte{}
		}
		var diff []treeDiff
		if err := diffTrees(ctx, get, first.tree, c.Tree, "", &diff); err != nil {
			return err
		}

		parentFiles := m.files
		if !linear {
			if parentFiles, err = n.filesAt(ctx, tx, first); err != nil {
				return err
			}
		}
		var needed [][]byte
		for _, d := range diff {
			if d.new != nil && d.new.Mode != gitobj.ModeGitlink {
				needed = append(needed, bytesOf(d.new.ID))
			}
		}
		sizes := map[gitobj.ID]int64{}
		if len(needed) > 0 {
			rows, err := tx.Query(ctx, `SELECT blob_sha, size FROM file_versions WHERE org = $1 AND blob_sha = ANY($2)`, org, needed)
			if err != nil {
				return err
			}
			for rows.Next() {
				var sha []byte
				var size int64
				if err := rows.Scan(&sha, &size); err != nil {
					rows.Close()
					return err
				}
				sizes[toID(sha)] = size
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
		content := func(d treeDiff) ([]byte, error) {
			if v := versions[d.new.ID]; v != nil {
				sizes[d.new.ID] = int64(len(v.data))
				return v.data, nil
			}
			if _, ok := sizes[d.new.ID]; ok {
				return s.readContent(ctx, tx, d.new.ID)
			}
			data, err := blob(d.new.ID)
			if err != nil {
				return nil, fmt.Errorf("blob %s: %w", d.new.ID, err)
			}
			if gitobj.BlobID(data) != d.new.ID {
				return nil, invalid("blob %s does not match its ID", d.new.ID)
			}
			versions[d.new.ID] = prepare(d.path, data)
			sizes[d.new.ID] = int64(len(data))
			return data, nil
		}
		for _, d := range diff {
			if d.new == nil || d.new.Mode == gitobj.ModeGitlink {
				continue
			}
			if _, stored := sizes[d.new.ID]; !stored {
				if _, err := content(d); err != nil {
					return err
				}
			}
		}

		// Entries: a path the first parent had keeps its entry; a new path
		// takes the identity its content declares, or the entry of the file
		// deleted in this commit that Git pairs it with, or of an identical
		// deleted file, or a new one.
		claimed := map[string]bool{}
		leaving := map[string]bool{}
		deleted := map[string]bool{}
		for _, d := range diff {
			if d.old != nil {
				leaving[parentFiles[d.path].EntryID] = true
				deleted[d.path] = d.new == nil
			}
		}
		for _, f := range parentFiles {
			claimed[f.EntryID] = true
		}
		for entry := range leaving {
			delete(claimed, entry)
		}
		assigned := map[string]string{}
		for _, d := range diff {
			if d.new != nil && d.old != nil {
				entry := parentFiles[d.path].EntryID
				assigned[d.path], claimed[entry] = entry, true
			}
		}
		for _, d := range diff {
			if d.new == nil || d.old != nil {
				continue
			}
			entry := ""
			if s.opts.ImportEntryID != nil && d.new.Mode != gitobj.ModeGitlink {
				data, _ := content(d)
				if declared := s.opts.ImportEntryID(d.path, data); validEntryID(declared) && !claimed[declared] {
					entry = declared
				}
			}
			if from, ok := renames[d.path]; entry == "" && ok && deleted[from] && !claimed[parentFiles[from].EntryID] {
				entry = parentFiles[from].EntryID
			}
			if entry == "" {
				for _, o := range diff {
					if o.old != nil && o.new == nil && o.old.ID == d.new.ID && !claimed[parentFiles[o.path].EntryID] {
						entry = parentFiles[o.path].EntryID
						break
					}
				}
			}
			if entry == "" {
				sum := sha1.Sum(append(bytesOf(id), d.path...))
				entry = hex.EncodeToString(sum[:16])
			}
			assigned[d.path], claimed[entry] = entry, true
		}

		head = Head{Seq: cur.Seq + 1, Commit: id}
		if fresh {
			head.Seq = 1
		}
		changed := map[string]*File{}
		before, after := map[string]File{}, map[string]File{}
		prev := map[gitobj.ID]gitobj.ID{}
		for _, d := range diff {
			if d.old != nil {
				before[parentFiles[d.path].EntryID] = parentFiles[d.path]
			}
			if d.new == nil {
				changed[d.path] = nil
				continue
			}
			f := &File{Path: d.path, EntryID: assigned[d.path], Blob: d.new.ID, Mode: d.new.Mode, Size: sizes[d.new.ID], Version: id}
			changed[d.path], after[f.EntryID] = f, *f
			if d.old != nil && versions[d.new.ID] != nil {
				prev[d.new.ID] = d.old.ID
			}
		}
		var changes []Change
		refs := map[gitobj.ID]bool{}
		for entry := range unionKeys(before, after) {
			b, hadB := before[entry]
			a, hasA := after[entry]
			ch := Change{EntryID: entry, Path: a.Path, Blob: a.Blob, Mode: a.Mode, Size: a.Size}
			switch {
			case !hadB:
				ch.Op = OpAdd
			case !hasA:
				ch = Change{Op: OpDelete, EntryID: entry, Path: b.Path, Mode: b.Mode}
			case a.Path != b.Path:
				ch.Op, ch.OldPath = OpRename, b.Path
			case a.Blob != b.Blob || a.Mode != b.Mode:
				ch.Op = OpModify
			default:
				continue
			}
			if ch.Op != OpDelete && ch.Mode != gitobj.ModeGitlink {
				refs[ch.Blob] = true
			}
			changes = append(changes, ch)
		}
		slices.SortFunc(changes, func(x, y Change) int { return strings.Compare(x.Path, y.Path) })
		if err := n.ownTrees(ctx, tx, id, kept); err != nil {
			return err
		}
		if stored, err = s.storeVersions(ctx, tx, slices.Collect(maps.Keys(refs)), versions, prev, staged); err != nil {
			return err
		}

		batch := &pgx.Batch{}
		queueTrees(batch, org, n.id, newTrees)
		queueRevision(batch, org, n.id, head.Seq, first.seq, id, c, raw)
		queueChanges(batch, org, n.id, head.Seq, changes)
		if linear {
			dirs, err := changedDirs(ctx, get, c.Tree, m.dirs)
			if err != nil {
				return err
			}
			queueEntries(batch, org, n.id, m, changed)
			next = m.derive(head, changed, dirs)
		} else {
			files := maps.Clone(parentFiles)
			for p, f := range changed {
				if f == nil {
					delete(files, p)
				} else {
					files[p] = *f
				}
			}
			dirs, err := changedDirs(ctx, get, c.Tree, nil)
			if err != nil {
				return err
			}
			batch.Queue(`DELETE FROM entries WHERE org = $1 AND notebook = $2`, org, n.id)
			var rows []*File
			next = &model{head: head, inc: inc, files: files, byEntry: map[string]string{}, dirs: map[string]*dirNode{}}
			for p, f := range files {
				next.byEntry[f.EntryID] = p
				rows = append(rows, &f)
			}
			for p, d := range dirs {
				next.dirs[p] = d
			}
			next.paths = slices.Sorted(maps.Keys(files))
			next.bytes = next.size()
			queueEntryRows(batch, org, n.id, rows)
		}
		batch.Queue(`UPDATE notebook_heads SET seq = $3, commit_sha = $4 WHERE org = $1 AND notebook = $2`, org, n.id, head.Seq, id[:])
		return tx.SendBatch(ctx, batch).Close()
	}
	if err := s.writeTx(ctx, attempt, staged); err != nil {
		return Head{}, err
	}
	if next != nil {
		for _, v := range stored {
			s.cacheVersion(v)
		}
		n.committed(next)
	}
	return head, nil
}

// ownTrees checks that the notebook has each tree in kept marked true, which
// an import of commit read from the store's tree cache, shared by every
// notebook, rather than from the notebook's git_trees.
func (n *Notebook) ownTrees(ctx context.Context, q querier, commit gitobj.ID, kept map[gitobj.ID]bool) error {
	var keys [][]byte
	for id, check := range kept {
		if check {
			keys = append(keys, bytesOf(id))
		}
	}
	if len(keys) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, `SELECT tree_sha FROM git_trees WHERE org = $1 AND notebook = $2 AND tree_sha = ANY($3)`, n.s.db.Org, n.id, keys)
	if err != nil {
		return err
	}
	have, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return err
	}
	for _, sha := range have {
		delete(kept, toID(sha))
	}
	for id, check := range kept {
		if check {
			return invalid("commit %s needs tree %s, which the import did not send", commit, id)
		}
	}
	return nil
}

// filesAt returns every file of revision r with its entry and Version.
func (n *Notebook) filesAt(ctx context.Context, q querier, r revision) (map[string]File, error) {
	tree, err := n.s.listTree(ctx, q, n.id, r.tree)
	if err != nil {
		return nil, err
	}
	at, err := n.versionsAt(ctx, q, r.seq, slices.Collect(maps.Keys(tree)))
	if err != nil {
		return nil, err
	}
	files := map[string]File{}
	for p, e := range tree {
		f := at[p]
		files[p] = File{Path: p, EntryID: f.entry, Blob: e.ID, Mode: e.Mode, Size: f.size, Version: f.version}
	}
	return files, nil
}

// changedDirs returns the directories of tree root that differ from old: each
// new or changed one, and nil for each of old's that root no longer has.
func changedDirs(ctx context.Context, get func(context.Context, []gitobj.ID) (map[gitobj.ID][]gitobj.TreeEntry, error), root gitobj.ID, old map[string]*dirNode) (map[string]*dirNode, error) {
	out := map[string]*dirNode{}
	present := map[string]bool{"": true}
	level := map[string]gitobj.ID{"": root}
	for len(level) > 0 {
		var ids []gitobj.ID
		for p, id := range level {
			if o := old[p]; o == nil || o.id != id {
				ids = append(ids, id)
			}
		}
		trees, err := get(ctx, ids)
		if err != nil {
			return nil, err
		}
		next := map[string]gitobj.ID{}
		for p, id := range level {
			if o := old[p]; o != nil && o.id == id {
				// Unchanged: so is everything below it.
				for sub := range old {
					if strings.HasPrefix(sub, p+"/") || p == "" {
						present[sub] = true
					}
				}
				continue
			}
			entries := trees[id]
			out[p] = &dirNode{id: id, entries: entries}
			for _, e := range entries {
				if e.IsTree() {
					child := joinPath(p, e.Name)
					present[child] = true
					next[child] = e.ID
				}
			}
		}
		level = next
	}
	for p := range old {
		if !present[p] {
			out[p] = nil
		}
	}
	return out, nil
}

// Objects calls emit with every object of the revisions after afterSeq, in
// order, each revision's blobs and trees before its commit, and only those
// its parents do not already have. Bodies are the stored objects byte for
// byte, ready for gitobj.WriteLoose.
func (n *Notebook) Objects(ctx context.Context, afterSeq int64, emit func(kind string, id gitobj.ID, body []byte) error) error {
	s, org := n.s, n.s.db.Org
	get := n.treeGetter(s.db.Pool)
	for {
		rows, err := s.db.Pool.Query(ctx, `SELECT seq, commit_sha, tree_sha, commit_body FROM revisions WHERE org = $1 AND notebook = $2 AND seq > $3 ORDER BY seq LIMIT 200`, org, n.id, afterSeq)
		if err != nil {
			return err
		}
		type rev struct {
			seq      int64
			id, tree gitobj.ID
			body     []byte
			commit   gitobj.Commit
		}
		var revs []rev
		parentIDs := map[gitobj.ID]bool{}
		for rows.Next() {
			var r rev
			var sha, tree []byte
			if err := rows.Scan(&r.seq, &sha, &tree, &r.body); err != nil {
				rows.Close()
				return err
			}
			r.id, r.tree = toID(sha), toID(tree)
			if r.commit, err = gitobj.ParseCommit(r.body); err != nil {
				rows.Close()
				return fmt.Errorf("revision %s: %w", r.id, err)
			}
			for _, p := range r.commit.Parents {
				parentIDs[p] = true
			}
			revs = append(revs, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(revs) == 0 {
			return err
		}
		parentTrees := map[gitobj.ID]gitobj.ID{}
		if len(parentIDs) > 0 {
			var keys [][]byte
			for p := range parentIDs {
				keys = append(keys, bytesOf(p))
			}
			rows, err := s.db.Pool.Query(ctx, `SELECT commit_sha, tree_sha FROM revisions WHERE org = $1 AND notebook = $2 AND commit_sha = ANY($3)`, org, n.id, keys)
			if err != nil {
				return err
			}
			for rows.Next() {
				var sha, tree []byte
				if err := rows.Scan(&sha, &tree); err != nil {
					rows.Close()
					return err
				}
				parentTrees[toID(sha)] = toID(tree)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
		for _, r := range revs {
			var base []gitobj.ID
			for _, p := range r.commit.Parents {
				base = append(base, parentTrees[p])
			}
			var newTrees, newBlobs []gitobj.ID
			if err := newObjects(ctx, get, r.tree, base, map[gitobj.ID]bool{}, &newTrees, &newBlobs); err != nil {
				return err
			}
			for _, b := range newBlobs {
				data, err := s.readContent(ctx, s.db.Pool, b)
				if err != nil {
					return err
				}
				if gitobj.BlobID(data) != b {
					return fmt.Errorf("content %s is corrupt", b)
				}
				if err := emit("blob", b, data); err != nil {
					return err
				}
			}
			bodies, err := n.treeBodies(ctx, newTrees)
			if err != nil {
				return err
			}
			for _, t := range newTrees {
				if err := emit("tree", t, bodies[t]); err != nil {
					return err
				}
			}
			if err := emit("commit", r.id, r.body); err != nil {
				return err
			}
			afterSeq = r.seq
		}
	}
}

// newObjects lists the trees under id and the blobs they hold that no tree of
// base, at the same path, already has; children come before their tree.
func newObjects(ctx context.Context, get func(context.Context, []gitobj.ID) (map[gitobj.ID][]gitobj.TreeEntry, error), id gitobj.ID, base []gitobj.ID, seen map[gitobj.ID]bool, trees, blobs *[]gitobj.ID) error {
	if seen[id] || slices.Contains(base, id) {
		return nil
	}
	seen[id] = true
	ids := append([]gitobj.ID{id}, base...)
	ids = slices.DeleteFunc(ids, func(x gitobj.ID) bool { return x.IsZero() })
	got, err := get(ctx, ids)
	if err != nil {
		return err
	}
	for _, e := range got[id] {
		var same []gitobj.ID
		for _, b := range base {
			for _, be := range got[b] {
				if be.Name == e.Name && be.IsTree() == e.IsTree() {
					same = append(same, be.ID)
				}
			}
		}
		switch {
		case e.IsTree():
			if err := newObjects(ctx, get, e.ID, same, seen, trees, blobs); err != nil {
				return err
			}
		case e.Mode != gitobj.ModeGitlink && !slices.Contains(same, e.ID) && !seen[e.ID]:
			seen[e.ID] = true
			*blobs = append(*blobs, e.ID)
		}
	}
	*trees = append(*trees, id)
	return nil
}

func (n *Notebook) treeBodies(ctx context.Context, ids []gitobj.ID) (map[gitobj.ID][]byte, error) {
	out := map[gitobj.ID][]byte{}
	var keys [][]byte
	for _, id := range ids {
		if id == gitobj.EmptyTree {
			out[id] = []byte{}
		} else {
			keys = append(keys, bytesOf(id))
		}
	}
	if len(keys) > 0 {
		rows, err := n.s.db.Pool.Query(ctx, `SELECT tree_sha, body FROM git_trees WHERE org = $1 AND notebook = $2 AND tree_sha = ANY($3)`, n.s.db.Org, n.id, keys)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var sha, body []byte
			if err := rows.Scan(&sha, &body); err != nil {
				return nil, err
			}
			out[toID(sha)] = body
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			return nil, fmt.Errorf("tree %s is missing", id)
		}
	}
	return out, nil
}
