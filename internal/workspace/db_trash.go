package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// The Trash for notebooks kept in the notes store: the committed index that
// loadTrash reads, the staging helpers every mutation that moves notes into
// the Trash shares, and the history a Trash record keeps. The index, the
// Trash/ paths and the discussions moved with their notes keep the Git
// backend's formats, so the export and knowledge's history filter read them
// unchanged.

// TrashPolicy is every Trash record, those archived first, as history search
// filters by them: each record's history lists the revisions that stand
// for its versions now, as a purge that rewrote them mapped them.
func (n *dbNotebook) TrashPolicy(ctx context.Context) (TrashIndex, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	index, err := n.loadTrash(ctx)
	if err == nil {
		var archived []TrashRecord
		if archived, err = n.trashArchived(ctx, n.trashRevision); err == nil && len(archived) > 0 {
			index.Records = append(cloneTrashIndex(TrashIndex{Records: archived}).Records, index.Records...)
		}
	}
	var rewrites map[gitobj.ID]gitobj.ID
	if err == nil {
		rewrites, err = n.purgeRewrites(ctx)
	}
	for i := range index.Records {
		index.Records[i].Hidden = matchesPrefix(index.Records[i].OriginalPath, n.browseExcluded) || matchesPrefix(index.Records[i].Path, n.browseExcluded)
		for p, revisions := range index.Records[i].History {
			index.Records[i].History[p] = currentRevisions(revisions, rewrites)
		}
	}
	return index, err
}

// purgeRewrites maps each revision purges rewrote to the one standing for
// it now, the zero ID for one a purge dropped. A Trash record keeps the
// revisions it was written with, and the mapping changes only with a purge,
// so it is loaded once for each incarnation of the notebook's history.
func (n *dbNotebook) purgeRewrites(ctx context.Context) (map[gitobj.ID]gitobj.ID, error) {
	inc, err := n.notes.Incarnation(ctx)
	if err != nil {
		return nil, err
	}
	if rewrites, ok := n.committedReads.purgeRewrites(inc); ok {
		return rewrites, nil
	}
	rewrites, err := n.notes.RewrittenAll(ctx)
	if err != nil {
		return nil, err
	}
	n.committedReads.keepRewrites(inc, rewrites)
	return rewrites, nil
}

// currentRevisions is revisions with each that a purge rewrote replaced by
// the one standing for it now, leaving out those a purge dropped. It never
// changes revisions.
func currentRevisions(revisions []string, rewrites map[gitobj.ID]gitobj.ID) []string {
	if len(rewrites) == 0 {
		return revisions
	}
	out := make([]string, 0, len(revisions))
	for _, rev := range revisions {
		if id, err := gitobj.ParseID(rev); err == nil {
			if now, ok := rewrites[id]; ok {
				if now.IsZero() {
					continue
				}
				rev = now.String()
			}
		}
		out = append(out, rev)
	}
	return out
}

func (n *dbNotebook) ListTrash(ctx context.Context) (TrashListing, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	index, err := n.loadTrash(ctx)
	if err != nil {
		return TrashListing{}, err
	}
	// The head loadTrash read the index at, so the listing and its
	// confirmation describe one revision even while other gateways write.
	head := n.trashRevision
	result := TrashListing{Revision: head, Entries: []TrashEntry{}}
	for _, r := range index.Records {
		if !r.Emptied && !r.Restored && !matchesPrefix(r.OriginalPath, n.browseExcluded) && !matchesPrefix(r.Path, n.browseExcluded) {
			result.Entries = append(result.Entries, r.TrashEntry)
		}
	}
	// Bind confirmation to every entry and the exact notebook revision. A stale
	// modal or MCP preview cannot empty newly added items.
	raw, _ := json.Marshal(index)
	result.Confirmation = ContentHash(append([]byte("empty-trash\x00"+head+"\x00"), raw...))
	return result, nil
}

