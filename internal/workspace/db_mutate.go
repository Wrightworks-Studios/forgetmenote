package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// Moves, duplicates and the property writes for notebooks kept in the notes
// store. Each keeps the Git backend's checks, results, errors and commit
// subject; where that backend looked at the working tree it looks at the
// head. A move commits every file it moves as a rename, so a note and its
// discussion keep their entries, and their histories follow them.

// Move is a single notebook transaction, as the Git backend's was. Callers
// enforce Owner authority; locks travel with content.
func (n *dbNotebook) Move(ctx context.Context, from, to, expected, actor string) (MoveResult, error) {
	ctx = n.mutationContext(ctx)
	return n.transfer(ctx, from, to, expected, actor, false)
}

// Duplicate copies content and effective locks, without moving references
// or discussions; each copied note that has an fmn-id gets one of its own.
func (n *dbNotebook) Duplicate(ctx context.Context, from, to, expected, actor string) (MoveResult, error) {
	ctx = n.mutationContext(ctx)
	return n.transfer(ctx, from, to, expected, actor, true)
}

func (n *dbNotebook) transfer(ctx context.Context, from, to, expected, actor string, copyOnly bool) (MoveResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if IsTrash(from) || IsTrash(to) {
		return MoveResult{}, ErrForbidden
	}
	result := MoveResult{From: from, To: to}
	if err := n.loadLocks(ctx); err != nil {
		return result, err
	}
	if err := n.mutateResolve(ctx, from); err != nil {
		return result, err
	}
	if err := n.mutateResolve(ctx, to); err != nil {
		return result, err
	}
	if from == to || strings.HasPrefix(to, from+"/") {
		return result, invalidInput("choose a different destination outside this folder")
	}
	folder, err := n.isDir(ctx, "", from)
	if err != nil {
		return result, err
	}
	if !folder {
		file, err := n.exists(ctx, "", from)
		if err != nil {
			return result, err
		}
		if !file {
			return result, &fs.PathError{Op: "stat", Path: filepath.Join(n.Root, from), Err: syscall.ENOENT}
		}
		if !strings.HasSuffix(from, ".md") || !strings.HasSuffix(to, ".md") {
			return result, invalidInput("notes must keep the .md extension")
		}
	}
	if taken, err := n.trashOccupied(ctx, to); err != nil {
		return result, errors.Join(errors.New("destination already exists or is inaccessible"), err)
	} else if taken {
		return result, invalidInput("destination already exists or is inaccessible")
	}
	if err = n.clean(ctx); err != nil {
		return result, err
	}
	head, err := n.mutateBase(ctx)
	if err != nil {
		return result, err
	}
	if expected != head {
		return result, ErrConflict
	}
	files, err := n.files(ctx, head)
	if err != nil {
		return result, err
	}
	if !copyOnly {
		// What a move changes of notes in the Trash, their links and the
		// media they embed, is the one change a scoped action's move makes
		// outside its scope (checkMutationAsset, checkTrashLinkRewrite).
		ctx = context.WithValue(ctx, trashLinkMoveKey{}, trashLinkMove{base: head, from: from, to: to})
	}
	contents, err := n.readCommittedContents(ctx, head, func(path string) bool {
		return movedPath(path, from, to) != path
	})
	if err != nil {
		return result, err
	}
	changes := map[string][]byte{} // nil removes a file; non-nil writes it.
	renames := map[string]string{} // each moved file's destination to its source
	moved := map[string]string{}
	for _, f := range files {
		p := f.Path
		newPath := movedPath(p, from, to)
		if newPath == p {
			continue
		}
		if _, err = n.resolve(p, false); err != nil {
			return result, fmt.Errorf("cannot move %s: %w", p, err)
		}
		if _, err = n.resolve(newPath, false); err != nil {
			return result, err
		}
		// Refuse symlinks, submodules, and other non-note repository objects.
		content, ok := contents[p]
		if !ok {
			return result, fs.ErrNotExist
		}
		if err = n.checkMutationTarget(ctx, p, false); err != nil {
			return result, err
		}
		if !copyOnly {
			changes[p] = nil
			renames[newPath] = p
		}
		changes[newPath] = append([]byte{}, content...)
		moved[p] = newPath
	}
	if len(moved) == 0 {
		return result, invalidInput("no tracked files at this path")
	}
	var trashed []string // notes in the Trash whose links change
	var locked string    // a note the move may not edit whose links change
	err = n.forEachCommittedContent(ctx, head, func(p string) bool {
		if !strings.HasSuffix(p, ".md") || copyOnly && movedPath(p, from, to) == p {
			return false
		}
		_, err := n.resolve(p, false)
		return err == nil
	}, func(p string, content []byte) error {
		next := movedPath(p, from, to)
		updated := rewriteLinks(content, p, next, from, to)
		if string(updated) != string(content) {
			if !copyOnly && IsTrash(p) {
				trashed = append(trashed, p)
			} else if !copyOnly && !n.editable(p) {
				locked = p
				return ErrForbidden
			}
			changes[next] = updated
			result.UpdatedLinks++
		}
		return nil
	})
	if locked != "" {
		return result, n.lockedLinkRefusal(ctx, locked)
	}
	if err != nil {
		return result, err
	}
	rewritten, skipped, err := n.checkTrashLinkRewrites(ctx, trashed)
	if err != nil {
		return result, err
	}
	for _, p := range skipped {
		delete(changes, p)
	}
	result.UpdatedLinks -= len(skipped)
	result.TrashLinksUpdated, result.TrashLinksSkipped = len(rewritten), len(skipped)
	if len(rewritten) > 0 {
		ctx = context.WithValue(ctx, trashLinkMoveKey{}, trashLinkMove{base: head, from: from, to: to, origins: rewritten})
	}
	if copyOnly {
		for _, p := range moved {
			if strings.HasSuffix(p, ".md") {
				if id, _ := NoteID(changes[p]); id == "" {
					continue
				}
				id, e := NewNoteID()
				if e != nil {
					return result, e
				}
				changes[p], e = WithNoteID(changes[p], id)
				if e != nil {
					return result, e
				}
			}
		}
	}
	for p, newPath := range moved {
		if copyOnly || !strings.HasSuffix(p, ".md") {
			continue
		}
		// The head's listing answers these checks, which every moved note
		// makes, without a listing or a lookup of its own.
		oldComments, newComments := commentPath(p), commentPath(newPath)
		if err := n.mutateResolveListed(files, oldComments); err != nil {
			return result, err
		}
		content, found, err := n.mutateListedContent(ctx, files, oldComments)
		if err != nil {
			return result, err
		}
		if !found {
			continue
		}
		if _, taken := mutateListedFile(files, newComments); taken || mutateListedDir(files, newComments) {
			return result, invalidInput("destination already has a discussion")
		}
		var discussion []Comment
		if err = json.Unmarshal(content, &discussion); err != nil {
			return result, err
		}
		migrated := false
		for i := range discussion {
			if discussion[i].Finding != nil {
				discussion[i].Finding.Path = newPath
				migrated = true
			}
		}
		if migrated {
			if content, err = json.MarshalIndent(discussion, "", "  "); err != nil {
				return result, err
			}
		}
		changes[oldComments] = nil
		changes[newComments] = append([]byte{}, content...)
		renames[newComments] = oldComments
	}
	paths := []string{}
	seen := map[string]bool{}
	for _, p := range n.projectLocks {
		if copyOnly && !seen[p] {
			paths = append(paths, p)
			seen[p] = true
		}
		np := movedPath(p, from, to)
		if !seen[np] {
			paths = append(paths, np)
			seen[np] = true
		}
	}
	// A child leaving a locked ancestor must remain locked at its destination.
	if !n.editable(from) && !matchesPrefix(to, paths) {
		paths = append(paths, to)
	}
	sort.Strings(paths)
	if strings.Join(paths, "\x00") != strings.Join(n.projectLocks, "\x00") {
		if len(paths) > 256 {
			return result, capacityReached("project lock limit reached")
		}
		policy, _ := json.MarshalIndent(lockPolicy{1, paths}, "", "  ")
		if len(policy)+1 > 65536 {
			return result, capacityReached("lock policy exceeds 64 KiB")
		}
		changes[locksPath] = append(policy, '\n')
	}
	verb := "Move"
	if copyOnly {
		verb = "Duplicate"
	}
	committed, err := n.applyChanges(ctx, head, fmt.Sprintf("%s %q to %q", verb, from, to), orderedChanges(changes, renames))
	if err != nil {
		return result, err
	}
	result.Revision = committed.Head.Commit.String()
	if policy, ok := changes[locksPath]; ok {
		// As SetLock does, the cache names the policy this revision holds.
		n.projectLocks, n.locksRevision, n.locksBlob = paths, result.Revision, gitobj.BlobID(policy)
	}
	return result, nil
}

