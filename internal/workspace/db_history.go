package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// Note, folder, notebook and task history of a notebook kept in the notes
// store. Each method answers as the Git backend's log, diff-tree and
// log --grep did, from the revisions and changes the store keeps. Like the
// Git backend's they read only immutable revisions, so they hold mu only to fix
// the head and to check paths and authority.

const (
	// historyLimit is the most revisions a note's, folder's or notebook's
	// history lists.
	historyLimit = 50
	// historyPage is the most revisions the first step of a scoped or whole
	// path walk loads; each further step loads twice as many, up to
	// historyPageMax, as a walk that has not found enough eligible versions
	// yet, or lists them all, may well go on.
	historyPage    = 64
	historyPageMax = 1024
	// taskHistoryLimit is the most revisions TaskHistory searches.
	taskHistoryLimit = 200
	// historyRevisionsLimit is the most revisions HistoryRevisions lists.
	historyRevisionsLimit = 200000
)

func (n *dbNotebook) History(ctx context.Context, path string) ([]Commit, error) {
	return n.committedHistoryAt(ctx, path, "", nil, historyLimit)
}

// WholeHistory is the whole history of which History lists the newest 50, as
// of revision, or of the head when revision is empty, for admin storage
// check, which compares every entry with the repository's at the
// repository's HEAD. No request lists a history whole.
func (n *dbNotebook) WholeHistory(ctx context.Context, path, revision string) ([]Commit, error) {
	if revision != "" && !revisionPattern.MatchString(revision) {
		return nil, ErrForbidden
	}
	return n.committedHistoryAt(ctx, path, revision, nil, 0)
}

// HistoryScoped filters version-local postimages before the 50-result limit.
func (n *dbNotebook) HistoryScoped(ctx context.Context, path string, scope ContentScope) ([]Commit, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if path == "AGENTS.md" || IsTrash(path) || !strings.HasSuffix(path, ".md") {
		return nil, ErrForbidden
	}
	return n.committedHistoryAt(ctx, path, "", &scope, historyLimit)
}

// historyHead is head with its seq, which bounds every history query to the
// revision the caller saw. Callers hold mu.
func (n *dbNotebook) historyHead(ctx context.Context) (notestore.Head, error) {
	if err := ctx.Err(); err != nil {
		return notestore.Head{}, err
	}
	h, err := n.notes.Head(ctx)
	if err != nil {
		return notestore.Head{}, err
	}
	n.observe(h, true)
	return h, nil
}

// committedHistoryAt fixes the head under mu,
// walks immutable revisions without it, and checks authority and the path
// again before returning. A revision, if given, takes the head's place; it
// must be one of the notebook's. It lists at most limit revisions, all for 0.
func (n *dbNotebook) committedHistoryAt(ctx context.Context, path, revision string, scope *ContentScope, limit int) ([]Commit, error) {
	n.mu.Lock()
	generation := n.authorityGeneration.Load()
	if path != "" {
		if _, err := n.resolve(path, false); err != nil {
			n.mu.Unlock()
			return nil, err
		}
	}
	if scope != nil && !n.visibleNote(path) {
		n.mu.Unlock()
		return nil, ErrForbidden
	}
	exclusions := slices.Clone(n.browseExcluded)
	head, err := n.historyHead(ctx)
	n.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if revision != "" {
		seq, ok, err := n.historySeq(ctx, head, revision)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrConflict
		}
		head = notestore.Head{Seq: seq}
	}
	var results []Commit
	if path == "" {
		results, err = n.notebookHistory(ctx, head, limit)
	} else {
		results, err = n.pathHistory(ctx, head, path, scope, exclusions, limit)
	}
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if generation != n.authorityGeneration.Load() {
		return nil, ErrConflict
	}
	if path != "" {
		if _, err := n.resolve(path, false); err != nil {
			return nil, err
		}
	}
	return results, nil
}