// TrashNotes moves 1–50 notes and their discussions into the Trash at
// expected, the head, as one "Move notes to Trash" revision that keeps each
// file's identity.
func (n *dbNotebook) TrashNotes(ctx context.Context, paths []string, expected, actor string) (TrashResult, error) {
	ctx = n.mutationContext(ctx)

	n.mu.Lock()
	defer n.mu.Unlock()
	if len(paths) < 1 || len(paths) > 50 {
		return TrashResult{}, invalidInput("choose 1–50 notes")
	}
	head, err := n.mutateBase(ctx)
	if err != nil {
		return TrashResult{}, err
	}
	if head != expected {
		return TrashResult{}, ErrConflict
	}
	seen := map[string]bool{}
	for _, path := range paths {
		if seen[path] || IsTrash(path) || path == "AGENTS.md" {
			return TrashResult{}, ErrForbidden
		}
		seen[path] = true
		if err = n.resolveLeaving(path); err != nil {
			return TrashResult{}, err
		}
	}
	// Accepted live text is checkpointed before relocation, while holding the
	// notebook lock; the commit's head check refuses any other writer between
	// the revision check and the move.
	pending := n.hasLivePending()
	if err = n.clean(ctx); err != nil {
		return TrashResult{}, err
	}
	index, err := n.loadTrash(ctx)
	if err != nil {
		return TrashResult{}, err
	}
	base := trashBase(head, n.trashRevision, pending)
	prepared, err := n.prepareTrash(ctx, paths, index, n.trashRevision)
	if err != nil {
		return TrashResult{}, err
	}
	changes, renames := map[string][]byte{}, map[string]string{}
	result := TrashResult{Entries: []TrashEntry{}}
	for _, path := range paths {
		entry, e := n.stageTrash(ctx, &index, path, actor, changes, &prepared)
		if e != nil {
			return result, e
		}
		addTrashMove(renames, changes, entry.OriginalPath, entry.Path)
		result.Entries = append(result.Entries, entry)
	}
	if err = stageTrashIndex(index, len(paths), changes); err != nil {
		return result, err
	}
	committed, err := n.applyChanges(ctx, base, "Move notes to Trash", orderedChanges(changes, renames))
	if err != nil {
		return result, err
	}
	result.Revision = committed.Head.Commit.String()
	return result, nil
}

// A damagedTrash is the notebook's own Trash, in what (a file, or its files
// together), that its readers refuse for reason: damage to stored data,
// which no request caused, so it is no ErrInvalid refusal. A bundle import,
// whose bundle brought it, refuses it as the bundle's (checkImportOpens).
type damagedTrash struct {
	what   string
	reason error
}

func (d *damagedTrash) Error() string {
	return fmt.Sprintf("the notebook's Trash is damaged (%s: %v); restore the notebook from a backup", d.what, d.reason)
}

// damaged is err, the readers' refusal of the Trash in what, as damage to
// it; nil stays nil.
func damaged(what string, err error) error {
	if err == nil {
		return nil
	}
	return &damagedTrash{what, err}
}

// trashBase is the head a Trash mutation commits on: checked, the revision
// it checked expected against, unless clean checkpointed pending live edits
// on top of it, in which case it is current, the head the mutation read the
// Trash index at after clean. The checkpoint may commit on another
// gateway's newer write, so callers read the index, and everything else
// they commit, only after clean, at current: the commit's head check then
// fails, as a conflict, only another gateway's write that lands after that
// read, which the Git backend's single writer never saw.
func trashBase(checked, current string, pending bool) string {
	if pending {
		return current
	}
	return checked
}

