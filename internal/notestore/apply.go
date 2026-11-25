package notestore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"

	"forgetmenote/internal/gitobj"

	"github.com/jackc/pgx/v5"
)

// A ChangeSet is one revision's worth of file changes and its commit.
type ChangeSet struct {
	// ExpectHead, when set, is the head the change must apply to.
	ExpectHead *gitobj.ID
	// ExpectVersion maps a path to the Version it must have; the zero ID
	// means the path must not exist.
	ExpectVersion map[string]gitobj.ID
	// Files are applied in order.
	Files []FileChange
	// Author signs the commit; its time defaults to now. The committer is
	// always ForgetMeNote at the same time.
	Author        gitobj.Signature
	Subject, Body string
	Trailers      []gitobj.Trailer
	// OperationID makes the change set idempotent: repeating an ID with the
	// same request returns the first result, and with another request fails
	// with ErrOperationMismatch.
	OperationID string
	// RequestID, with OperationID, is the caller's own name for the request:
	// the commit records both as its Gitnotes-Operation and Gitnotes-Request
	// trailers, and a repeated OperationID compares RequestID, not the change
	// set, so a retry replays even after other writes moved the head.
	RequestID string
	// AllowEmpty commits even when nothing changes; otherwise an unchanged
	// notebook gets no commit and the current head is returned.
	AllowEmpty bool
	// ExpectAuthority, when set, is the version the notebook's authority
	// record must still have, 0 for none; another fails the change set with
	// ErrAuthorityChanged. PutAuthority holds the head row, so no record is
	// stored between that check and the revision.
	ExpectAuthority *int64
	// Pending, when set, names a receipt that must still be pending when the
	// revision commits: one settled meanwhile (SettleReceipt) fails the
	// change set with a ConflictError.
	Pending *ReceiptKey
}

// A ReceiptKey names an operation's receipt.
type ReceiptKey struct{ Kind, Operation string }

// A FileChange writes, deletes or renames one path.
type FileChange struct {
	Path string
	// Content is the new content; nil deletes Path unless From is set.
	Content []byte
	// From renames that path to Path, keeping its entry ID, and its content
	// unless Content is set.
	From string
	// Mode is ModeFile, ModeExecutable or ModeSymlink; 0 keeps the current
	// mode, or ModeFile for a new file.
	Mode uint32
	// EntryID is the identity of a new file, such as a note's fmn-id; a
	// random one is chosen when it is empty. It must match an existing file's.
	EntryID string
}

// A CommitResult is the head after a change set, with the Version of every
// path it named (zero for a path it deleted).
type CommitResult struct {
	Head     Head
	Versions map[string]gitobj.ID
	Replayed bool
}

const commitReceipt = "commit"