// lockedLinkRefusal refuses a move whose link updates would edit the locked
// note at p, naming it. A scoped action that may not edit p either is
// refused as it is for any note outside its scope whose links the move
// changes (checkMutationChanges): it learns neither p nor whether p is
// locked. Callers hold mu.
func (n *dbNotebook) lockedLinkRefusal(ctx context.Context, p string) error {
	if mutationScopeFrom(ctx) != nil {
		if err := n.checkMutationTarget(ctx, p, false); err != nil {
			return err
		}
	}
	return fmt.Errorf("updating links would edit locked note %s; unlock it first: %w", p, ErrForbidden)
}

// checkTrashLinkRewrites checks that a move may rewrite the links of the
// notes at paths, which are in the Trash, returning the original location
// of each it rewrites and the notes it skips. A note in the Trash keeps
// links that resolve from its Trash path, and a restore rewrites them for
// where it puts the note, so a move rewrites them as it does every other
// note's and a restored note still reaches what it linked to. The Trash is
// read-only to people, not to these rewrites; they are refused where an
// edit of the note would be: in a read-only notebook, under a lock rule of
// the note's Trash path or of the location it was trashed from, which a
// restore and Empty Trash refuse too. A scoped chat action's corpus leaves
// out the Trash, so its move rewrites these links as the one change it
// makes outside its corpus, on a best-effort basis: it skips a note an
// ordinary move refuses to change instead of refusing, and the action
// learns only how many notes it updated and skipped, never which. Callers
// hold mu.
func (n *dbNotebook) checkTrashLinkRewrites(ctx context.Context, paths []string) (map[string]string, []string, error) {
	if len(paths) == 0 {
		return nil, nil, nil
	}
	index, err := n.loadTrash(ctx)
	if err != nil {
		return nil, nil, err
	}
	original := trashOrigins(index)
	scoped := mutationScopeFrom(ctx) != nil
	rewritten := map[string]string{}
	var skipped []string
	for _, p := range paths {
		from := original[p]
		switch block := trashLinkCheck(p, from, n.projectLocks, n.readOnly.Load(), false); {
		case block == trashLinkFree:
			rewritten[p] = from
		case scoped:
			skipped = append(skipped, p)
		case block == trashLinkOriginLocked:
			return nil, nil, fmt.Errorf("updating links would edit locked note %s, now in the Trash as %s; unlock it first: %w", from, p, ErrForbidden)
		default:
			return nil, nil, fmt.Errorf("updating links would edit locked note %s; unlock it first: %w", p, ErrForbidden)
		}
	}
	return rewritten, skipped, nil
}

