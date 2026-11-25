package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"forgetmenote/internal/capacity"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// The tree reads of a notebook kept in the notes store, which keep the
// contracts of the Git backend's helpers they replaced; the notes store
// answers the head and its files from memory and history from Postgres.

// parseRevision reads a revision string, "" meaning the head, as the notes
// store's zero ID does; Git's all-zero ID names no revision.
func parseRevision(revision string) (gitobj.ID, error) {
	if revision == "" {
		return gitobj.ID{}, nil
	}
	if !revisionPattern.MatchString(revision) {
		return gitobj.ID{}, ErrForbidden
	}
	id, err := gitobj.ParseID(revision)
	if err == nil && id.IsZero() {
		err = unknownRevision(id)
	}
	return id, err
}

func unknownRevision(id gitobj.ID) error {
	return fmt.Errorf("revision %s is not in this notebook's history", id)
}

// head returns the notebook's head and tells watchers about a new one.
// Callers hold mu. A cancelled request fails, as a Git read would, even when
// the head is in memory.
func (n *dbNotebook) head(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	h, err := n.notes.Head(ctx)
	if err != nil {
		return "", err
	}
	n.observe(h, true)
	return h.Commit.String(), nil
}

// knownRevision fails unless revision, the zero ID meaning the head, is one
// of the notebook's.
func (n *dbNotebook) knownRevision(ctx context.Context, rev gitobj.ID) error {
	if rev.IsZero() {
		return nil
	}
	if h, err := n.notes.Head(ctx); err == nil && h.Commit == rev {
		return nil
	}
	_, err := n.notes.Commit(ctx, rev)
	if errors.Is(err, fs.ErrNotExist) {
		return unknownRevision(rev)
	}
	return err
}

// isAncestor reports whether revision is head or one of its ancestors: a
// revision of the notebook no newer than head.
func (n *dbNotebook) isAncestor(ctx context.Context, revision, head string) (bool, error) {
	if !revisionPattern.MatchString(revision) || !revisionPattern.MatchString(head) {
		return false, nil
	}
	if revision == head {
		return true, nil
	}
	seq := func(r string) (int64, bool, error) {
		id, _ := gitobj.ParseID(r)
		if h, err := n.notes.Head(ctx); err == nil && h.Commit == id {
			return h.Seq, true, nil
		}
		info, err := n.notes.Commit(ctx, id)
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}
		return info.Seq, err == nil, err
	}
	older, ok, err := seq(revision)
	if err != nil || !ok {
		return false, err
	}
	newer, ok, err := seq(head)
	if err != nil || !ok {
		return false, err
	}
	return older <= newer, nil
}

// fileVersion is "git log -1 --format=%H revision -- path": the revision
// that last changed path, deletions included, or "".
func (n *dbNotebook) fileVersion(ctx context.Context, path, revision string) (string, error) {
	rev, err := parseRevision(revision)
	if err != nil {
		return "", err
	}
	v, err := n.notes.PathVersion(ctx, rev, path)
	if err != nil || v.IsZero() {
		return "", err
	}
	return v.String(), nil
}

func (n *dbNotebook) read(ctx context.Context, path, revision string) (Document, error) {
	if _, err := n.resolve(path, false); err != nil {
		return Document{}, err
	}
	if revision == "" {
		var err error
		if revision, err = n.head(ctx); err != nil {
			return Document{}, err
		}
	}
	return n.readRevision(ctx, path, revision)
}

// readRevision reads path at a fixed revision. It touches
// only immutable content and the committed-read cache, so callers may read
// without mu; they still check path access and authority.
func (n *dbNotebook) readRevision(ctx context.Context, path, revision string) (Document, error) {
	if !revisionPattern.MatchString(revision) {
		return Document{}, ErrForbidden
	}
	// The incarnation the read begins in: what a revision holds stays the
	// same until a purge, on any gateway, rewrites the history.
	inc, err := n.notes.Incarnation(ctx)
	if err != nil {
		return Document{}, err
	}
	doc, found, cached := n.committedReads.get(inc, path, revision)
	if !cached {
		if doc, found, err = n.readCommitted(ctx, path, revision); err != nil {
			return Document{}, err
		}
		n.committedReads.put(inc, path, revision, doc, found)
	}
	if !found {
		return Document{}, os.ErrNotExist
	}
	// The budget is configuration, not a fact of the commit; apply the current one.
	if len(doc.Content) > MaxReadBytes(path) {
		return Document{}, errors.New("file exceeds its configured file or source byte budget")
	}
	return doc, nil
}