// Apply writes cs as one revision in one transaction, which locks the
// notebook's head row, checks the expectations, builds the trees and commit,
// and stores the revision, its changes, the new content and the new head. A
// failed expectation returns a ConflictError, matching ErrConflict.
func (n *Notebook) Apply(ctx context.Context, cs ChangeSet) (CommitResult, error) {
	author, err := cleanSignature(cs.Author)
	if err != nil {
		return CommitResult{}, err
	}
	if strings.TrimSpace(cs.Subject) == "" {
		return CommitResult{}, invalid("commit subject is empty")
	}
	for _, t := range cs.Trailers {
		if strings.TrimSpace(t.Key) == "" {
			return CommitResult{}, invalid("trailer without a key")
		}
	}
	message, err := operationMessage(gitobj.CleanMessage(cs.Subject, cs.Body, cs.Trailers), cs.OperationID, cs.RequestID)
	if err != nil {
		return CommitResult{}, err
	}
	// Git refuses a commit with a NUL in its message or headers, and so
	// does git fsck, which every export and backup must pass.
	if strings.ContainsRune(message+author.Name+author.Email, 0) {
		return CommitResult{}, invalid("a commit message or signature contains NUL")
	}
	prepared := make([]*version, len(cs.Files))
	versions := map[gitobj.ID]*version{}
	for i, fc := range cs.Files {
		written := fc.Content != nil || fc.From != ""
		if written && !validPath(fc.Path) || !written && !storedPath(fc.Path) || fc.From != "" && (!storedPath(fc.From) || fc.From == fc.Path) {
			return CommitResult{}, invalid("path %q", fc.Path)
		}
		switch fc.Mode {
		case 0, gitobj.ModeFile, gitobj.ModeExecutable, gitobj.ModeSymlink:
		default:
			return CommitResult{}, invalid("mode %o of %s", fc.Mode, fc.Path)
		}
		if fc.EntryID != "" && !validEntryID(fc.EntryID) {
			return CommitResult{}, invalid("entry ID %q", fc.EntryID)
		}
		if fc.Content != nil {
			prepared[i] = prepare(fc.Path, fc.Content)
			versions[prepared[i].blob] = prepared[i]
		}
	}
	request := requestHash(cs, prepared, author)
	if cs.RequestID != "" {
		request = cs.RequestID
	}

	n.write.Lock()
	defer n.write.Unlock()
	s, org := n.s, n.s.db.Org
	staged := map[[32]byte]bool{}
	if err := n.prestage(ctx, cs.Files, prepared, staged); err != nil {
		return CommitResult{}, err
	}
	var result CommitResult
	var next *model
	var stored []*version
	attempt := func(tx pgx.Tx) error {
		result, next, stored = CommitResult{}, nil, nil
		head, inc, err := lockHead(ctx, tx, org, n.id)
		if err != nil {
			return err
		}
		if cs.OperationID != "" {
			r, found, err := readReceipt(ctx, tx, org, n.id, commitReceipt, cs.OperationID)
			if err != nil {
				return err
			}
			if found {
				if r.RequestHash != request {
					return fmt.Errorf("%w: %s", ErrOperationMismatch, cs.OperationID)
				}
				if err := json.Unmarshal(r.Result, &result); err != nil {
					return err
				}
				result.Replayed = true
				return nil
			}
		}
		if cs.ExpectAuthority != nil {
			version, err := authorityVersion(ctx, tx, org, n.id)
			if err != nil {
				return err
			}
			if version != *cs.ExpectAuthority {
				return ErrAuthorityChanged
			}
		}
		if cs.Pending != nil {
			r, found, err := readReceipt(ctx, tx, org, n.id, cs.Pending.Kind, cs.Pending.Operation)
			if err != nil {
				return err
			}
			if !found || r.State != ReceiptPending {
				return &ConflictError{Reason: fmt.Sprintf("the %s receipt of operation %s is not pending", cs.Pending.Kind, cs.Pending.Operation)}
			}
		}
		m := n.model.Load()
		if m == nil || !m.at(head, inc) {
			if m, err = s.loadModel(ctx, tx, n.id); err != nil {
				return err
			}
		}
		if cs.ExpectHead != nil && *cs.ExpectHead != head.Commit {
			return &ConflictError{Expected: *cs.ExpectHead, Actual: head.Commit}
		}
		for _, p := range slices.Sorted(maps.Keys(cs.ExpectVersion)) {
			want := cs.ExpectVersion[p]
			have := m.files[p].Version
			if have != want {
				return &ConflictError{Path: p, Expected: want, Actual: have}
			}
		}
		plan, err := planWrite(m, cs.Files, prepared)
		if err != nil {
			return err
		}
		result.Head = head
		result.Versions = map[string]gitobj.ID{}
		if len(plan.changes) == 0 && !cs.AllowEmpty {
			for _, fc := range cs.Files {
				result.Versions[fc.Path] = m.files[fc.Path].Version
			}
			return n.putReceipt(ctx, tx, cs.OperationID, request, result)
		}
		commit := gitobj.Commit{Tree: plan.root, Parents: []gitobj.ID{head.Commit}, Author: author, Committer: committer(author), Message: message}
		body := gitobj.EncodeCommit(commit)
		result.Head = Head{Seq: head.Seq + 1, Commit: gitobj.HashObject("commit", body)}
		for _, f := range plan.changed {
			if f != nil {
				f.Version = result.Head.Commit
			}
		}
		for _, fc := range cs.Files {
			for _, p := range []string{fc.From, fc.Path} {
				if p == "" {
					continue
				}
				if f, ok := plan.changed[p]; ok {
					if f != nil {
						result.Versions[p] = f.Version
					} else {
						result.Versions[p] = gitobj.ID{}
					}
				} else {
					result.Versions[p] = m.files[p].Version
				}
			}
		}
		if stored, err = s.storeVersions(ctx, tx, plan.refs, versions, plan.prev, staged); err != nil {
			return err
		}
		batch := &pgx.Batch{}
		queueTrees(batch, org, n.id, plan.trees)
		queueRevision(batch, org, n.id, result.Head.Seq, head.Seq, result.Head.Commit, commit, body)
		queueChanges(batch, org, n.id, result.Head.Seq, plan.changes)
		queueEntries(batch, org, n.id, m, plan.changed)
		batch.Queue(`UPDATE notebook_heads SET seq = $3, commit_sha = $4 WHERE org = $1 AND notebook = $2`, org, n.id, result.Head.Seq, result.Head.Commit[:])
		if cs.OperationID != "" {
			raw, _ := json.Marshal(result)
			queueReceipt(batch, org, n.id, Receipt{Kind: commitReceipt, Operation: cs.OperationID, RequestHash: request, State: ReceiptDone, Result: raw})
		}
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return err
		}
		next = m.derive(result.Head, plan.changed, plan.dirs)
		return nil
	}
	if err := s.writeTx(ctx, attempt, staged); err != nil {
		return CommitResult{}, err
	}
	if next != nil {
		for _, v := range stored {
			s.cacheVersion(v)
		}
		n.committed(next)
	}
	return result, nil
}