// checkTrashLinkRewrite checks a scoped action's change of the note at path,
// in the Trash, to after. Apart from the Trash controls, only a move may
// make one: the move in ctx, rewriting the links of a note it planned to,
// to exactly what rewriteLinks makes of the note at the move's base, and
// only while a move may change the note (checkTrashLinkRewrites). Anything
// else, removing or adding a note included, is forbidden.
func (n *dbNotebook) checkTrashLinkRewrite(ctx context.Context, path string, after []byte) error {
	move, ok := ctx.Value(trashLinkMoveKey{}).(trashLinkMove)
	origin, planned := move.origins[path]
	if !ok || !planned || after == nil || trashLinkCheck(path, origin, n.projectLocks, n.readOnly.Load(), false) != trashLinkFree {
		return ErrForbidden
	}
	base, err := parseRevision(move.base)
	if err != nil || base.IsZero() {
		return ErrForbidden
	}
	before, _, err := n.notes.Read(ctx, base, path)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	if bytes.Equal(before, after) || !bytes.Equal(rewriteLinks(before, path, path, move.from, move.to), after) {
		return ErrForbidden
	}
	return nil
}

// trashLinkMoveKey carries a move's link rewrites in the Trash to
// checkMutationChanges: for a scoped action they are the one change in the
// Trash it allows other than the Trash controls'.
type trashLinkMoveKey struct{}

// trashLinkMove is a move of from to to committed on base, and the original
// location of each note in the Trash whose links it rewrites.
type trashLinkMove struct {
	base, from, to string
	origins        map[string]string
}

// moves reports whether path is what the move takes or where it puts it.
func (m trashLinkMove) moves(path string) bool {
	return movedPath(path, m.from, m.to) != path || path == m.to || strings.HasPrefix(path, m.to+"/")
}

// trashLinkBlock is why a move may not rewrite the links of a note in the
// Trash: an ordinary move refuses then, and a scoped chat action's move
// and an organization move leave the note as it is.
type trashLinkBlock int