// RestoreTrash moves the Trash entry id and its discussion back to to, its
// original path when to is empty, at expected, the head, as one "Restore
// note from Trash" revision that keeps each file's identity.
func (n *dbNotebook) RestoreTrash(ctx context.Context, id, to, expected, actor string) (TrashResult, error) {
	ctx = n.mutationContext(ctx)

	n.mu.Lock()
	defer n.mu.Unlock()
	head, err := n.mutateBase(ctx)
	if err != nil {
		return TrashResult{}, err
	}
	if head != expected {
		return TrashResult{}, ErrConflict
	}
	// Accepted live text is checkpointed first, as TrashNotes does, so the
	// index, the note and its discussion are all read at the head the
	// restore commits on: an index read before the checkpoint could miss
	// another gateway's Trash change that the checkpoint commits on top of.
	pending := n.hasLivePending()
	if err = n.clean(ctx); err != nil {
		return TrashResult{}, err
	}
	index, err := n.loadTrash(ctx)
	if err != nil {
		return TrashResult{}, err
	}
	base := trashBase(head, n.trashRevision, pending)
	for i, r := range index.Records {
		if r.ID != id || r.Emptied || r.Restored {
			continue
		}
		if to == "" {
			to = r.OriginalPath
		}
		if IsTrash(to) || to == "AGENTS.md" || !strings.HasSuffix(to, ".md") {
			return TrashResult{}, ErrForbidden
		}
		if _, err = n.resolve(to, true); err != nil {
			return TrashResult{}, err
		}
		if !n.editable(r.OriginalPath) {
			return TrashResult{}, ErrForbidden
		}
		if occupied, err := n.restoreOccupied(ctx, to); err != nil {
			return TrashResult{}, err
		} else if occupied {
			return TrashResult{}, fmt.Errorf("restore destination is occupied: %w", ErrConflict)
		}
		doc, e := n.read(ctx, r.Path, base)
		if e != nil {
			return TrashResult{}, e
		}
		changes := map[string][]byte{r.Path: nil, to: rewriteLinks(doc.Content, r.Path, to, r.Path, to)}
		// The discussion is read at the head heard now, which is base unless
		// another gateway wrote since; then the commit's head check refuses.
		if err = n.stageTrashDiscussion(ctx, r.Path, to, changes); err != nil {
			return TrashResult{}, err
		}
		index.Records[i].Restored = true
		if err = stageTrashIndex(index, 0, changes); err != nil {
			return TrashResult{}, err
		}
		renames := map[string]string{}
		addTrashMove(renames, changes, r.Path, to)
		committed, err := n.applyChanges(ctx, base, "Restore note from Trash", orderedChanges(changes, renames))
		if err != nil {
			return TrashResult{}, err
		}
		entry := r.TrashEntry
		entry.Path = to
		return TrashResult{Revision: committed.Head.Commit.String(), Entries: []TrashEntry{entry}}, nil
	}
	return TrashResult{}, os.ErrNotExist
}

// restoreOccupied reports whether a restore to to would overwrite anything
// in the head: a file or folder at to, or a file where one of its folders
// would be, which the working tree's os.Lstat refused too.
func (n *dbNotebook) restoreOccupied(ctx context.Context, to string) (bool, error) {
	if taken, err := n.trashOccupied(ctx, to); err != nil || taken {
		return taken, err
	}
	for dir := filepath.Dir(to); dir != "."; dir = filepath.Dir(dir) {
		if file, err := n.exists(ctx, "", dir); err != nil || file {
			return file, err
		}
	}
	return false, nil
}

