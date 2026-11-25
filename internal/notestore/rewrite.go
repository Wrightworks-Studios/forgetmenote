package notestore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"forgetmenote/internal/gitobj"

	"github.com/jackc/pgx/v5"
)

// PurgeTrailer marks every commit a purge rewrote with the purge's ID, which
// the Git export's purge flag names.
const PurgeTrailer = "Gitnotes-Purge"

// purgeID is the form of a purge ID; the Git export and its backup worker
// accept the same.
var purgeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// A RewriteFile is one file version a rewrite asks its function about.
type RewriteFile struct {
	Path    string
	EntryID string
	Blob    gitobj.ID
	Mode    uint32
	Size    int64
}

// A RewriteFunc decides what a file version becomes. content reads the
// version's bytes, so a function that decides by entry, path or blob alone
// reads nothing. keep false drops the file from the revision; otherwise
// newContent replaces its bytes, or nil keeps them. Rewrite asks once for each
// entry, path and blob, so the answer must depend on nothing else.
type RewriteFunc func(f RewriteFile, content func() ([]byte, error)) (newContent []byte, keep bool, err error)

// RewriteOptions describe a rewrite beyond its function.
type RewriteOptions struct {
	// ExpectHead, when set, is the head the rewrite must find, such as the
	// one its plan was made at; another is a ConflictError.
	ExpectHead *gitobj.ID
	// Message, when set, rewrites the message of each rewritten commit, given
	// its seq, before the purge trailer is added.
	Message func(seq int64, message string) (string, error)
	// Actor and Reason say who ran the purge and why, and Scope, marshaled
	// as JSON, what it covered. They are kept in the purges table, so none of
	// them may hold purged content or anything derived from it.
	Actor, Reason string
	Scope         any
	// ResetLive names entries whose live documents start a new epoch besides
	// those whose content the rewrite changes, such as notes whose pending
	// live edits hold purged text.
	ResetLive []string
	// ExpectAuthority, when set, is the version the notebook's authority
	// record must still have, 0 for none, as for ChangeSet.ExpectAuthority:
	// a handoff another gateway prepared or completed since the purge
	// checked it fails the purge with ErrAuthorityChanged.
	ExpectAuthority *int64
}

// checkAuthority fails with ErrAuthorityChanged unless the notebook's
// authority record is still at o.ExpectAuthority. tx holds the head row.
func (o RewriteOptions) checkAuthority(ctx context.Context, tx pgx.Tx, org, notebook string) error {
	if o.ExpectAuthority == nil {
		return nil
	}
	version, err := authorityVersion(ctx, tx, org, notebook)
	if err != nil {
		return err
	}
	if version != *o.ExpectAuthority {
		return ErrAuthorityChanged
	}
	return nil
}

// A RewriteResult is the outcome of a purge's history rewrite.
type RewriteResult struct {
	// Head is the new head: the same seq, a new commit.
	Head    Head
	FromSeq int64
	// Rewritten maps each rewritten commit to its replacement.
	Rewritten map[gitobj.ID]gitobj.ID
	// Entries lists the entries the rewrite changed or dropped a version of.
	Entries []string
	// LiveReset lists the entries whose live documents started a new epoch.
	LiveReset []string
	Removed   RemovedContent
}

// RemovedContent counts what a rewrite deleted: file versions no revision
// uses any more, trees no revision reaches, and their segments and blocks.
type RemovedContent struct {
	Versions, Trees, Segments, Blocks int
}

// A newRevision is a rewritten commit, as the rewrite stores it.
type newRevision struct {
	seq, parentSeq int64
	old, id        gitobj.ID
	commit         gitobj.Commit
	body           []byte
	changes        []Change
}

// A newVersion is content a rewrite stores: inline, or as segments whose
// blocks it put before its transaction, so it holds no large content.
type newVersion struct {
	blob     gitobj.ID
	size     int64
	inline   []byte
	segments []segmentRef
	bodies   [][]byte
}

type rewriteKey struct {
	entry, path string
	blob        gitobj.ID
}

type rewriteAnswer struct {
	blob          gitobj.ID
	size          int64
	keep, changed bool
}

// lineState is the rewritten state of one revision: the files and
// directories of its replacement.
type lineState struct {
	files map[string]File
	dirs  map[string]*dirNode
}

func (st *lineState) clone() *lineState {
	return &lineState{files: maps.Clone(st.files), dirs: maps.Clone(st.dirs)}
}