// notebookHistory is "git log -50 head", limit being 50: the newest
// revisions, merges and the first one included; a limit of 0 lists them all.
// Revisions are newest first by seq; across an imported merge that is the
// topological order the import kept, where Git's log orders the two lines by
// commit date.
func (n *dbNotebook) notebookHistory(ctx context.Context, head notestore.Head, limit int) ([]Commit, error) {
	infos, err := n.notes.History(ctx, notestore.HistoryQuery{Before: head.Seq + 1, Limit: limit})
	if err != nil {
		return nil, err
	}
	result := []Commit{}
	for _, info := range infos {
		result = append(result, commitRecord(info))
	}
	return result, nil
}

// pathHistory is the Git backend's log walk for a note or folder: the
// revisions up to head that changed a file at or under path, newest first and
// at most limit, or all for 0, following path back through each
// "Move %q to %q" revision that changed it and, for a note, each rename.
// FMN's own moves are renames that keep a note's entry; an imported move may
// be a deletion and an addition that only its subject ties together. Git's
// log lists no changes for a merge, so no merge is listed. With a scope,
// whose limit is historyLimit, only versions whose regular-file postimage the
// scope admits are listed, filtered before the limit.
func (n *dbNotebook) pathHistory(ctx context.Context, head notestore.Head, path string, scope *ContentScope, exclusions []string, limit int) ([]Commit, error) {
	result := []Commit{}
	note := strings.HasSuffix(path, ".md")
	before := head.Seq + 1
	for page := historyPage; limit == 0 || len(result) < limit; page = min(2*page, historyPageMax) {
		size := page
		if scope == nil && limit > 0 {
			size = limit - len(result)
		}
		infos, err := n.notes.History(ctx, notestore.HistoryQuery{Prefix: path, Before: before, Limit: size})
		if err != nil {
			return nil, err
		}
		var versions []Commit
		var files []notestore.File
		moved := false
		for _, info := range infos {
			before = info.Seq
			merge := len(info.Commit.Parents) > 1
			record := commitRecord(info)
			if !merge && historyTouches(info.Changes, path) {
				if note {
					record.Path = path
				}
				if scope == nil {
					result = append(result, record)
				} else if f, ok := historyPostimage(info.Changes, path); ok && n.historyEligiblePath(path, exclusions) && f.Size <= int64(MaxReadBytes(path)) {
					record.Message = "Committed eligible note version"
					versions, files = append(versions, record), append(files, f)
				}
			}
			changes := info.Changes
			if merge {
				changes = nil
			}
			if origin := historyOrigin(commitSubject(info.Commit.Message), changes, path, note); origin != path {
				path, moved = origin, true
				break
			}
		}
		if scope != nil {
			if err := n.eligibleVersions(ctx, *scope, versions, files, &result); err != nil {
				return nil, err
			}
		}
		if !moved && len(infos) < size {
			break
		}
	}
	return result, nil
}

// historyEligiblePath is whether a scoped history may cite path's version: a
// visible note outside Trash, never the notebook instructions.
func (n *dbNotebook) historyEligiblePath(path string, exclusions []string) bool {
	// resolve reads no mutable state for this backend, so this needs no mu.
	return n.visibleNoteWithExclusions(path, exclusions) && !IsTrash(path) && path != "AGENTS.md"
}

// eligibleVersions appends to result, in order and up to the limit, the
// versions whose content the scope admits. Like the Git backend, it reads 64
// at a time, or as many as the limit could still take if more.
func (n *dbNotebook) eligibleVersions(ctx context.Context, scope ContentScope, versions []Commit, files []notestore.File, result *[]Commit) error {
	for len(files) > 0 && len(*result) < historyLimit {
		k := min(len(files), max(historyPage, historyLimit-len(*result)))
		limits := make([]int, k)
		for i := range limits {
			limits[i] = MaxReadBytes(files[i].Path)
		}
		err := n.readBlobsBounded(ctx, files[:k], limits, func(i int, content []byte) {
			metadata, _ := ParseMetadata(content)
			if len(*result) < historyLimit && scope.Tier(metadata) >= 0 {
				*result = append(*result, versions[i])
			}
		})
		if err != nil {
			return err
		}
		versions, files = versions[k:], files[k:]
	}
	return nil
}

// historyTouches is whether a revision's changes touch a file at or under
// path, either side of a rename included, as Git's name-status lists them.
func historyTouches(changes []notestore.Change, path string) bool {
	under := func(p string) bool { return p == path || strings.HasPrefix(p, path+"/") }
	for _, c := range changes {
		if under(c.Path) || c.Op == notestore.OpRename && under(c.OldPath) {
			return true
		}
	}
	return false
}