// EmptyTrash removes every note in the Trash, and their discussions, as one
// "Empty Trash" revision, when expected is the head and confirmation the
// one ListTrash gave for it. The index keeps each record as a tombstone,
// which moves to an archive once the index fills (stageTrashIndex), and
// history keeps the notes.
func (n *dbNotebook) EmptyTrash(ctx context.Context, expected, confirmation string) (TrashResult, error) {
	ctx = n.mutationContext(ctx)

	n.mu.Lock()
	defer n.mu.Unlock()
	if _, err := n.head(ctx); err != nil {
		return TrashResult{}, err
	}
	index, err := n.loadTrash(ctx)
	if err != nil {
		return TrashResult{}, err
	}
	// The head loadTrash read the index at, which the confirmation binds.
	head := n.trashRevision
	raw, _ := json.Marshal(index)
	want := ContentHash(append([]byte("empty-trash\x00"+head+"\x00"), raw...))
	if expected != head || confirmation == "" || confirmation != want {
		return TrashResult{}, ErrConflict
	}
	if err = n.loadLocks(ctx); err != nil {
		return TrashResult{}, err
	}
	changes := map[string][]byte{}
	result := TrashResult{Revision: head, Entries: []TrashEntry{}}
	required := map[string]bool{}
	for _, record := range index.Records {
		if !record.Emptied && !record.Restored {
			required[record.Path] = true
		}
	}
	if err = n.forEachRequiredContent(ctx, head, required, func(string, []byte) error { return nil }); err != nil {
		return result, err
	}
	for i, r := range index.Records {
		if r.Emptied || r.Restored {
			continue
		}
		if matchesPrefix(r.OriginalPath, n.browseExcluded) || matchesPrefix(r.Path, n.browseExcluded) {
			return result, invalidInput("Trash contains excluded notes; review notebook exclusions before emptying")
		}
		if !n.editable(r.OriginalPath) {
			return result, ErrForbidden
		}
		changes[r.Path] = nil
		c := commentPath(r.Path)
		if _, e := n.resolve(c, false); e != nil {
			return result, e
		}
		if file, e := n.exists(ctx, head, c); e != nil {
			return result, e
		} else if file {
			changes[c] = nil
		} else if dir, e := n.isDir(ctx, head, c); e != nil {
			return result, e
		} else if dir {
			// Git could not remove a folder where the discussion belongs.
			return result, ErrForbidden
		}
		index.Records[i].Emptied = true
		result.Entries = append(result.Entries, r.TrashEntry)
	}
	if len(changes) == 0 {
		return result, nil
	}
	// Never checkpoint unrelated live text during a destructive confirmation.
	n.checkpointing = true
	err = n.clean(ctx)
	n.checkpointing = false
	if err != nil {
		return result, err
	}
	if err = stageTrashIndex(index, 0, changes); err != nil {
		return result, err
	}
	committed, err := n.applyChanges(ctx, head, "Empty Trash", orderedChanges(changes, nil))
	if err != nil {
		return result, err
	}
	result.Revision = committed.Head.Commit.String()
	return result, nil
}

// prepareTrash reads, at head, the notes a transaction moves into the Trash
// and their discussions within one budget, and each note's history, as its
// original does. Callers hold mu.
func (n *dbNotebook) prepareTrash(ctx context.Context, paths []string, index TrashIndex, head string) (trashPreparation, error) {
	selected := map[string]bool{}
	for _, path := range paths {
		selected[path], selected[commentPath(path)] = true, true
	}
	prepared := trashPreparation{contents: map[string][]byte{}, histories: map[string]map[string][]string{}}
	total := 0
	err := n.forEachCommittedContent(ctx, head, func(path string) bool { return selected[path] }, func(path string, content []byte) error {
		total += len(content)
		if total > MaxSnapshotBytes() {
			return invalidInput("Trash transaction exceeds FMN_LIMIT_INDEX_BYTES; move fewer notes at a time")
		}
		prepared.contents[path] = content
		return nil
	})
	if err != nil {
		return prepared, err
	}
	for _, path := range paths {
		if _, ok := prepared.contents[path]; !ok {
			return prepared, os.ErrNotExist
		}
		history, err := n.trashHistoryAt(ctx, path, index, head)
		if err != nil {
			return prepared, err
		}
		prepared.histories[path] = history
	}
	return prepared, nil
}