const (
	trashLinkFree trashLinkBlock = iota
	trashLinkScoped
	trashLinkReadOnly
	trashLinkLocked
	trashLinkOriginLocked
)

// trashLinkCheck is why a move may not rewrite the links of the note at p in
// the Trash, trashed from from, under the lock rules locks: a scoped
// action, a read-only notebook, a rule over p or one over from. Only an
// organization move passes scoped: a scoped move's rewrites are the
// exception checkMutationChanges makes (checkTrashLinkRewrite).
func trashLinkCheck(p, from string, locks []string, readOnly, scoped bool) trashLinkBlock {
	switch {
	case scoped:
		return trashLinkScoped
	case readOnly:
		return trashLinkReadOnly
	case matchesPrefix(p, locks):
		return trashLinkLocked
	case from != "" && matchesPrefix(from, locks):
		return trashLinkOriginLocked
	}
	return trashLinkFree
}

// mutateBase is the head a mutation commits on, read under mu once live
// edits are checkpointed: the head its lock rules were read at. A check may
// still read a newer head another gateway wrote meanwhile; the commit then
// fails with ErrConflict, so no lock or revision check is ever bypassed.
// Mutations read their base here, never with head after loadLocks: a lock
// committed between the two would hold at the base but not in the rules
// checked, and applyChanges would refuse the commit as a conflict.
func (n *dbNotebook) mutateBase(ctx context.Context) (string, error) {
	if err := n.loadLocks(ctx); err != nil {
		return "", err
	}
	return n.locksRevision, nil
}

// mutateResolve is resolve with the checks the Git backend's resolve makes
// in the working tree made in the head instead: a symbolic link at path or
// on the way to it is forbidden, and a file on the way is not a folder.
// Like that resolve, it checks every component: a folder or a missing one
// on the way does not end the walk.
func (n *dbNotebook) mutateResolve(ctx context.Context, path string) error {
	return n.mutateResolveWith(path, func(p string) (notestore.File, bool, error) {
		f, err := n.notes.Stat(ctx, gitobj.ID{}, p)
		if errors.Is(err, fs.ErrNotExist) {
			return f, false, nil
		}
		return f, err == nil, err
	})
}

// mutateResolveListed is mutateResolve against files, a listing of the head
// sorted by path, for a mutation that checks many paths.
func (n *dbNotebook) mutateResolveListed(files []notestore.File, path string) error {
	return n.mutateResolveWith(path, func(p string) (notestore.File, bool, error) {
		f, found := mutateListedFile(files, p)
		return f, found, nil
	})
}

// mutateResolveWith walks path as mutateResolve does, stat giving the file
// at each component and whether there is one.
func (n *dbNotebook) mutateResolveWith(path string, stat func(string) (notestore.File, bool, error)) error {
	if _, err := n.resolve(path, false); err != nil {
		return err
	}
	parts := strings.Split(path, "/")
	for i := range parts {
		f, found, err := stat(strings.Join(parts[:i+1], "/"))
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if f.Mode == gitobj.ModeSymlink {
			return ErrForbidden
		}
		if i < len(parts)-1 {
			return &fs.PathError{Op: "lstat", Path: filepath.Join(n.Root, strings.Join(parts[:i+2], "/")), Err: syscall.ENOTDIR}
		}
	}
	return nil
}

// mutateListedFile is the file at path in files, a listing sorted by path,
// and whether there is one.
func mutateListedFile(files []notestore.File, path string) (notestore.File, bool) {
	i, found := slices.BinarySearchFunc(files, path, func(f notestore.File, p string) int { return strings.Compare(f.Path, p) })
	if !found {
		return notestore.File{}, false
	}
	return files[i], true
}

// mutateListedDir reports whether path is a folder in files, a listing
// sorted by path: whether any file lies under it.
func mutateListedDir(files []notestore.File, path string) bool {
	prefix := path + "/"
	i, _ := slices.BinarySearchFunc(files, prefix, func(f notestore.File, p string) int { return strings.Compare(f.Path, p) })
	return i < len(files) && strings.HasPrefix(files[i].Path, prefix)
}