// historyPostimage is the regular file a revision left at path, as Git's raw
// diff gives its postimage mode and blob; a deletion leaves none.
func historyPostimage(changes []notestore.Change, path string) (notestore.File, bool) {
	for _, c := range changes {
		if c.Path == path && c.Op != notestore.OpDelete {
			return notestore.File{Path: path, Blob: c.Blob, Mode: c.Mode, Size: c.Size}, c.Mode == gitobj.ModeFile
		}
	}
	return notestore.File{}, false
}

// historyOrigin is where path was before a revision with this subject and
// these changes: the source of a "Move %q to %q" revision that moved it or a
// folder holding it, or for a note, the source of a rename to it.
func historyOrigin(subject string, changes []notestore.Change, path string, note bool) string {
	var from, to string
	if k, _ := fmt.Sscanf(subject, "Move %q to %q", &from, &to); k == 2 && from != "" && to != "" {
		return movedPath(path, to, from)
	}
	if note {
		for _, c := range changes {
			if c.Op == notestore.OpRename && c.Path == path {
				return c.OldPath
			}
		}
	}
	return path
}

// HistoryRevisions lists target and its ancestors newer than after, newest
// first, with their committer times. A revision's ancestors are the
// notebook's revisions before it, as isAncestor has it; after must already be
// one of target's.
func (n *dbNotebook) HistoryRevisions(ctx context.Context, target, after string) ([]HistoryRevision, error) {
	if !revisionPattern.MatchString(target) || after != "" && !revisionPattern.MatchString(after) {
		return nil, ErrForbidden
	}
	n.mu.Lock()
	head, err := n.historyHead(ctx)
	n.mu.Unlock()
	if err != nil {
		return nil, err
	}
	last, ok, err := n.historySeq(ctx, head, target)
	if err != nil {
		return nil, err
	}
	if !ok || last > head.Seq {
		return nil, ErrConflict
	}
	var first int64
	if after != "" {
		if first, ok, err = n.historySeq(ctx, head, after); err != nil {
			return nil, err
		}
		if !ok || first > last {
			return nil, ErrConflict
		}
	}
	result := []HistoryRevision{}
	for from := first; from < last; {
		infos, err := n.notes.Revisions(ctx, from, 1000)
		if err != nil {
			return nil, err
		}
		if len(infos) == 0 {
			break
		}
		for _, info := range infos {
			if info.Seq > last {
				break
			}
			result = append(result, HistoryRevision{Revision: info.ID.String(), Time: isoTime(info.Commit.Committer)})
			if len(result) > historyRevisionsLimit {
				return nil, errors.New("historical search exceeds 200000 commits")
			}
		}
		from = infos[len(infos)-1].Seq
	}
	slices.Reverse(result)
	return result, nil
}

// historySeq is revision's seq, or false when the notebook has no such
// revision.
func (n *dbNotebook) historySeq(ctx context.Context, head notestore.Head, revision string) (int64, bool, error) {
	id, err := gitobj.ParseID(revision)
	if err != nil {
		return 0, false, nil
	}
	if id == head.Commit {
		return head.Seq, true, nil
	}
	info, err := n.notes.Commit(ctx, id)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return info.Seq, true, nil
}