// stageTrash stages the move of the note at path into the Trash, with its
// discussion, and appends its record to index, as the Git backend did. With
// no preparation it reads the note and its history at the head itself.
// Callers hold mu, stage the index with stageTrashIndex, and give
// applyChanges the moves as renames with addTrashMove.
func (n *dbNotebook) stageTrash(ctx context.Context, index *TrashIndex, path, actor string, changes map[string][]byte, prepared *trashPreparation) (TrashEntry, error) {
	if IsTrash(path) || path == "AGENTS.md" || !n.visibleNote(path) {
		return TrashEntry{}, ErrForbidden
	}
	if err := n.resolveLeaving(path); err != nil {
		return TrashEntry{}, err
	}
	var content []byte
	var err error
	if prepared == nil {
		doc, readErr := n.read(ctx, path, "")
		if readErr != nil {
			return TrashEntry{}, readErr
		}
		content = doc.Content
	} else {
		var ok bool
		content, ok = prepared.contents[path]
		if !ok {
			return TrashEntry{}, os.ErrNotExist
		}
	}
	id := make([]byte, 16)
	rand.Read(id)
	title, _ := NoteSummary(content)
	meta, _ := ParseMetadata(content)
	if meta.Title != "" {
		title = meta.Title
	}
	if title == "" {
		title = filepath.Base(path)
	}
	entry := TrashEntry{ID: hex.EncodeToString(id), OriginalPath: path, Title: title, DeletedAt: time.Now().UTC().Format(time.RFC3339), DeletedBy: actor}
	entry.Path = "Trash/" + entry.ID + ".md"
	if _, err = n.resolve(entry.Path, false); err != nil {
		return entry, err
	}
	if taken, err := n.trashOccupied(ctx, entry.Path); err != nil {
		return entry, err
	} else if taken {
		return entry, ErrConflict
	}
	var history map[string][]string
	if prepared == nil {
		history, err = n.trashHistory(ctx, path)
		if err != nil {
			return entry, err
		}
	} else {
		history = prepared.histories[path]
	}
	total := len(content)
	for _, content := range changes {
		total += len(content)
	}
	if total > MaxSnapshotBytes() {
		return entry, invalidInput("Trash transaction exceeds FMN_LIMIT_INDEX_BYTES; move fewer notes at a time")
	}
	changes[path] = nil
	changes[entry.Path] = rewriteLinks(content, path, entry.Path, path, entry.Path)
	if prepared == nil {
		err = n.stageTrashDiscussion(ctx, path, entry.Path, changes)
	} else {
		err = n.stageTrashDiscussionContent(ctx, path, entry.Path, prepared.contents[commentPath(path)], changes)
	}
	if err != nil {
		return entry, err
	}
	index.Records = append(index.Records, TrashRecord{TrashEntry: entry, History: history})
	if prepared != nil {
		// The manifest now owns this note. Release its originals as staging
		// advances instead of retaining two whole transaction payloads.
		delete(prepared.contents, path)
		delete(prepared.contents, commentPath(path))
		delete(prepared.histories, path)
	}
	return entry, nil
}

// resolveLeaving checks path, a note a mutation moves into the Trash, as
// Move checks the path it moves from: the note leaves path, which is read,
// not written, so a note an earlier build stored under a name Git refuses
// (git~1/b.md) can still be deleted. The note must still be editable.
func (n *dbNotebook) resolveLeaving(path string) error {
	if _, err := n.resolve(path, false); err != nil {
		return err
	}
	if !n.editable(path) {
		return ErrForbidden
	}
	return nil
}

// stageTrashDiscussion stages the move of from's discussion at the head, if
// it has one, to to's, as the Git backend did. Callers hold mu.
func (n *dbNotebook) stageTrashDiscussion(ctx context.Context, from, to string, changes map[string][]byte) error {
	old := commentPath(from)
	if _, err := n.resolve(old, false); err != nil {
		return err
	}
	raw, _, err := n.notes.Read(ctx, gitobj.ID{}, old)
	if errors.Is(err, fs.ErrNotExist) {
		// Git shows a folder there as a tree listing, which is no discussion.
		dir, err := n.isDir(ctx, "", old)
		if err != nil {
			return err
		}
		if dir {
			return ErrForbidden
		}
		return nil
	}
	if err != nil {
		return err
	}
	return n.stageTrashDiscussionContent(ctx, from, to, raw, changes)
}