// mutateListedContent reads the regular file at path in files, a listing
// sorted by path, reporting whether there is one. A folder or any other
// kind of file there is forbidden, as reading it from the working tree was.
func (n *dbNotebook) mutateListedContent(ctx context.Context, files []notestore.File, path string) ([]byte, bool, error) {
	f, found := mutateListedFile(files, path)
	if !found {
		if mutateListedDir(files, path) {
			return nil, false, ErrForbidden
		}
		return nil, false, nil
	}
	if f.Mode != gitobj.ModeFile {
		return nil, false, ErrForbidden
	}
	if f.Size > int64(MaxFileBytes()) {
		return nil, false, errors.New("file exceeds FMN_LIMIT_FILE_BYTES")
	}
	var raw []byte
	err := n.readBlobs(ctx, []notestore.File{f}, func(_ int, content []byte) { raw = content })
	return raw, true, err
}

func (n *dbNotebook) SetContainer(ctx context.Context, path, container, expected, actor string) (Document, error) {
	ctx = n.mutationContext(ctx)
	if container != "" && container != "draft" && container != "archive" {
		return Document{}, invalidInput("choose Notes, Drafts, or Archives")
	}
	if !strings.HasSuffix(path, ".md") || path == "AGENTS.md" {
		return Document{}, ErrForbidden
	}
	doc, err := n.Read(ctx, path, "")
	if err != nil {
		return Document{}, err
	}
	before, err := ParseContainer(doc.Content)
	if err != nil {
		return Document{}, err
	}
	if before == container {
		return n.Save(ctx, path, doc.Content, expected, actor)
	}
	content, err := WithContainer(doc.Content, container)
	if err != nil {
		return Document{}, err
	}
	return n.Save(ctx, path, content, expected, actor)
}

func (n *dbNotebook) SetMetadata(ctx context.Context, path string, m Metadata, expected, actor string) (Document, error) {
	ctx = n.mutationContext(ctx)
	if !strings.HasSuffix(path, ".md") {
		return Document{}, ErrForbidden
	}
	for _, v := range []string{m.Title, m.Kind, m.Status} {
		if !utf8.ValidString(v) || len(v) > 500 || strings.ContainsAny(v, "\r\n\x00") {
			return Document{}, invalidInput("properties must be single-line text of at most 500 bytes")
		}
	}
	if len(m.Tags) > 50 {
		return Document{}, invalidInput("use at most 50 tags per note")
	}
	tags := []string{}
	seen := map[string]bool{}
	for _, tag := range m.Tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if len(tag) > 80 || !utf8.ValidString(tag) || strings.ContainsAny(tag, "\r\n\x00") {
			return Document{}, invalidInput("tags must be single-line text of at most 80 bytes")
		}
		key := strings.ToLower(tag)
		if !seen[key] {
			seen[key] = true
			tags = append(tags, tag)
		}
	}
	m.Tags = tags
	if m.Cover != nil && *m.Cover != "" {
		if !ValidCoverPath(*m.Cover) {
			return Document{}, invalidInput("cover must be an image path under assets/")
		}
		if _, err := n.Read(ctx, *m.Cover, ""); err != nil {
			return Document{}, invalidInput("cover image is unavailable; upload it first")
		}
	}
	doc, err := n.Read(ctx, path, "")
	if err != nil {
		return Document{}, err
	}
	old, ranges, lines, body, newline, err := metadataFields(doc.Content)
	if err != nil {
		return Document{}, err
	}
	if m.Cover == nil {
		m.Cover = old.Cover
	}
	values := map[string]any{"cover": m.Cover, "title": m.Title, "kind": m.Kind, "status": m.Status, "tags": m.Tags}
	oldValues := map[string]any{"cover": old.Cover, "title": old.Title, "kind": old.Kind, "status": old.Status, "tags": old.Tags}
	replacements := map[int]string{}
	skip := map[int]bool{}
	add := ""
	for _, key := range []string{"title", "kind", "status", "tags", "cover"} {
		raw, _ := json.Marshal(values[key])
		before, _ := json.Marshal(oldValues[key])
		if bytes.Equal(raw, before) {
			continue
		}
		value := key + ": " + string(raw) + newline
		if r, ok := ranges[key]; ok {
			replacements[r.start] = value
			for j := r.start + 1; j < r.end; j++ {
				skip[j] = true
			}
		} else {
			add += value
		}
	}
	if len(replacements) == 0 && add == "" {
		return n.Save(ctx, path, doc.Content, expected, actor)
	}
	var output strings.Builder
	output.WriteString("---" + newline)
	for i, line := range lines {
		if skip[i] {
			continue
		}
		if value, ok := replacements[i]; ok {
			output.WriteString(value)
		} else {
			output.WriteString(line)
		}
	}
	output.WriteString(add)
	output.WriteString("---" + newline)
	output.Write(body)
	return n.Save(ctx, path, []byte(output.String()), expected, actor)
}
