package workspace

import (
	"context"
	"errors"
	"path"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// trashedLinkers saves F/a.md, F/deep/b.md and N.md, and two notes that
// link to them, sub/folder.md into the folder F and note.md to the note N,
// then moves both linking notes to the Trash. It returns their entries by
// original path.
func trashedLinkers(t *testing.T, s *Store) map[string]TrashEntry {
	t.Helper()
	saveNote(t, s, "F/a.md", "# A\n")
	saveNote(t, s, "F/deep/b.md", "# B\n")
	saveNote(t, s, "N.md", "# N\n")
	saveNote(t, s, "sub/folder.md", "# Folder links\n\n[A](../F/a.md), [B](<../F/deep/b.md#part>) and [the folder](../F/).\n\n[ref]: ../F/a.md \"A\"\n")
	saveNote(t, s, "note.md", "# Note link\n\nSee [N](N.md).\n")
	trashed, err := s.TrashNotes(context.Background(), []string{"sub/folder.md", "note.md"}, headOf(t, s), "owner")
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]TrashEntry{}
	for _, entry := range trashed.Entries {
		entries[entry.OriginalPath] = entry
	}
	return entries
}

var markdownLink = regexp.MustCompile(`\]\(<?([^)>\s#]+)|(?m)^\[[^\]]+\]: (\S+)`)