// stageTrashDiscussionContent checks the destination in the head, as the
// Git backend did in its working tree: it moves the discussion raw, nil for
// none, from from's discussion path to to's and points its findings at to.
func (n *dbNotebook) stageTrashDiscussionContent(ctx context.Context, from, to string, raw []byte, changes map[string][]byte) error {
	old, next := commentPath(from), commentPath(to)
	if _, err := n.resolve(old, false); err != nil {
		return err
	}
	if raw == nil {
		return nil
	}
	var err error
	if _, err = n.resolve(next, false); err != nil {
		return err
	}
	if taken, err := n.trashOccupied(ctx, next); err != nil {
		return err
	} else if taken {
		return invalidInput("destination already has a discussion")
	}
	var discussion []Comment
	if err = json.Unmarshal(raw, &discussion); err != nil {
		return err
	}
	for i := range discussion {
		if discussion[i].Finding != nil {
			discussion[i].Finding.Path = to
		}
	}
	raw, err = json.MarshalIndent(discussion, "", "  ")
	if err != nil {
		return err
	}
	changes[old] = nil
	changes[next] = raw
	return nil
}

// trashOccupied reports whether a file or folder is at path in the head, as
// os.Lstat in the working tree did.
func (n *dbNotebook) trashOccupied(ctx context.Context, path string) (bool, error) {
	if file, err := n.exists(ctx, "", path); err != nil || file {
		return file, err
	}
	return n.isDir(ctx, "", path)
}

// addTrashMove records in renames, each destination to its source, the move
// of a note from from to to that the staging helpers put in changes, and of
// its discussion when they moved that too. applyChanges then keeps both
// files' identities, so their histories follow them as Git's rename
// detection followed them.
func addTrashMove(renames map[string]string, changes map[string][]byte, from, to string) {
	for _, move := range [][2]string{{from, to}, {commentPath(from), commentPath(to)}} {
		content, written := changes[move[1]]
		old, removed := changes[move[0]]
		if written && content != nil && removed && old == nil {
			renames[move[1]] = move[0]
		}
	}
}

// trashHistory is the history of the note at path in the head, as its
// original computes it. Callers hold mu.
func (n *dbNotebook) trashHistory(ctx context.Context, path string) (map[string][]string, error) {
	index, err := n.loadTrash(ctx)
	if err != nil {
		return nil, err
	}
	head, err := n.head(ctx)
	if err != nil {
		return nil, err
	}
	return n.trashHistoryAt(ctx, path, index, head)
}

// trashHistoryPage is how many of an entry's revisions one query reads.
const trashHistoryPage = 1000

var errTrashHistory = errors.New("cannot establish this note's complete history")