// writeTx runs a write's transaction. When the transaction finds blocks it
// must put, it rolls back; writeTx puts them and runs it again. Another
// gateway may commit before every try, each time to a file that lacks other
// blocks, so after a second such try writeTx puts every block of the write's
// new content not put yet, and the next try lacks none. Every try that rolls
// back puts a block no earlier try put, of the finitely many the write has,
// so the loop ends; only a real failure ends the write with an error.
func (s *Store) writeTx(ctx context.Context, fn func(pgx.Tx) error, staged map[[32]byte]bool) error {
	for tries := 1; ; tries++ {
		err := pgx.BeginFunc(ctx, s.db.Pool, fn)
		var unstaged *unstagedError
		if !errors.As(err, &unstaged) {
			return err
		}
		blocks := unstaged.blocks
		if tries > 1 {
			blocks = unstaged.all
		}
		if err := s.stage(ctx, blocks, staged); err != nil {
			return err
		}
	}
}

// prestage puts, before the write's transaction, the blocks of new large
// contents that the file each replaces, as the read model has it, lacks. The
// transaction stages whatever a newer head needs besides.
func (n *Notebook) prestage(ctx context.Context, files []FileChange, prepared []*version, staged map[[32]byte]bool) error {
	large := false
	for _, v := range prepared {
		large = large || v != nil && !v.inline()
	}
	if !large {
		return nil
	}
	m, err := n.current(ctx)
	if err != nil {
		return nil // the transaction reports it
	}
	var blocks []pendingBlock
	seen := map[[32]byte]bool{}
	for i, fc := range files {
		v := prepared[i]
		if v == nil || v.inline() {
			continue
		}
		base := fc.Path
		if fc.From != "" {
			base = fc.From
		}
		shared := map[[32]byte]bool{}
		if f, ok := m.files[base]; ok && f.Blob == v.blob {
			continue
		} else if ok && f.Mode != gitobj.ModeGitlink {
			if old, err := n.s.manifest(ctx, n.s.db.Pool, f.Blob); err == nil {
				for _, r := range old.blocks {
					shared[r.sha] = true
				}
			}
		}
		for j, r := range v.refs {
			if !shared[r.sha] && !seen[r.sha] {
				seen[r.sha] = true
				blocks = append(blocks, pendingBlock{r, v.blocks[j]})
			}
		}
	}
	return n.s.stage(ctx, blocks, staged)
}