// checkLinksResolve fails unless every relative link of the note at p
// names a note or a folder at the head.
func checkLinksResolve(t *testing.T, s *Store, p string) {
	t.Helper()
	ctx := context.Background()
	doc, err := s.Read(ctx, p, "")
	if err != nil {
		t.Fatal(err)
	}
	files, err := backendOf(s).files(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, match := range markdownLink.FindAllStringSubmatch(string(doc.Content), -1) {
		target := path.Join(path.Dir(p), match[1]+match[2])
		found := false
		for _, f := range files {
			found = found || f.Path == target || strings.HasPrefix(f.Path, target+"/")
		}
		if !found {
			t.Errorf("%s links to %s, which is not there: %q", p, target, doc.Content)
		}
	}
}

// Moving a folder or renaming a note rewrites the links of notes in the
// Trash, as those of every other note, so a restored note's links reach
// the new locations; the Trash index, the notes' histories and restores
// are unchanged by the rewrite.
func TestMovesRewriteLinksInTrashNotes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	entries := trashedLinkers(t, s)
	folder, note := entries["sub/folder.md"], entries["note.md"]
	before, err := s.TrashPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}

	moved, err := s.Move(ctx, "F", "G", headOf(t, s), "owner")
	if err != nil {
		t.Fatal("moving a folder a note in the Trash links into:", err)
	}
	if moved.UpdatedLinks != 1 {
		t.Errorf("updated links in %d notes, want 1", moved.UpdatedLinks)
	}
	renamed, err := s.Move(ctx, "N.md", "M.md", headOf(t, s), "owner")
	if err != nil {
		t.Fatal("renaming a note a note in the Trash links to:", err)
	}
	for p, want := range map[string]string{
		folder.Path: "# Folder links\n\n[A](../G/a.md), [B](<../G/deep/b.md#part>) and [the folder](../G/).\n\n[ref]: ../G/a.md \"A\"\n",
		note.Path:   "# Note link\n\nSee [N](../M.md).\n",
	} {
		doc, err := s.Read(ctx, p, "")
		if err != nil || string(doc.Content) != want {
			t.Errorf("%s in the Trash: %q %v, want %q", p, doc.Content, err, want)
		}
		checkLinksResolve(t, s, p)
	}
	after, err := s.TrashPolicy(ctx)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Errorf("the Trash index changed: %+v %v, was %+v", after, err, before)
	}
	listing, err := s.ListTrash(ctx)
	if err != nil || len(listing.Entries) != 2 {
		t.Fatalf("the Trash after the moves: %+v %v", listing, err)
	}

	for _, entry := range []TrashEntry{folder, note} {
		if _, err := s.RestoreTrash(ctx, entry.ID, "", headOf(t, s), "owner"); err != nil {
			t.Fatal("restoring", entry.OriginalPath, err)
		}
		checkLinksResolve(t, s, entry.OriginalPath)
	}
	for p, want := range map[string]string{
		"sub/folder.md": "# Folder links\n\n[A](../G/a.md), [B](<../G/deep/b.md#part>) and [the folder](../G/).\n\n[ref]: ../G/a.md \"A\"\n",
		"note.md":       "# Note link\n\nSee [N](M.md).\n",
	} {
		if doc, err := s.Read(ctx, p, ""); err != nil || string(doc.Content) != want {
			t.Errorf("restored %s: %q %v, want %q", p, doc.Content, err, want)
		}
	}

	// Each note's history runs from its creation through the Trash, the
	// rewrite made there and the restore, and every version reads.
	for p, rewrite := range map[string]string{"sub/folder.md": moved.Revision, "note.md": renamed.Revision} {
		history, err := s.History(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		var subjects []string
		for _, version := range history {
			subjects = append(subjects, commitSubject(version.Message))
			if _, err := s.Read(ctx, version.Path, version.Revision); err != nil {
				t.Errorf("%s's version %s at %s: %v", p, version.Path, version.Revision, err)
			}
			if version.Revision == rewrite && !IsTrash(version.Path) {
				t.Errorf("%s's link rewrite is at %s, not in the Trash", p, version.Path)
			}
		}
		if len(subjects) != 4 || subjects[0] != "Restore note from Trash" || !strings.HasPrefix(subjects[1], "Move ") || subjects[2] != "Move notes to Trash" || subjects[3] != "Update "+p {
			t.Errorf("%s's history: %q", p, subjects)
		}
	}
	// The records keep the histories they had; trashing a restored note
	// again continues them.
	policy, err := s.TrashPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range policy.Records {
		if !r.Restored || !reflect.DeepEqual(r.History, before.Records[i].History) {
			t.Errorf("record %s after the restore: %+v, was %+v", r.ID, r, before.Records[i])
		}
	}
	again, err := s.TrashNotes(ctx, []string{"note.md"}, headOf(t, s), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if history, err := trashedHistory(s, again.Entries[0].ID); err != nil || len(history["note.md"]) != 2 {
		t.Errorf("the history of a note trashed again: %v %v", history, err)
	}
}

// trashedHistory is the history the Trash index records for entry id.
func trashedHistory(s *Store, id string) (map[string][]string, error) {
	policy, err := s.TrashPolicy(context.Background())
	if err != nil {
		return nil, err
	}
	for _, r := range policy.Records {
		if r.ID == id {
			return r.History, nil
		}
	}
	return nil, errors.New("no record " + id)
}

// A move whose link rewrites reach notes in the Trash leaves Empty Trash
// working: the confirmation binds the new head, and the emptied records
// keep the histories they had.
func TestEmptyTrashAfterAMoveRewroteTrashLinks(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	entries := trashedLinkers(t, s)
	before, err := s.TrashPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(ctx, "F", "G", headOf(t, s), "owner"); err != nil {
		t.Fatal(err)
	}
	listing, err := s.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	emptied, err := s.EmptyTrash(ctx, listing.Revision, listing.Confirmation)
	if err != nil || len(emptied.Entries) != 2 {
		t.Fatalf("emptying the Trash: %+v %v", emptied, err)
	}
	for _, entry := range entries {
		if _, err := s.Read(ctx, entry.Path, ""); err == nil {
			t.Errorf("%s is still there", entry.Path)
		}
	}
	policy, err := s.TrashPolicy(ctx)
	if err != nil || len(policy.Records) != len(before.Records) {
		t.Fatalf("the Trash index after emptying: %+v %v", policy, err)
	}
	for i, r := range policy.Records {
		if !r.Emptied || !reflect.DeepEqual(r.History, before.Records[i].History) {
			t.Errorf("record %s after emptying: %+v, was %+v", r.ID, r, before.Records[i])
		}
	}
}

// A move still refuses to rewrite the links of a note the user locked,
// whether it is in the notebook or in the Trash with its original location
// locked, and of any note in a read-only notebook; a refused move changes
// nothing. A scoped chat action's move skips such a note in the Trash
// instead (TestScopedMovesUpdateTrashLinksBestEffort).
func TestMovesRefuseLinksInLockedNotesAndTrashNotesOutOfReach(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name    string
		prepare func(t *testing.T, s *Store, trashed TrashEntry) context.Context
		message string
	}{
		{"a locked note", func(t *testing.T, s *Store, _ TrashEntry) context.Context {
			saveNote(t, s, "locked.md", "[A](F/a.md)\n")
			lockPath(t, s, "locked.md")
			return ctx
		}, "updating links would edit locked note locked.md; unlock it first: this path is not accessible"},
		{"a note in the Trash from a locked folder", func(t *testing.T, s *Store, _ TrashEntry) context.Context {
			lockPath(t, s, "L")
			return ctx
		}, "updating links would edit locked note L/linker.md, now in the Trash as <trash>; unlock it first: this path is not accessible"},
		{"a note in the Trash under a lock rule of its Trash path", func(t *testing.T, s *Store, trashed TrashEntry) context.Context {
			setLocks(t, s, trashed.Path)
			return ctx
		}, "updating links would edit locked note <trash>; unlock it first: this path is not accessible"},
		{"a read-only notebook", func(t *testing.T, s *Store, _ TrashEntry) context.Context {
			s.SetReadOnly(true)
			return ctx
		}, "updating links would edit locked note <trash>; unlock it first: this path is not accessible"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newTestStore(t)
			saveNote(t, s, "F/a.md", "# A\n")
			saveNote(t, s, "L/linker.md", "# Linker\n\n[A](../F/a.md)\n")
			trashed, err := s.TrashNotes(ctx, []string{"L/linker.md"}, headOf(t, s), "owner")
			if err != nil {
				t.Fatal(err)
			}
			moveCtx := c.prepare(t, s, trashed.Entries[0])
			head := headOf(t, s)
			_, err = s.Move(moveCtx, "F", "G", head, "owner")
			want := strings.ReplaceAll(c.message, "<trash>", trashed.Entries[0].Path)
			if !errors.Is(err, ErrForbidden) || errorText(err) != want {
				t.Errorf("the move: %v, want %q", err, want)
			}
			if now := headOf(t, s); now != head {
				t.Errorf("a refused move wrote %s", now)
			}
		})
	}
}

// lockPath locks path with SetLock, as an owner does.
func lockPath(t *testing.T, s *Store, path string) {
	t.Helper()
	state, err := s.Locks(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetLock(context.Background(), path, true, state.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
}