// trashHistoryAt is trashHistoryFrom for the notes store: each name the note
// at path in head had, to the revisions that changed it under that name,
// newest first. Instead of walking Git's whole log it reads the revisions of
// the note's entry, which follows the note through its renames, and applies
// the Git walk's rules to them:
//
//   - a note added without a rename starts its history, even when a deleted
//     note's fmn-id gave it that entry again, unless the revision is a Move
//     whose subject names a removed source, or a restore from the Trash that
//     removed one recorded Trash note and added this one;
//   - a name the Trash index records continues with the history recorded
//     there, each revision a purge rewrote as the one standing for it now;
//   - a walk past 100,000 revisions from head, more than 100,000 revisions
//     or more than 1,000 names exceeds the Trash's capacity, a limit of the
//     note that no retry lifts (trashLimits).
//
// Every notebook FMN writes has a linear history. As the Git walk does when
// it loses a note's trail, this refuses a history it cannot follow: a merge,
// or a revision that changed the entry under another name.
func (n *dbNotebook) trashHistoryAt(ctx context.Context, path string, index TrashIndex, head string) (map[string][]string, error) {
	archived, err := n.trashArchived(ctx, head)
	if err != nil {
		return nil, err
	}
	rewrites, err := n.purgeRewrites(ctx)
	if err != nil {
		return nil, err
	}
	records := map[string]TrashRecord{}
	for _, list := range [][]TrashRecord{archived, index.Records} {
		for _, record := range list {
			records[record.Path] = record
		}
	}
	rev, err := parseRevision(head)
	if err != nil {
		return nil, err
	}
	headSeq, err := n.trashSeq(ctx, rev)
	if err != nil {
		return nil, err
	}
	file, err := n.notes.Stat(ctx, rev, path)
	if err != nil {
		return nil, err
	}
	history := map[string][]string{}
	seen := map[string]bool{}
	total := 0
	add := func(name, revision string) error {
		if _, err := n.resolve(name, false); err != nil {
			return err
		}
		key := name + "\x00" + revision
		if !seen[key] {
			seen[key] = true
			history[name] = append(history[name], revision)
			total++
			if total > trashLimits.revisions || len(history) > trashLimits.names {
				return limitExceeded("note history exceeds Trash capacity")
			}
		}
		return nil
	}
	// The walk has read every revision from head down to, not including,
	// last; oldest is the last revision the Git walk would have read.
	name, entry, last := path, file.EntryID, headSeq+1
	var oldest int64
walk:
	for {
		if record, ok := records[name]; ok {
			// The UUID has one owner; recorded occurrences retain generation
			// boundaries across restore/retrash cycles, including reused
			// original filenames.
			for old, revisions := range record.History {
				for _, rev := range currentRevisions(revisions, rewrites) {
					if err = add(old, rev); err != nil {
						return nil, err
					}
				}
			}
			oldest = last - 1
			break
		}
		page, err := n.notes.History(ctx, notestore.HistoryQuery{EntryID: entry, Before: last, Limit: trashHistoryPage})
		if err != nil {
			return nil, err
		}
		for _, c := range page {
			last = c.Seq
			change, ok := entryChange(c.Changes, entry)
			if !ok || len(c.Commit.Parents) > 1 {
				return nil, errTrashHistory
			}
			if change.Op == notestore.OpDelete {
				oldest = c.Seq
				break walk
			}
			if change.Path != name {
				return nil, errTrashHistory
			}
			if err = add(name, c.ID.String()); err != nil {
				return nil, err
			}
			previous, previousEntry := name, entry
			if change.Op == notestore.OpRename || change.Op == notestore.OpAdd {
				previous = change.OldPath
				subject := commitSubject(c.Commit.Message)
				var from, to string
				if k, _ := fmt.Sscanf(subject, "Move %q to %q", &from, &to); k == 2 {
					if old := movedPath(name, to, from); old != name {
						if e, removed := removedIn(c.Changes, old); removed {
							previous, previousEntry = old, e
						}
					}
				}
				if previous == "" && subject == "Restore note from Trash" {
					if trash, ok := restoredFrom(c.Changes, name); ok {
						if _, ok := records[trash]; ok {
							previous = trash
						}
					}
				}
			}
			if previous == "" {
				oldest = c.Seq
				break walk
			}
			if previous != name || previousEntry != entry {
				name, entry = previous, previousEntry
				continue walk
			}
		}
		if len(page) < trashHistoryPage {
			return nil, errTrashHistory
		}
	}
	if headSeq-oldest+1 > int64(trashLimits.revisions) {
		return nil, limitExceeded("note history exceeds Trash capacity")
	}
	return history, nil
}

// trashSeq is the sequence number of rev, the zero ID meaning the head.
func (n *dbNotebook) trashSeq(ctx context.Context, rev gitobj.ID) (int64, error) {
	h, err := n.notes.Head(ctx)
	if err != nil {
		return 0, err
	}
	if rev.IsZero() || rev == h.Commit {
		return h.Seq, nil
	}
	info, err := n.notes.Commit(ctx, rev)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, unknownRevision(rev)
	}
	return info.Seq, err
}

// entryChange is a revision's change to entry.
func entryChange(changes []notestore.Change, entry string) (notestore.Change, bool) {
	for _, c := range changes {
		if c.EntryID == entry {
			return c, true
		}
	}
	return notestore.Change{}, false
}

// removedIn reports whether a revision removed the file at path, deleting it
// or renaming it away without putting another file there, which Git's raw
// diff reports as a deletion; and the entry that was there.
func removedIn(changes []notestore.Change, path string) (string, bool) {
	entry, removed := "", false
	for _, c := range changes {
		switch {
		case c.Op == notestore.OpDelete && c.Path == path, c.Op == notestore.OpRename && c.OldPath == path:
			entry, removed = c.EntryID, true
		case c.Path == path:
			return "", false
		}
	}
	return entry, removed
}