// putReceipt records result, of a change set that committed nothing, as the
// answer to its operation, if it has one, so a retry after another write
// changes nothing either.
func (n *Notebook) putReceipt(ctx context.Context, tx pgx.Tx, operation, request string, result CommitResult) error {
	if operation == "" {
		return nil
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	sql, args := receiptStatement(n.s.db.Org, n.id, Receipt{Kind: commitReceipt, Operation: operation, RequestHash: request, State: ReceiptDone, Result: raw})
	_, err = tx.Exec(ctx, sql, args...)
	return err
}

// lockHead locks notebook's head row and returns the head and incarnation.
func lockHead(ctx context.Context, tx pgx.Tx, org, notebook string) (Head, int64, error) {
	return scanHead(tx.QueryRow(ctx, `SELECT seq, commit_sha, incarnation FROM notebook_heads WHERE org = $1 AND notebook = $2 FOR UPDATE`, org, notebook), notebook)
}

func validEntryID(id string) bool {
	if len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return id != ""
}

func newEntryID() string {
	raw := make([]byte, 16)
	rand.Read(raw)
	return hex.EncodeToString(raw)
}

func requestHash(cs ChangeSet, prepared []*version, author gitobj.Signature) string {
	type file struct {
		Path, From, EntryID, Blob string
		Mode                      uint32
		Delete                    bool
	}
	digest := struct {
		ExpectHead    string
		ExpectVersion map[string]gitobj.ID
		Files         []file
		Name, Email   string
		Subject, Body string
		Trailers      []gitobj.Trailer
		AllowEmpty    bool
	}{ExpectVersion: cs.ExpectVersion, Name: author.Name, Email: author.Email, Subject: cs.Subject, Body: cs.Body, Trailers: cs.Trailers, AllowEmpty: cs.AllowEmpty}
	if cs.ExpectHead != nil {
		digest.ExpectHead = cs.ExpectHead.String()
	}
	for i, fc := range cs.Files {
		f := file{Path: fc.Path, From: fc.From, EntryID: fc.EntryID, Mode: fc.Mode, Delete: fc.Content == nil && fc.From == ""}
		if prepared[i] != nil {
			f.Blob = prepared[i].blob.String()
		}
		digest.Files = append(digest.Files, f)
	}
	raw, _ := json.Marshal(digest)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// A writePlan is what a change set does to a read model.
type writePlan struct {
	changed map[string]*File    // final file of each changed path; nil removes it
	changes []Change            // net, by entry
	dirs    map[string]*dirNode // replaced directories; nil removes one
	trees   map[gitobj.ID][]byte
	root    gitobj.ID
	prev    map[gitobj.ID]gitobj.ID // new blob to the blob it replaced
	refs    []gitobj.ID             // blobs the new changes reference
}

func planWrite(m *model, files []FileChange, prepared []*version) (*writePlan, error) {
	overlay := map[string]*File{}
	entryAt := map[string]string{}
	lookup := func(p string) (File, bool) {
		if f, ok := overlay[p]; ok {
			if f == nil {
				return File{}, false
			}
			return *f, true
		}
		f, ok := m.files[p]
		return f, ok
	}
	live := func(entry string) bool {
		if p, ok := entryAt[entry]; ok {
			return p != ""
		}
		_, ok := m.byEntry[entry]
		return ok
	}
	set := func(p string, f *File) {
		if old, ok := lookup(p); ok {
			entryAt[old.EntryID] = ""
		}
		overlay[p] = f
		if f != nil {
			entryAt[f.EntryID] = p
		}
	}
	prev := map[gitobj.ID]gitobj.ID{}
	for i, fc := range files {
		v := prepared[i]
		switch {
		case fc.From != "":
			src, ok := lookup(fc.From)
			if !ok {
				return nil, fmt.Errorf("rename source %s: %w", fc.From, fs.ErrNotExist)
			}
			if _, exists := lookup(fc.Path); exists {
				return nil, &ConflictError{Path: fc.Path, Reason: "a file already exists there"}
			}
			if fc.EntryID != "" && fc.EntryID != src.EntryID {
				return nil, invalid("a rename keeps entry %s", src.EntryID)
			}
			moved := src
			moved.Path = fc.Path
			if v != nil {
				moved.Blob, moved.Size = v.blob, int64(len(v.data))
				prev[v.blob] = src.Blob
			}
			if fc.Mode != 0 {
				moved.Mode = fc.Mode
			} else if v != nil && moved.Mode == gitobj.ModeGitlink {
				moved.Mode = gitobj.ModeFile
			}
			set(fc.From, nil)
			set(fc.Path, &moved)
		case v == nil:
			if _, ok := lookup(fc.Path); !ok {
				return nil, fmt.Errorf("%s: %w", fc.Path, fs.ErrNotExist)
			}
			set(fc.Path, nil)
		default:
			f := File{Path: fc.Path, Blob: v.blob, Size: int64(len(v.data)), Mode: fc.Mode}
			if old, exists := lookup(fc.Path); exists {
				if fc.EntryID != "" && fc.EntryID != old.EntryID {
					return nil, invalid("%s already has entry %s", fc.Path, old.EntryID)
				}
				f.EntryID = old.EntryID
				if f.Mode == 0 && old.Mode != gitobj.ModeGitlink {
					f.Mode = old.Mode
				} else if f.Mode == 0 {
					f.Mode = gitobj.ModeFile // content replaces a submodule entry
				}
				if old.Blob != v.blob {
					prev[v.blob] = old.Blob
				}
			} else {
				f.EntryID = fc.EntryID
				if f.EntryID == "" {
					f.EntryID = newEntryID()
				} else if live(f.EntryID) {
					return nil, &ConflictError{Path: fc.Path, Reason: "entry " + f.EntryID + " is already in use"}
				}
				if f.Mode == 0 {
					f.Mode = gitobj.ModeFile
				}
			}
			set(fc.Path, &f)
		}
	}
	plan := &writePlan{changed: map[string]*File{}, prev: prev, trees: map[gitobj.ID][]byte{}}
	before, after := map[string]File{}, map[string]File{}
	for p, f := range overlay {
		old, existed := m.files[p]
		if existed {
			before[old.EntryID] = old
		}
		if f != nil {
			after[f.EntryID] = *f
		}
		if f == nil && !existed || f != nil && existed && f.EntryID == old.EntryID && f.Blob == old.Blob && f.Mode == old.Mode {
			continue
		}
		plan.changed[p] = f
	}
	refs := map[gitobj.ID]bool{}
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
		case a.Blob != b.Blob || a.Mode != b.Mode:
			c.Op = OpModify
		default:
			continue
		}
		if c.Op != OpDelete {
			refs[c.Blob] = true
		}
		plan.changes = append(plan.changes, c)
	}
	slices.SortFunc(plan.changes, func(x, y Change) int { return strings.Compare(x.Path, y.Path) })
	plan.refs = slices.Collect(maps.Keys(refs))
	if err := buildTrees(m, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

func unionKeys[V any](a, b map[string]V) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// buildTrees re-encodes the directories along the changed paths, deepest
// first, and records the new root.
func buildTrees(m *model, plan *writePlan) error {
	names := map[string]map[string]*File{} // dir to its changed files by name
	children := map[string]map[string]bool{}
	for p, f := range plan.changed {
		dir := parentDir(p)
		if names[dir] == nil {
			names[dir] = map[string]*File{}
		}
		names[dir][path.Base(p)] = f
		for d := dir; d != ""; {
			parent := parentDir(d)
			if children[parent] == nil {
				children[parent] = map[string]bool{}
			}
			if children[parent][d] {
				break
			}
			children[parent][d] = true
			d = parent
		}
	}
	dirty := map[string]bool{"": true}
	for d := range names {
		dirty[d] = true
	}
	for _, kids := range children {
		for d := range kids {
			dirty[d] = true
		}
	}
	order := slices.Collect(maps.Keys(dirty))
	depth := func(d string) int {
		if d == "" {
			return 0
		}
		return strings.Count(d, "/") + 1
	}
	slices.SortFunc(order, func(a, b string) int { return depth(b) - depth(a) })
	plan.dirs = map[string]*dirNode{}
	for _, d := range order {
		entries := map[string]gitobj.TreeEntry{}
		if node := m.dirs[d]; node != nil {
			for _, e := range node.entries {
				entries[e.Name] = e
			}
		}
		for name, f := range names[d] {
			if e, ok := entries[name]; f == nil && ok && !e.IsTree() {
				delete(entries, name)
			}
		}
		for child := range children[d] {
			name := path.Base(child)
			if e, ok := entries[name]; ok && !e.IsTree() {
				return &ConflictError{Path: joinPath(d, name), Reason: "a file is where a folder is needed"}
			}
			if node := plan.dirs[child]; node == nil {
				delete(entries, name)
			} else {
				entries[name] = gitobj.TreeEntry{Name: name, Mode: gitobj.ModeTree, ID: node.id}
			}
		}
		for name, f := range names[d] {
			if f == nil {
				continue
			}
			if e, ok := entries[name]; ok && e.IsTree() {
				return &ConflictError{Path: joinPath(d, name), Reason: "a folder is where a file is needed"}
			}
			entries[name] = gitobj.TreeEntry{Name: name, Mode: f.Mode, ID: f.Blob}
		}
		if len(entries) == 0 && d != "" {
			plan.dirs[d] = nil
			continue
		}
		list := slices.Collect(maps.Values(entries))
		sortEntries(list)
		body := gitobj.EncodeTree(list)
		id := gitobj.HashObject("tree", body)
		plan.trees[id] = body
		plan.dirs[d] = &dirNode{id: id, entries: list}
	}
	// A removed directory's subdirectories go with it.
	for d, node := range plan.dirs {
		if node == nil {
			for sub := range m.dirs {
				if strings.HasPrefix(sub, d+"/") {
					plan.dirs[sub] = nil
				}
			}
		}
	}
	plan.root = plan.dirs[""].id
	return nil
}

func queueTrees(b *pgx.Batch, org, notebook string, trees map[gitobj.ID][]byte) {
	if len(trees) == 0 {
		return
	}
	var shas, bodies [][]byte
	for id, body := range trees {
		shas = append(shas, id[:])
		if body == nil {
			body = []byte{}
		}
		bodies = append(bodies, body)
	}
	b.Queue(`INSERT INTO git_trees (org, notebook, tree_sha, body) SELECT $1, $2, * FROM unnest($3::bytea[], $4::bytea[]) ON CONFLICT DO NOTHING`, org, notebook, shas, bodies)
}

func queueRevision(b *pgx.Batch, org, notebook string, seq, parentSeq int64, id gitobj.ID, c gitobj.Commit, body []byte) {
	fields := map[string]*string{}
	for _, t := range gitobj.Trailers(c.Message) {
		value := t.Value
		fields[t.Key] = &value
	}
	var parent *int64
	if parentSeq > 0 {
		parent = &parentSeq
	}
	b.Queue(`INSERT INTO revisions (org, notebook, seq, commit_sha, parent_seq, tree_sha, commit_body, committed_at, author_name, author_email, actor, source, client, operation_id, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		org, notebook, seq, id[:], parent, c.Tree[:], body, c.Committer.Time(), c.Author.Name, c.Author.Email,
		fields["Gitnotes-User"], fields["Gitnotes-Source"], fields["Gitnotes-Client"], fields["Gitnotes-Operation"], fields["Gitnotes-Request"])
}

func insertRevision(ctx context.Context, tx pgx.Tx, org, notebook string, seq, parentSeq int64, id gitobj.ID, c gitobj.Commit, body []byte) error {
	b := &pgx.Batch{}
	queueRevision(b, org, notebook, seq, parentSeq, id, c, body)
	return tx.SendBatch(ctx, b).Close()
}

func queueChanges(b *pgx.Batch, org, notebook string, seq int64, changes []Change) {
	if len(changes) == 0 {
		return
	}
	entries, ops, paths, oldPaths := make([]string, len(changes)), make([]string, len(changes)), make([]string, len(changes)), make([]string, len(changes))
	blobs, modes, sizes := make([][]byte, len(changes)), make([]int32, len(changes)), make([]int64, len(changes))
	for i, c := range changes {
		entries[i], ops[i], paths[i], oldPaths[i] = c.EntryID, c.Op, c.Path, c.OldPath
		blobs[i], modes[i], sizes[i] = idOrNil(c.Blob), int32(c.Mode), c.Size
		if c.Op == OpDelete {
			blobs[i] = nil
		}
	}
	b.Queue(`INSERT INTO revision_changes (org, notebook, seq, entry_id, op, path, old_path, blob_sha, mode, size)
		SELECT $1, $2, $3, e, o, p, NULLIF(op_, ''), bl, m, sz FROM unnest($4::text[], $5::text[], $6::text[], $7::text[], $8::bytea[], $9::integer[], $10::bigint[]) AS u(e, o, p, op_, bl, m, sz)`,
		org, notebook, seq, entries, ops, paths, oldPaths, blobs, modes, sizes)
}

// queueEntries replaces the entries rows of the changed paths, and of the
// entries that left them.
func queueEntries(b *pgx.Batch, org, notebook string, m *model, changed map[string]*File) {
	var gone, paths []string
	var files []*File
	for p, f := range changed {
		paths = append(paths, p)
		if old, ok := m.files[p]; ok {
			gone = append(gone, old.EntryID)
		}
		if f != nil {
			gone = append(gone, f.EntryID)
			files = append(files, f)
		}
	}
	b.Queue(`DELETE FROM entries WHERE org = $1 AND notebook = $2 AND (entry_id = ANY($3) OR path = ANY($4))`, org, notebook, gone, paths)
	queueEntryRows(b, org, notebook, files)
}

func queueEntryRows(b *pgx.Batch, org, notebook string, files []*File) {
	if len(files) == 0 {
		return
	}
	ids, paths, kinds := make([]string, len(files)), make([]string, len(files)), make([]string, len(files))
	blobs, versions := make([][]byte, len(files)), make([][]byte, len(files))
	modes, sizes := make([]int32, len(files)), make([]int64, len(files))
	for i, f := range files {
		ids[i], paths[i], kinds[i] = f.EntryID, f.Path, entryKind(f.Path)
		blobs[i], versions[i] = bytesOf(f.Blob), bytesOf(f.Version)
		modes[i], sizes[i] = int32(f.Mode), f.Size
	}
	b.Queue(`INSERT INTO entries (org, notebook, entry_id, path, blob_sha, mode, size, version, kind)
		SELECT $1, $2, * FROM unnest($3::text[], $4::text[], $5::bytea[], $6::integer[], $7::bigint[], $8::bytea[], $9::text[])`,
		org, notebook, ids, paths, blobs, modes, sizes, versions, kinds)
}

func bytesOf(id gitobj.ID) []byte { return append([]byte(nil), id[:]...) }

// entryKind classifies a path for queries; it is derived, not authoritative.
func entryKind(p string) string {
	switch {
	case strings.HasPrefix(p, "comments/"):
		return "comments"
	case strings.HasPrefix(path.Base(p), ".gitnotes-") || p == "AGENTS.md":
		return "system"
	case isMarkdown(p):
		return "note"
	default:
		return "asset"
	}
}