// readCommitted reads one regular file and the revision that last changed
// it. A path that is missing at a revision of the notebook is not found; an
// unknown revision is an error, since an import may still bring it.
func (n *dbNotebook) readCommitted(ctx context.Context, path, revision string) (Document, bool, error) {
	rev, err := parseRevision(revision)
	if err != nil {
		return Document{}, false, err
	}
	f, err := n.notes.Stat(ctx, rev, path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := n.knownRevision(ctx, rev); err != nil {
			return Document{}, false, err
		}
		dir, err := n.isDir(ctx, revision, path)
		if err != nil {
			return Document{}, false, err
		}
		if dir {
			return Document{}, false, ErrForbidden
		}
		return Document{}, false, nil
	}
	if err != nil {
		return Document{}, false, err
	}
	if f.Mode != gitobj.ModeFile {
		return Document{}, false, ErrForbidden
	}
	if f.Size > int64(MaxReadBytes(path)) {
		return Document{}, false, errors.New("file exceeds its configured file or source byte budget")
	}
	content, f, err := n.notes.Read(ctx, rev, path)
	if err != nil {
		return Document{}, false, err
	}
	return Document{Path: path, Content: content, Revision: revision, Version: f.Version.String()}, true, nil
}

// files returns every file at revision, "" meaning the head, by path.
func (n *dbNotebook) files(ctx context.Context, revision string) ([]notestore.File, error) {
	rev, err := parseRevision(revision)
	if err != nil {
		return nil, err
	}
	return n.notes.Files(ctx, rev)
}