// restoredFrom is the Trash path a restore revision took the note at name
// from: the one Trash file it removed, when the one note it added is name.
func restoredFrom(changes []notestore.Change, name string) (string, bool) {
	var deletedTrash, addedNotes []string
	for _, c := range changes {
		switch c.Op {
		case notestore.OpDelete:
			if IsTrash(c.Path) {
				deletedTrash = append(deletedTrash, c.Path)
			}
		case notestore.OpRename:
			if IsTrash(c.OldPath) {
				deletedTrash = append(deletedTrash, c.OldPath)
			}
			if strings.HasSuffix(c.Path, ".md") {
				addedNotes = append(addedNotes, c.Path)
			}
		case notestore.OpAdd:
			if strings.HasSuffix(c.Path, ".md") {
				addedNotes = append(addedNotes, c.Path)
			}
		}
	}
	if len(deletedTrash) == 1 && len(addedNotes) == 1 && addedNotes[0] == name {
		return deletedTrash[0], true
	}
	return "", false
}

// countTrashArchives is how many Trash archives there are at rev, numbered
// from 1. Callers hold mu.
func (n *dbNotebook) countTrashArchives(ctx context.Context, rev gitobj.ID) (int, error) {
	for k := 0; ; k++ {
		_, err := n.notes.Stat(ctx, rev, trashArchivePath(k+1))
		if errors.Is(err, fs.ErrNotExist) {
			return k, nil
		}
		if err != nil {
			return k, err
		}
	}
}

// trashArchived is the records of the Trash archives at revision, oldest
// first, which callers must not change; it rereads only the archives whose
// content changed. An archive holds only tombstones, in the index's format
// and within its bounds. Callers hold mu.
func (n *dbNotebook) trashArchived(ctx context.Context, revision string) ([]TrashRecord, error) {
	rev, err := parseRevision(revision)
	if err != nil {
		return nil, err
	}
	read := map[gitobj.ID][]TrashRecord{}
	var records []TrashRecord
	for k := 1; ; k++ {
		path := trashArchivePath(k)
		f, err := n.notes.Stat(ctx, rev, path)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return nil, err
		}
		archived, ok := n.trashArchives[f.Blob]
		if !ok || f.Mode != gitobj.ModeFile {
			raw, _, err := n.policyFile(ctx, revision, path, trashFileBytes, "Trash archive exceeds 8 MiB")
			if err != nil {
				return nil, err
			}
			archive, err := parseTrashArchive(raw)
			if err != nil {
				return nil, damaged(path, err)
			}
			for i := range archive.Records {
				archive.Records[i].Hidden = false
			}
			if err := n.validateTrash(archive); err != nil {
				return nil, damaged(path, err)
			}
			archived = archive.Records
		}
		read[f.Blob] = archived
		records = append(records, archived...)
	}
	n.trashArchives = read
	return records, nil
}

// checkTrashArchives refuses a notebook whose Trash archives readers would
// misread or skip, as a bundle import may bring them: an invalid archive,
// one holding a note still in the Trash, one past a gap in their numbering,
// archives without an index, or a record in two files. A handoff checks
// its snapshot alike (validateSnapshotPolicies); after these checks only
// compactions and purges write archives. index is what loadTrash just
// read. Callers hold mu.
func (n *dbNotebook) checkTrashArchives(ctx context.Context, index TrashIndex) error {
	files, err := n.files(ctx, n.trashRevision)
	if err != nil {
		return err
	}
	archives, indexed := 0, false
	for _, f := range files {
		switch {
		case f.Path == trashIndexPath:
			indexed = true
		case trashFile(f.Path):
			archives++
		}
	}
	if archives == 0 {
		return nil
	}
	if !indexed || archives != index.archives {
		return damaged("its archives", errors.New("invalid Trash archive"))
	}
	archived, err := n.trashArchived(ctx, n.trashRevision)
	if err != nil {
		return err
	}
	return damaged("its index and archives", distinctTrashRecords(archived, index.Records))
}