type rewriter struct {
	n       *Notebook
	fn      RewriteFunc
	message func(int64, string) (string, error)
	purge   string
	from    int64

	answers  map[rewriteKey]rewriteAnswer
	versions map[gitobj.ID]*newVersion
	replaced map[gitobj.ID]bool // old blobs some rewritten version no longer uses
	entries  map[string]bool
	staged   map[[32]byte]bool
	known    map[gitobj.ID][]gitobj.TreeEntry // trees this rewrite parsed or built
	trees    map[gitobj.ID][]byte             // trees it built
	mapping  map[gitobj.ID]gitobj.ID
	dropped  map[droppedFile]bool // the files the rewrite dropped from the revisions that wrote them
}

// A droppedFile is a file a commit wrote that the rewrite dropped.
type droppedFile struct {
	commit gitobj.ID
	path   string
}

// Rewrite rebuilds the notebook's history from revision fromSeq to the head
// with fn applied to every file version, for a purge (storage v2 design §11),
// the one operation that changes history. Every rewritten commit keeps its
// author, committer, times and message, with a "Gitnotes-Purge: <purge>"
// trailer added and any signature dropped; its tree and parents are
// recomputed, so it and every later commit get new IDs, and each old ID is
// recorded against its replacement under purge. Rewritten commits are kept
// even when nothing of theirs is left, so the head keeps its seq and every
// old commit maps to a new one.
//
// One transaction, holding the head row's lock, replaces the revisions,
// their changes, the entries and the head, records the purge, points the
// commit receipts at the rewritten commits without the files fn dropped,
// resets the live documents of every entry whose content changed and of
// o.ResetLive, and deletes the trees no revision reaches and the file
// versions no revision of any notebook uses. Their segments and blocks go
// right after, unless another file version still uses them. A file of the
// revision before fromSeq that fn would change is refused: it would stay in
// the history the rewrite keeps.
func (n *Notebook) Rewrite(ctx context.Context, fromSeq int64, purge string, fn RewriteFunc, o RewriteOptions) (RewriteResult, error) {
	if !purgeID.MatchString(purge) {
		return RewriteResult{}, invalid("purge ID %q", purge)
	}
	if fn == nil {
		return RewriteResult{}, invalid("a rewrite needs a function")
	}
	if strings.TrimSpace(o.Actor) == "" || strings.TrimSpace(o.Reason) == "" {
		return RewriteResult{}, invalid("a purge needs an actor and a reason")
	}
	scope, err := json.Marshal(o.Scope)
	if err != nil {
		return RewriteResult{}, err
	}
	for _, entry := range o.ResetLive {
		if !validEntryID(entry) {
			return RewriteResult{}, invalid("entry ID %q", entry)
		}
	}
	n.write.Lock()
	locked := true
	defer func() {
		if locked {
			n.write.Unlock()
		}
	}()
	s, org := n.s, n.s.db.Org
	head, inc, err := readHead(ctx, s.db.Pool, org, n.id)
	if err != nil {
		return RewriteResult{}, err
	}
	if o.ExpectHead != nil && *o.ExpectHead != head.Commit {
		return RewriteResult{}, &ConflictError{Expected: *o.ExpectHead, Actual: head.Commit}
	}
	fromSeq = max(fromSeq, 1)
	if fromSeq > head.Seq {
		return RewriteResult{}, invalid("revision %d is after the head", fromSeq)
	}
	r := &rewriter{n: n, fn: fn, message: o.Message, purge: purge, from: fromSeq,
		answers: map[rewriteKey]rewriteAnswer{}, versions: map[gitobj.ID]*newVersion{}, replaced: map[gitobj.ID]bool{},
		entries: map[string]bool{}, staged: map[[32]byte]bool{}, known: map[gitobj.ID][]gitobj.TreeEntry{},
		trees: map[gitobj.ID][]byte{}, mapping: map[gitobj.ID]gitobj.ID{}, dropped: map[droppedFile]bool{}}
	// The rewrite is planned from the stored history outside the transaction,
	// which then checks that the head is still the one planned from, so no
	// revision the plan read has changed.
	revs, last, err := r.rebuild(ctx, head)
	if err != nil {
		return RewriteResult{}, err
	}
	result := RewriteResult{FromSeq: fromSeq, Rewritten: r.mapping, Head: Head{Seq: head.Seq, Commit: r.mapping[head.Commit]}}
	result.Entries = slices.Sorted(maps.Keys(r.entries))
	reset := map[string]bool{}
	for _, entry := range append(slices.Clone(result.Entries), o.ResetLive...) {
		reset[entry] = true
	}
	var next *model
	var segments [][32]byte
	var removedTrees []gitobj.ID
	err = pgx.BeginFunc(ctx, s.db.Pool, func(tx pgx.Tx) error {
		result.Removed, result.LiveReset, next, segments, removedTrees = RemovedContent{}, nil, nil, nil, nil
		cur, curInc, err := lockHead(ctx, tx, org, n.id)
		if err != nil {
			return err
		}
		if cur != head || curInc != inc {
			return &ConflictError{Expected: head.Commit, Actual: cur.Commit, Reason: "the notebook changed during the purge"}
		}
		if err := o.checkAuthority(ctx, tx, org, n.id); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO purges (org, notebook, purge_id, actor, reason, scope, from_seq) VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING`,
			org, n.id, purge, o.Actor, o.Reason, scope, fromSeq)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return invalid("purge %s already ran", purge)
		}
		// Locked first, as every writer locks the file versions it uses
		// before blocks and segments.
		replaced, err := lockVersions(ctx, tx, org, slices.Collect(maps.Keys(r.replaced)))
		if err != nil {
			return err
		}
		if err := r.storeVersions(ctx, tx); err != nil {
			return err
		}
		if err := insertTrees(ctx, tx, org, n.id, r.trees); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM revisions WHERE org = $1 AND notebook = $2 AND seq >= $3`, org, n.id, fromSeq); err != nil {
			return err
		}
		if err := insertRevisions(ctx, tx, org, n.id, revs); err != nil {
			return err
		}
		if err := insertRewrites(ctx, tx, org, n.id, purge, revs); err != nil {
			return err
		}
		if err := r.remapReceipts(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE live_docs l SET base_version = m.new FROM unnest($3::bytea[], $4::bytea[]) AS m(old, new)
			WHERE l.org = $1 AND l.notebook = $2 AND l.base_version = m.old`, org, n.id, oldIDs(revs), newIDs(revs)); err != nil {
			return err
		}
		if result.LiveReset, err = resetLive(ctx, tx, org, n.id, slices.Sorted(maps.Keys(reset))); err != nil {
			return err
		}
		// A new incarnation, so every gateway replaces its read model of the
		// old history, whatever seq it is at.
		if err = tx.QueryRow(ctx, `UPDATE notebook_heads SET commit_sha = $3, incarnation = nextval(pg_get_serial_sequence('notebook_heads', 'incarnation'))
			WHERE org = $1 AND notebook = $2 RETURNING incarnation`, org, n.id, result.Head.Commit[:]).Scan(&curInc); err != nil {
			return err
		}
		next = r.model(last, result.Head, curInc)
		batch := &pgx.Batch{}
		batch.Queue(`DELETE FROM entries WHERE org = $1 AND notebook = $2`, org, n.id)
		files := make([]*File, 0, len(next.files))
		for _, p := range next.paths {
			f := next.files[p]
			files = append(files, &f)
		}
		queueEntryRows(batch, org, n.id, files)
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return err
		}
		if removedTrees, err = r.collectTrees(ctx, tx, revs); err != nil {
			return err
		}
		result.Removed.Trees = len(removedTrees)
		segments, result.Removed.Versions, err = deleteVersions(ctx, tx, org, replaced)
		return err
	})
	if err != nil {
		return RewriteResult{}, err
	}
	for b := range r.replaced {
		s.manifests.remove(b)
	}
	for _, id := range removedTrees {
		s.trees.remove(id)
	}
	n.committed(next)
	// The rewrite is done; removing what it freed locks the rows it removes,
	// as every write that uses them does, so it keeps no write to the
	// notebook waiting however long the blob store takes.
	n.write.Unlock()
	locked = false
	result.Removed.Segments, result.Removed.Blocks, err = s.purgeAfterCommit(ctx, segments)
	if err != nil {
		return result, fmt.Errorf("purge %s rewrote the notebook, but removing the content blocks it freed failed, so the purged content is still in the blob store until the scheduled block sweep removes it within about a day: %w", purge, err)
	}
	return result, nil
}

// rebuild computes every rewritten revision from r.from to head, and returns
// them with the state of the last.
func (r *rewriter) rebuild(ctx context.Context, head Head) ([]newRevision, *lineState, error) {
	n, s := r.n, r.n.s
	// Revisions whose first parent is not the revision before them, as
	// imported branches have, start from that parent's state.
	rows, err := s.db.Pool.Query(ctx, `SELECT parent_seq FROM revisions WHERE org = $1 AND notebook = $2 AND seq > $3 AND parent_seq >= $3 AND parent_seq IS DISTINCT FROM seq - 1`,
		s.db.Org, n.id, r.from)
	if err != nil {
		return nil, nil, err
	}
	points, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, nil, err
	}
	keep := map[int64]bool{}
	for _, p := range points {
		keep[p] = true
	}
	saved := map[int64]*lineState{}
	var revs []newRevision
	var cur *lineState
	curSeq := int64(-1)
	for after := r.from - 1; after < head.Seq; {
		batch, err := r.load(ctx, after, head.Seq)
		if err != nil {
			return nil, nil, err
		}
		if len(batch) == 0 {
			return nil, nil, fmt.Errorf("notebook %s: revisions after %d are missing", n.id, after)
		}
		for _, old := range batch {
			switch {
			case old.parentSeq == 0:
				cur = &lineState{files: map[string]File{}, dirs: map[string]*dirNode{"": {id: gitobj.EmptyTree}}}
			case old.parentSeq == curSeq:
			case old.parentSeq < r.from:
				if cur, err = r.base(ctx, old.parentSeq); err != nil {
					return nil, nil, err
				}
			default:
				st, ok := saved[old.parentSeq]
				if !ok {
					return nil, nil, fmt.Errorf("notebook %s: revision %d follows %d, which does not precede it", n.id, old.seq, old.parentSeq)
				}
				cur = st.clone()
			}
			next, err := r.revision(ctx, cur, old)
			if err != nil {
				return nil, nil, err
			}
			revs = append(revs, next)
			curSeq, after = old.seq, old.seq
			if keep[old.seq] {
				saved[old.seq] = cur.clone()
			}
		}
	}
	return revs, cur, nil
}

// load reads up to 500 revisions after seq after, through last, with their
// changes.
func (r *rewriter) load(ctx context.Context, after, last int64) ([]newRevision, error) {
	s, n := r.n.s, r.n
	rows, err := s.db.Pool.Query(ctx, `SELECT seq, coalesce(parent_seq, 0), commit_sha, commit_body FROM revisions WHERE org = $1 AND notebook = $2 AND seq > $3 AND seq <= $4 ORDER BY seq LIMIT 500`,
		s.db.Org, n.id, after, last)
	if err != nil {
		return nil, err
	}
	var revs []newRevision
	index := map[int64]int{}
	for rows.Next() {
		var rev newRevision
		var sha, body []byte
		if err := rows.Scan(&rev.seq, &rev.parentSeq, &sha, &body); err != nil {
			rows.Close()
			return nil, err
		}
		rev.old, rev.body = toID(sha), body
		index[rev.seq] = len(revs)
		revs = append(revs, rev)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(revs) == 0 {
		return nil, err
	}
	rows, err = s.db.Pool.Query(ctx, `SELECT seq, op, path, coalesce(old_path, ''), entry_id, blob_sha, mode, size FROM revision_changes
		WHERE org = $1 AND notebook = $2 AND seq >= $3 AND seq <= $4 ORDER BY seq, path`, s.db.Org, n.id, revs[0].seq, revs[len(revs)-1].seq)
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
		if i, ok := index[seq]; ok {
			revs[i].changes = append(revs[i].changes, c)
		}
	}
	return revs, rows.Err()
}

// base returns the state of revision seq, which the rewrite keeps: every
// file must stay as it is, or what the rewrite removes would remain in the
// history before it.
func (r *rewriter) base(ctx context.Context, seq int64) (*lineState, error) {
	s, n := r.n.s, r.n
	var rev revision
	var sha, tree []byte
	err := s.db.Pool.QueryRow(ctx, `SELECT seq, commit_sha, tree_sha FROM revisions WHERE org = $1 AND notebook = $2 AND seq = $3`, s.db.Org, n.id, seq).Scan(&rev.seq, &sha, &tree)
	if err != nil {
		return nil, fmt.Errorf("notebook %s revision %d: %w", n.id, seq, err)
	}
	rev.id, rev.tree = toID(sha), toID(tree)
	files, err := n.filesAt(ctx, s.db.Pool, rev)
	if err != nil {
		return nil, err
	}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		f := files[p]
		after, keep, err := r.transform(ctx, f)
		if err != nil {
			return nil, err
		}
		if !keep || after.Blob != f.Blob {
			return nil, invalid("revision %s already holds %s as the rewrite would change it; rewrite from an earlier revision", rev.id, p)
		}
	}
	dirs, err := s.walkTrees(ctx, s.db.Pool, n.id, rev.tree)
	if err != nil {
		return nil, err
	}
	for _, d := range dirs {
		r.known[d.id] = d.entries
	}
	return &lineState{files: maps.Clone(files), dirs: dirs}, nil
}

// revision rewrites old on top of st, the rewritten state of its first
// parent, and leaves st the state of its replacement.
func (r *rewriter) revision(ctx context.Context, st *lineState, old newRevision) (newRevision, error) {
	// What the old revision changed, as it becomes: a path the old revision
	// vacated, or whose new file the rewrite drops, maps to nil.
	changed := map[string]*File{}
	vacate := func(p string) {
		if _, ok := changed[p]; !ok {
			changed[p] = nil
		}
	}
	for _, c := range old.changes {
		switch c.Op {
		case OpDelete:
			vacate(c.Path)
		case OpRename:
			vacate(c.OldPath)
		}
	}
	for _, c := range old.changes {
		if c.Op == OpDelete {
			continue
		}
		f := File{Path: c.Path, EntryID: c.EntryID, Blob: c.Blob, Mode: c.Mode, Size: c.Size, Version: old.old}
		after, keep, err := r.transform(ctx, f)
		if err != nil {
			return newRevision{}, err
		}
		if keep {
			changed[c.Path] = &after
		} else {
			changed[c.Path] = nil
			r.dropped[droppedFile{old.old, c.Path}] = true
		}
	}
	// A change the rewrite made a no-op is no change.
	for p, f := range changed {
		cur, exists := st.files[p]
		if f == nil && !exists || f != nil && exists && cur.EntryID == f.EntryID && cur.Blob == f.Blob && cur.Mode == f.Mode {
			delete(changed, p)
		}
	}
	commit, err := gitobj.ParseCommit(old.body)
	if err != nil {
		return newRevision{}, fmt.Errorf("revision %s: %w", old.old, err)
	}
	plan := &writePlan{changed: changed, trees: map[gitobj.ID][]byte{}}
	if len(changed) == 0 {
		plan.root = st.dirs[""].id
	} else if err := buildTrees(&model{dirs: st.dirs}, plan); err != nil {
		return newRevision{}, fmt.Errorf("revision %s: %w", old.old, err)
	}
	commit.Tree = plan.root
	for i, p := range commit.Parents {
		if id, ok := r.mapping[p]; ok {
			commit.Parents[i] = id
		}
	}
	message := commit.Message
	if r.message != nil {
		if message, err = r.message(old.seq, message); err != nil {
			return newRevision{}, err
		}
	}
	commit.Message = gitobj.CleanMessage(message, "", []gitobj.Trailer{{Key: PurgeTrailer, Value: r.purge}})
	commit.Extra = dropSignatures(commit.Extra)
	next := newRevision{seq: old.seq, parentSeq: old.parentSeq, old: old.old, commit: commit, body: gitobj.EncodeCommit(commit)}
	next.id = gitobj.HashObject("commit", next.body)
	r.mapping[old.old] = next.id

	// The replacement's changes against its first parent, by entry, as
	// Apply records them.
	before, after := map[string]File{}, map[string]File{}
	for p, f := range changed {
		if cur, ok := st.files[p]; ok {
			before[cur.EntryID] = cur
		}
		if f != nil {
			f.Version = next.id
			after[f.EntryID] = *f
		}
	}
	for entry := range unionKeys(before, after) {
		b, hadB := before[entry]
		a, hasA := after[entry]
		c := Change{EntryID: entry, Path: a.Path, Blob: a.Blob, Mode: a.Mode, Size: a.Size}
		switch {
		case !hadB:
			c.Op = OpAdd
		case !hasA:
			c = Change{Op: OpDelete, EntryID: entry, Path: b.Path, Mode: b.Mode}
		case a.Path != b.Path:
			c.Op, c.OldPath = OpRename, b.Path
		default:
			c.Op = OpModify
		}
		next.changes = append(next.changes, c)
	}
	slices.SortFunc(next.changes, func(x, y Change) int { return strings.Compare(x.Path, y.Path) })
	for p, f := range changed {
		if f == nil {
			delete(st.files, p)
		} else {
			st.files[p] = *f
		}
	}
	for d, node := range plan.dirs {
		if node == nil {
			delete(st.dirs, d)
		} else {
			st.dirs[d] = node
			r.known[node.id] = node.entries
		}
	}
	maps.Copy(r.trees, plan.trees)
	return next, nil
}

// transform returns what f becomes, asking fn once per entry, path and blob.
func (r *rewriter) transform(ctx context.Context, f File) (File, bool, error) {
	key := rewriteKey{f.EntryID, f.Path, f.Blob}
	a, ok := r.answers[key]
	if !ok {
		var err error
		if a, err = r.ask(ctx, f); err != nil {
			return f, false, err
		}
		r.answers[key] = a
	}
	if a.changed {
		f.Blob, f.Size = a.blob, a.size
	}
	return f, a.keep, nil
}

func (r *rewriter) ask(ctx context.Context, f File) (rewriteAnswer, error) {
	s := r.n.s
	content := func() ([]byte, error) {
		if f.Mode == gitobj.ModeGitlink {
			return nil, invalid("%s is a submodule, not a file", f.Path)
		}
		return s.readContent(ctx, s.db.Pool, f.Blob)
	}
	data, keep, err := r.fn(RewriteFile{Path: f.Path, EntryID: f.EntryID, Blob: f.Blob, Mode: f.Mode, Size: f.Size}, content)
	if err != nil {
		return rewriteAnswer{}, err
	}
	if !keep {
		r.replaced[f.Blob], r.entries[f.EntryID] = true, true
		return rewriteAnswer{}, nil
	}
	if data == nil {
		return rewriteAnswer{keep: true}, nil
	}
	if f.Mode == gitobj.ModeGitlink {
		return rewriteAnswer{}, invalid("%s is a submodule, which has no content to rewrite", f.Path)
	}
	id := gitobj.BlobID(data)
	if id == f.Blob {
		return rewriteAnswer{keep: true}, nil
	}
	if err := r.store(ctx, f, id, data); err != nil {
		return rewriteAnswer{}, err
	}
	r.replaced[f.Blob], r.entries[f.EntryID] = true, true
	return rewriteAnswer{blob: id, size: int64(len(data)), keep: true, changed: true}, nil
}

// store prepares new content for the transaction and puts its blocks now,
// except those of the version it replaces, which stay in use until the
// transaction has stored the new version.
func (r *rewriter) store(ctx context.Context, f File, id gitobj.ID, data []byte) error {
	if _, ok := r.versions[id]; ok {
		return nil
	}
	s := r.n.s
	v := prepare(f.Path, data)
	nv := &newVersion{blob: id, size: int64(len(data))}
	if v.inline() {
		nv.inline = bytes.Clone(data)
		if nv.inline == nil {
			nv.inline = []byte{}
		}
		r.versions[id] = nv
		return nil
	}
	nv.segments, nv.bodies = v.segments, v.bodies
	have := map[[32]byte]bool{}
	if old, err := s.manifest(ctx, s.db.Pool, f.Blob); err == nil {
		for _, b := range old.blocks {
			have[b.sha] = true
		}
	}
	var put []pendingBlock
	for i, ref := range v.refs {
		if !have[ref.sha] && !r.staged[ref.sha] {
			have[ref.sha] = true
			put = append(put, pendingBlock{ref, v.blocks[i]})
		}
	}
	if err := s.stage(ctx, put, r.staged); err != nil {
		return err
	}
	r.versions[id] = nv
	return nil
}

// model is the read model of the rewritten head.
func (r *rewriter) model(st *lineState, head Head, inc int64) *model {
	m := &model{head: head, inc: inc, files: st.files, byEntry: map[string]string{}, dirs: st.dirs}
	for p, f := range st.files {
		m.byEntry[f.EntryID] = p
	}
	m.paths = slices.Sorted(maps.Keys(st.files))
	m.bytes = m.size()
	return m
}

// dropSignatures removes the signature headers from a commit's extra
// headers: a rewritten commit is no longer what was signed.
func dropSignatures(extra []byte) []byte {
	if len(extra) == 0 {
		return extra
	}
	var out []byte
	skip := false
	for _, line := range bytes.SplitAfter(extra, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		if line[0] != ' ' {
			name, _, _ := bytes.Cut(bytes.TrimSuffix(line, []byte("\n")), []byte(" "))
			switch string(name) {
			case "gpgsig", "gpgsig-sha256", "mergetag":
				skip = true
			default:
				skip = false
			}
		}
		if !skip {
			out = append(out, line...)
		}
	}
	return out
}