// exists reports whether a file is at path in revision; it replaces the
// working tree's os.Lstat.
func (n *dbNotebook) exists(ctx context.Context, revision, path string) (bool, error) {
	rev, err := parseRevision(revision)
	if err != nil {
		return false, err
	}
	_, err = n.notes.Stat(ctx, rev, path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// isDir reports whether path is a folder in revision: whether any file lies
// under it. It copies no file listing, so a mutation may ask for each of
// thousands of paths.
func (n *dbNotebook) isDir(ctx context.Context, revision, path string) (bool, error) {
	rev, err := parseRevision(revision)
	if err != nil {
		return false, err
	}
	return n.notes.HasUnder(ctx, rev, path)
}

// readBlobs reads files' contents in order, each within MaxFileBytes.
func (n *dbNotebook) readBlobs(ctx context.Context, files []notestore.File, consume func(int, []byte)) error {
	limits := make([]int, len(files))
	for i := range limits {
		limits[i] = MaxFileBytes()
	}
	return n.readBlobsBounded(ctx, files, limits, consume)
}

// readBlobsBounded reads files' contents in order, refusing any larger than
// its limit before reading it. Callers select the files from one listing.
func (n *dbNotebook) readBlobsBounded(ctx context.Context, files []notestore.File, limits []int, consume func(int, []byte)) error {
	blobs := make([]gitobj.ID, len(files))
	for i, f := range files {
		if f.Size < 0 || f.Size > int64(limits[i]) {
			return errors.New("invalid snapshot size")
		}
		blobs[i] = f.Blob
	}
	return n.notes.ReadBlobs(ctx, blobs, func(i int, raw []byte) error {
		if len(raw) > limits[i] {
			return errors.New("invalid snapshot size")
		}
		consume(i, raw)
		return nil
	})
}

// readCommittedContents batches the immutable reads one mutation selects.
func (n *dbNotebook) readCommittedContents(ctx context.Context, revision string, selectPath func(string) bool) (map[string][]byte, error) {
	contents := map[string][]byte{}
	err := n.forEachCommittedContent(ctx, revision, selectPath, func(path string, content []byte) error {
		contents[path] = content
		return nil
	})
	return contents, err
}

// forEachCommittedContent hands consume each selected regular file of
// revision in turn; any other kind of file selected is forbidden.
func (n *dbNotebook) forEachCommittedContent(ctx context.Context, revision string, selectPath func(string) bool, consume func(string, []byte) error) error {
	files, err := n.files(ctx, revision)
	if err != nil {
		return err
	}
	var selected []notestore.File
	var limits []int
	for _, f := range files {
		if !selectPath(f.Path) {
			continue
		}
		if _, err := n.resolve(f.Path, false); err != nil {
			return err
		}
		if f.Mode != gitobj.ModeFile {
			return ErrForbidden
		}
		limit := MaxReadBytes(f.Path)
		if f.Size > int64(limit) {
			return errors.New("file exceeds its configured file or source byte budget")
		}
		selected = append(selected, f)
		limits = append(limits, limit)
	}
	var consumeErr error
	err = n.readBlobsBounded(ctx, selected, limits, func(i int, raw []byte) {
		if consumeErr == nil {
			consumeErr = consume(selected[i].Path, raw)
		}
	})
	return errors.Join(err, consumeErr)
}

// forEachRequiredContent is forEachCommittedContent for paths that must all
// exist.
func (n *dbNotebook) forEachRequiredContent(ctx context.Context, revision string, required map[string]bool, consume func(string, []byte) error) error {
	remaining := len(required)
	err := n.forEachCommittedContent(ctx, revision, func(path string) bool { return required[path] }, func(path string, content []byte) error {
		remaining--
		return consume(path, content)
	})
	if err == nil && remaining != 0 {
		return os.ErrNotExist
	}
	return err
}

func (n *dbNotebook) snapshotRevision(ctx context.Context, revision string) (string, error) {
	if revision == "" {
		return n.head(ctx)
	}
	rev, err := parseRevision(revision)
	if err != nil {
		return "", err
	}
	if n.knownRevision(ctx, rev) != nil {
		return "", errors.New("snapshot commit is unavailable")
	}
	return revision, nil
}

func (n *dbNotebook) snapshotSelection(ctx context.Context, revision string) (snapshotSelection, error) {
	revision, err := n.snapshotRevision(ctx, revision)
	if err != nil {
		return snapshotSelection{}, err
	}
	trash, err := n.loadTrash(ctx)
	if err != nil {
		return snapshotSelection{}, err
	}
	exclusions := append([]string(nil), n.browseExcluded...)
	selection := snapshotSelection{revision: revision, visibility: historyVisibilityID(exclusions), exclusions: exclusions, visibleTrash: map[string]bool{}, generation: n.authorityGeneration.Load()}
	for _, record := range trash.Records {
		if !record.Emptied && !record.Restored && !matchesPrefix(record.OriginalPath, exclusions) {
			selection.visibleTrash[record.Path] = true
		}
	}
	return selection, nil
}

func (n *dbNotebook) snapshot(ctx context.Context, revision string) (Snapshot, error) {
	return n.snapshotWithScope(ctx, revision, nil)
}

func (n *dbNotebook) snapshotWithScope(ctx context.Context, revision string, scope *ContentScope) (Snapshot, error) {
	selection, err := n.snapshotSelection(ctx, revision)
	if err != nil {
		return Snapshot{}, err
	}
	return n.readSnapshot(ctx, selection, scope)
}

// readSnapshot reads the visible notes of a selection, in Git's ls-tree
// order, which is the files' byte order of paths, within the same budgets.
func (n *dbNotebook) readSnapshot(ctx context.Context, selection snapshotSelection, scope *ContentScope) (Snapshot, error) {
	result := Snapshot{Revision: selection.revision, Visibility: selection.visibility, Files: []SnapshotFile{}}
	if scope != nil {
		result.ScopeID = scope.ID()
	}
	files, err := n.files(ctx, selection.revision)
	if err != nil {
		return result, err
	}
	var selected []notestore.File
	scanned := 0
	scanBudget := MaxSnapshotBytes()
	if scope != nil {
		scanBudget *= 4
	}
	for _, f := range files {
		path := f.Path
		if !n.visibleNoteWithExclusions(path, selection.exclusions) || scope != nil && !scope.AllowsPath(path) || IsTrash(path) && !selection.visibleTrash[path] {
			continue
		}
		if f.Mode != gitobj.ModeFile {
			continue
		}
		if f.Size < 0 || f.Size > int64(MaxFileBytes()) {
			return result, errors.New("snapshot note exceeds file limit")
		}
		scanned += int(f.Size)
		if scanned > scanBudget || len(selected) > scanBudget/128 {
			return result, capacity.IndexBytes.Error()
		}
		selected = append(selected, f)
	}
	total := 0
	var selectionErr error
	err = n.readBlobs(ctx, selected, func(i int, raw []byte) {
		metadata, _ := ParseMetadata(raw)
		if scope != nil && scope.Tier(metadata) < 0 {
			return
		}
		total += len(raw)
		if total > MaxSnapshotBytes() {
			selectionErr = capacity.IndexBytes.Error()
			return
		}
		tier := 0
		if scope != nil {
			tier = scope.Tier(metadata)
		}
		result.Files = append(result.Files, SnapshotFile{Priority: tier, Path: selected[i].Path, Content: string(raw), Hash: ContentHash(raw), Metadata: metadata})
	})
	if err != nil || selectionErr != nil {
		return Snapshot{}, errors.Join(err, selectionErr)
	}
	if scope != nil {
		sort.SliceStable(result.Files, func(i, j int) bool {
			return scope.Tier(result.Files[i].Metadata) < scope.Tier(result.Files[j].Metadata)
		})
	}
	return result, nil
}

// policyFile reads a service-managed policy file at revision, refusing any
// but a regular file of at most limit bytes. It returns nil when there is
// none, with the file's blob either way.
func (n *dbNotebook) policyFile(ctx context.Context, revision, path string, limit int64, tooLarge string) ([]byte, notestore.File, error) {
	rev, err := parseRevision(revision)
	if err != nil {
		return nil, notestore.File{}, err
	}
	f, err := n.notes.Stat(ctx, rev, path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, notestore.File{}, nil
	}
	if err != nil {
		return nil, f, err
	}
	if f.Mode != gitobj.ModeFile {
		return nil, f, ErrForbidden
	}
	if f.Size > limit {
		return nil, f, errors.New(tooLarge)
	}
	raw, _, err := n.notes.Read(ctx, rev, path)
	return raw, f, err
}

// loadLocks loads only committed rules, as the Git backend did, reparsing the
// policy only when its content changed. Callers hold mu.
func (n *dbNotebook) loadLocks(ctx context.Context) error {
	revision, err := n.head(ctx)
	if err != nil {
		return err
	}
	if revision == n.locksRevision {
		for _, path := range n.projectLocks {
			if _, err := n.resolve(path, false); err != nil {
				return err
			}
		}
		return nil
	}
	rev, err := parseRevision(revision)
	if err != nil {
		return err
	}
	f, err := n.notes.Stat(ctx, rev, locksPath)
	if errors.Is(err, fs.ErrNotExist) {
		n.projectLocks, n.locksRevision, n.locksBlob = nil, revision, gitobj.ID{}
		return nil
	}
	if err != nil {
		return err
	}
	if f.Blob == n.locksBlob && f.Mode == gitobj.ModeFile {
		n.locksRevision = revision
		return nil
	}
	// Policies are deliberately small and bounded, including imported repositories.
	raw, f, err := n.policyFile(ctx, revision, locksPath, maxLockPolicyBytes, "lock policy exceeds 64 KiB")
	if err != nil {
		return err
	}
	paths, err := n.parseLockPolicy(raw)
	if err != nil {
		return err
	}
	n.projectLocks, n.locksRevision, n.locksBlob = paths, revision, f.Blob
	return nil
}

// checkLockRulesAt fails with ErrConflict unless the lock policy at base is
// the one loadLocks last read, whose rules the caller's checks used. Callers
// hold mu.
func (n *dbNotebook) checkLockRulesAt(ctx context.Context, base gitobj.ID) error {
	if base.String() == n.locksRevision {
		return nil
	}
	// No policy is the zero blob, as loadLocks records it; loadLocks
	// refuses a policy that is no regular file, so no rules came from one.
	var blob gitobj.ID
	f, err := n.notes.Stat(ctx, base, locksPath)
	switch {
	case err == nil:
		blob = f.Blob
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if blob != n.locksBlob || err == nil && f.Mode != gitobj.ModeFile {
		return fmt.Errorf("the lock rules changed: %w", ErrConflict)
	}
	return nil
}

// lockState is the rules covering path and the head they were read at, the
// revision a change of them commits on.
func (n *dbNotebook) lockState(ctx context.Context, path string) (LockState, error) {
	revision, err := n.mutateBase(ctx)
	if err != nil {
		return LockState{}, err
	}
	return n.lockRules(path, revision), nil
}

// loadTrash returns the committed Trash index, as the Git backend did,
// reparsing it only when its content changed. Callers hold mu.
func (n *dbNotebook) loadTrash(ctx context.Context) (TrashIndex, error) {
	empty := TrashIndex{Version: 1, Records: []TrashRecord{}}
	revision, err := n.head(ctx)
	if err != nil {
		return empty, err
	}
	if revision == n.trashRevision {
		index := cloneTrashIndex(n.trashCache)
		return index, damaged(trashIndexPath, n.validateTrash(index))
	}
	rev, err := parseRevision(revision)
	if err != nil {
		return empty, err
	}
	if empty.archives, err = n.countTrashArchives(ctx, rev); err != nil {
		return empty, err
	}
	f, err := n.notes.Stat(ctx, rev, trashIndexPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		file, err := n.exists(ctx, revision, "Trash")
		if err != nil {
			return empty, err
		}
		dir, err := n.isDir(ctx, revision, "Trash")
		if err != nil {
			return empty, err
		}
		if file || dir {
			return empty, errors.New("existing Trash content is not system managed; relocate it before enabling Trash")
		}
		n.trashRevision, n.trashCache, n.trashBlob = revision, empty, gitobj.ID{}
		return cloneTrashIndex(empty), nil
	case err != nil:
		return empty, err
	case f.Blob == n.trashBlob && f.Mode == gitobj.ModeFile:
		n.trashRevision, n.trashCache.archives = revision, empty.archives
		index := cloneTrashIndex(n.trashCache)
		return index, damaged(trashIndexPath, n.validateTrash(index))
	}
	raw, f, err := n.policyFile(ctx, revision, trashIndexPath, trashFileBytes, "Trash index exceeds 8 MiB")
	if err != nil {
		return empty, err
	}
	index, err := parseTrashIndex(raw)
	if err != nil {
		return empty, damaged(trashIndexPath, err)
	}
	for i := range index.Records {
		index.Records[i].Hidden = false
	}
	if err := n.validateTrash(index); err != nil {
		return empty, damaged(trashIndexPath, err)
	}
	index.archives = empty.archives
	n.trashRevision, n.trashCache, n.trashBlob = revision, cloneTrashIndex(index), f.Blob
	return index, nil
}

// identityIndex maps each note at the head to its fmn-id, rereading only the
// notes whose content changed. Callers hold mu.
func (n *dbNotebook) identityIndex(ctx context.Context) error {
	head, err := n.head(ctx)
	if err != nil {
		return err
	}
	if n.identityHead == head {
		return nil
	}
	files, err := n.files(ctx, head)
	if err != nil {
		return err
	}
	next, objects := map[string]string{}, map[string]string{}
	var reads []notestore.File
	for _, f := range files {
		if !strings.HasSuffix(f.Path, ".md") || f.Path == "AGENTS.md" || f.Mode != gitobj.ModeFile && f.Mode != gitobj.ModeExecutable {
			continue
		}
		blob := f.Blob.String()
		objects[f.Path] = blob
		if n.identityObjects[f.Path] == blob {
			next[f.Path] = n.identityIDs[f.Path]
		} else {
			reads = append(reads, f)
		}
	}
	err = n.readBlobs(ctx, reads, func(i int, b []byte) {
		if id, e := NoteID(b); e == nil {
			next[reads[i].Path] = id
		}
	})
	if err != nil {
		return err
	}
	n.identityHead, n.identityIDs, n.identityObjects = head, next, objects
	return nil
}

// prepareIdentities refuses changes that would alter a note's fmn-id or
// give two notes one, as the Git backend did.
func (n *dbNotebook) prepareIdentities(ctx context.Context, changes map[string][]byte) error {
	notes := false
	for p, b := range changes {
		if b != nil && strings.HasSuffix(p, ".md") && p != "AGENTS.md" {
			notes = true
			break
		}
	}
	if !notes {
		return nil
	}
	if err := n.identityIndex(ctx); err != nil {
		return err
	}
	used := map[string]bool{}
	for p, id := range n.identityIDs {
		if _, changing := changes[p]; !changing && id != "" {
			used[id] = true
		}
	}
	for p, content := range changes {
		if content == nil || !strings.HasSuffix(p, ".md") || p == "AGENTS.md" {
			continue
		}
		id, err := NoteID(content)
		if err != nil {
			if n.identityIDs[p] == "" && !hasFrontMatterField(content, "fmn-id") {
				continue
			}
			return err
		}
		old := n.identityIDs[p]
		if old != "" && old != id {
			return invalidInput("fmn-id is immutable; duplicate the note to create a new identity")
		}
		if id == "" {
			continue
		}
		if used[id] {
			return invalidInput("fmn-id is already used by another note in this notebook")
		}
		used[id] = true
	}
	return nil
}