// HistoryNotes reads one revision's changed regular Markdown postimages
// against its first parent, in "git diff-tree -r --no-renames" order, so
// offsets stay those the Git backend handed out: a rename is its source's
// deletion and its destination's addition. Deletions have no postimage to
// cite.
func (n *dbNotebook) HistoryNotes(ctx context.Context, revision string, offset int, known map[string]SnapshotFile) (HistoryBatch, error) {
	if !revisionPattern.MatchString(revision) || offset < 0 {
		return HistoryBatch{}, ErrForbidden
	}
	// Bind every batch to the exact policy used to filter it, including empty
	// batches.
	n.mu.Lock()
	exclusions := slices.Clone(n.browseExcluded)
	n.mu.Unlock()
	rev, err := parseRevision(revision)
	if err != nil {
		return HistoryBatch{}, err
	}
	info, err := n.notes.Commit(ctx, rev)
	if err != nil {
		return HistoryBatch{}, err
	}
	changed := historyDiff(info.Changes)
	paths := slices.Sorted(maps.Keys(changed))
	result := HistoryBatch{Visibility: historyVisibilityID(exclusions), Files: []HistoryFile{}, Next: offset, Done: true}
	var reads []notestore.File
	var positions []int
	bytes := 0
	for i := offset; i < len(paths); i++ {
		if len(result.Files) >= 32 {
			result.Done = false
			break
		}
		path, c := paths[i], changed[paths[i]]
		// resolve reads no mutable state for this backend, so this needs no mu.
		if c == nil || c.Mode != gitobj.ModeFile || !n.visibleNoteWithExclusions(path, exclusions) {
			result.Next = i + 1
			continue
		}
		blob := c.Blob.String()
		if cached, ok := known[blob]; ok {
			cached.Path = path
			cached.Metadata, _ = ParseMetadata([]byte(cached.Content))
			cached.Priority = 0
			result.Files = append(result.Files, HistoryFile{SnapshotFile: cached, Blob: blob})
			result.Next = i + 1
			continue
		}
		if c.Size < 0 || c.Size > int64(MaxFileBytes()) {
			return HistoryBatch{}, errors.New("historical note exceeds FMN_LIMIT_FILE_BYTES")
		}
		if bytes+int(c.Size) > 4<<20 && len(result.Files) > 0 {
			result.Done = false
			break
		}
		bytes += int(c.Size)
		reads = append(reads, notestore.File{Path: path, Blob: c.Blob, Mode: c.Mode, Size: c.Size})
		positions = append(positions, len(result.Files))
		result.Files = append(result.Files, HistoryFile{SnapshotFile: SnapshotFile{Path: path}, Blob: blob})
		result.Next = i + 1
	}
	valid := true
	err = n.readBlobs(ctx, reads, func(i int, raw []byte) {
		i = positions[i]
		if !utf8.Valid(raw) {
			valid = false
			return
		}
		result.Files[i].Content = string(raw)
		result.Files[i].Hash = ContentHash(raw)
		result.Files[i].Metadata, _ = ParseMetadata(raw)
	})
	if !valid {
		return HistoryBatch{}, errors.New("historical note is not UTF-8")
	}
	return result, err
}

// historyDiff maps each path a revision changed to its postimage, nil for a
// path it removed, as "git diff-tree --no-renames" lists them.
func historyDiff(changes []notestore.Change) map[string]*notestore.Change {
	changed := map[string]*notestore.Change{}
	removed := func(p string) {
		if _, ok := changed[p]; !ok {
			changed[p] = nil
		}
	}
	for i := range changes {
		c := &changes[i]
		switch c.Op {
		case notestore.OpDelete:
			removed(c.Path)
		case notestore.OpRename:
			removed(c.OldPath)
			changed[c.Path] = c
		default:
			changed[c.Path] = c
		}
	}
	return changed
}

// TaskHistory lists the revisions that changed one task, newest first: of the
// newest 200 whose message names it, those whose subject starts with it, as
// task actions write them.
func (n *dbNotebook) TaskHistory(ctx context.Context, task string) ([]Commit, error) {
	if !taskName.MatchString(task) {
		return nil, invalidInput("invalid task name")
	}
	n.mu.Lock()
	head, err := n.historyHead(ctx)
	n.mu.Unlock()
	if err != nil {
		return nil, err
	}
	// The limit applies to the literal search, as Git's does; the prefix
	// check drops a longer task name that starts with this one and other
	// mentions.
	infos, err := n.notes.History(ctx, notestore.HistoryQuery{MessageContains: "Task " + task, Before: head.Seq + 1, Limit: taskHistoryLimit})
	if err != nil {
		return nil, err
	}
	out := []Commit{}
	for _, info := range infos {
		c := commitRecord(info)
		if strings.HasPrefix(c.Message, "Task "+task+" ") || strings.HasPrefix(c.Message, "Task "+task+":") {
			out = append(out, c)
		}
	}
	return out, nil
}
