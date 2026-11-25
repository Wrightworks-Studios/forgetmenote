package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// chatMoveScope is the mutation scope integrated chat attaches to a
// move_notes action over a corpus that leaves out archives and drafts.
func chatMoveScope(ctx context.Context, control string) context.Context {
	scope := ContentScope{Policy: ContainerPolicy{Version: 1, Archives: "exclude", Drafts: "exclude"}, Collection: "all"}
	return WithMutationScope(ctx, MutationScope{Version: 1, Content: scope, Control: control})
}

// scopedTrashLinkers saves F/a.md, index.md linking to it and four notes
// in L, K and P, three of which link to it, then moves those four to the
// Trash and returns their entries by original path.
func scopedTrashLinkers(t *testing.T, s *Store) map[string]TrashEntry {
	t.Helper()
	saveNote(t, s, "F/a.md", "# A\n")
	saveNote(t, s, "index.md", "# Index\n\n[A](F/a.md)\n")
	for _, p := range []string{"L/free.md", "K/origin-locked.md", "P/path-locked.md"} {
		saveNote(t, s, p, "# Linker\n\n[A](../F/a.md)\n")
	}
	saveNote(t, s, "L/unrelated.md", "# Unrelated\n\n[Index](../index.md)\n")
	trashed, err := s.TrashNotes(context.Background(), []string{"L/free.md", "K/origin-locked.md", "P/path-locked.md", "L/unrelated.md"}, headOf(t, s), "owner")
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]TrashEntry{}
	for _, entry := range trashed.Entries {
		entries[entry.OriginalPath] = entry
	}
	return entries
}

// A scoped chat action's move rewrites the links of notes in the Trash as
// an ordinary move does, the one change it makes outside its corpus, and
// skips, never refusing, a note an ordinary move would refuse to change: one
// trashed from a location now locked, or under a lock rule of its Trash
// path. Its result counts them and names no note in the Trash, and the
// note it updated reaches the new location once restored.
func TestScopedMovesUpdateTrashLinksBestEffort(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	entries := scopedTrashLinkers(t, s)
	free, originLocked, pathLocked := entries["L/free.md"], entries["K/origin-locked.md"], entries["P/path-locked.md"]
	setLocks(t, s, "K", pathLocked.Path)
	before := map[string]string{}
	for _, entry := range entries {
		doc, err := s.Read(ctx, entry.Path, "")
		if err != nil {
			t.Fatal(err)
		}
		before[entry.Path] = string(doc.Content)
	}

	moved, err := s.Move(chatMoveScope(ctx, "move_notes"), "F", "G", headOf(t, s), "agent")
	if err != nil {
		t.Fatal("a scoped move with notes in the Trash linking into it:", err)
	}
	if moved.UpdatedLinks != 2 || moved.TrashLinksUpdated != 1 || moved.TrashLinksSkipped != 2 {
		t.Errorf("the move's counts: %+v, want 2 notes updated, 1 in the Trash and 2 skipped there", moved)
	}
	raw, _ := json.Marshal(moved)
	if strings.Contains(string(raw), "Trash/") || !strings.Contains(string(raw), `"trashLinksUpdated":1,"trashLinksSkipped":2`) {
		t.Errorf("the move's result: %s", raw)
	}
	if doc, err := s.Read(ctx, "index.md", ""); err != nil || string(doc.Content) != "# Index\n\n[A](G/a.md)\n" {
		t.Errorf("index.md: %q %v", doc.Content, err)
	}
	if doc, err := s.Read(ctx, free.Path, ""); err != nil || string(doc.Content) != "# Linker\n\n[A](../G/a.md)\n" {
		t.Errorf("the note in the Trash the move may change: %q %v", doc.Content, err)
	}
	checkLinksResolve(t, s, free.Path)
	for _, entry := range []TrashEntry{originLocked, pathLocked, entries["L/unrelated.md"]} {
		if doc, err := s.Read(ctx, entry.Path, ""); err != nil || string(doc.Content) != before[entry.Path] {
			t.Errorf("%s, trashed from %s, changed: %q %v", entry.Path, entry.OriginalPath, doc.Content, err)
		}
	}

	if _, err := s.RestoreTrash(ctx, free.ID, "", headOf(t, s), "owner"); err != nil {
		t.Fatal(err)
	}
	if doc, err := s.Read(ctx, "L/free.md", ""); err != nil || string(doc.Content) != "# Linker\n\n[A](../G/a.md)\n" {
		t.Errorf("the restored note: %q %v", doc.Content, err)
	}
	checkLinksResolve(t, s, "L/free.md")
}

// An ordinary move, which rewrites every note in the Trash or refuses,
// counts those it rewrote; one that rewrites none reports neither count.
func TestMovesCountTrashLinksTheyUpdate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	trashedLinkers(t, s)
	moved, err := s.Move(ctx, "F", "G", headOf(t, s), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if moved.UpdatedLinks != 1 || moved.TrashLinksUpdated != 1 || moved.TrashLinksSkipped != 0 {
		t.Errorf("the move's counts: %+v", moved)
	}
	saveNote(t, s, "X.md", "# X\n")
	plain, err := s.Move(ctx, "X.md", "Y.md", headOf(t, s), "owner")
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(plain); strings.Contains(string(raw), "trashLinks") {
		t.Errorf("a move reaching no note in the Trash: %s", raw)
	}
}

// A scoped move the notebook refuses for another reason, here because it
// is read-only, names no note in the Trash: the notes there it may not
// change are skipped, not refused. Only notes in the Trash link to what it
// moves.
func TestScopedMoveRefusalsNameNoTrashNote(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	entries := scopedTrashLinkers(t, s)
	saveNote(t, s, "index.md", "# Index\n")
	head := headOf(t, s)
	s.SetReadOnly(true)
	_, err := s.Move(chatMoveScope(ctx, "move_notes"), "F", "G", head, "agent")
	if !errors.Is(err, ErrReadOnly) {
		t.Errorf("a scoped move in a read-only notebook: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(errorText(err), entry.Path) || strings.Contains(errorText(err), "Trash") {
			t.Errorf("the refusal names the Trash: %v", err)
		}
	}
	if now := headOf(t, s); now != head {
		t.Errorf("a refused move wrote %s", now)
	}
}

// The exception is a scoped move's own link rewrites: no other scoped
// action may change a note in the Trash, not even with the very content a
// move would write, and a move may write nothing else there.
func TestScopedActionsCannotOtherwiseEditTrashNotes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	entries := scopedTrashLinkers(t, s)
	free := entries["L/free.md"]
	rewrite := "# Linker\n\n[A](../G/a.md)\n"
	head := headOf(t, s)

	for _, control := range []string{"move_notes", "duplicate_notes", "set_note_container", ""} {
		scoped := chatMoveScope(ctx, control)
		group := EditGroup{ID: "edit-trash-" + control, Summary: "Edit a note in the Trash", Edits: []NoteEdit{{Path: free.Path, Content: rewrite, Revision: head}}}
		if err := s.ValidateMutationGroup(scoped, group); !errors.Is(err, ErrForbidden) {
			t.Errorf("validating a %q group that edits the Trash: %v", control, err)
		}
		if _, err := s.ApplyGroup(scoped, group); err == nil {
			t.Errorf("a %q group edited the Trash", control)
		}
		if _, err := s.Save(scoped, free.Path, []byte(rewrite), head, "agent"); err == nil {
			t.Errorf("a %q save edited the Trash", control)
		}
	}
	copied, err := s.Duplicate(chatMoveScope(ctx, "duplicate_notes"), "F", "H", head, "agent")
	if err != nil || copied.TrashLinksUpdated != 0 || copied.TrashLinksSkipped != 0 {
		t.Errorf("a scoped duplicate: %+v %v", copied, err)
	}
	if doc, err := s.Read(ctx, free.Path, ""); err != nil || string(doc.Content) != "# Linker\n\n[A](../F/a.md)\n" {
		t.Errorf("the note in the Trash after the other actions: %q %v", doc.Content, err)
	}

	// checkMutationChanges, given a move of F to G planning to rewrite
	// free's links, lets it write exactly that rewrite and nothing else.
	head = headOf(t, s)
	move := trashLinkMove{base: head, from: "F", to: "G", origins: map[string]string{free.Path: free.OriginalPath}}
	scoped := context.WithValue(chatMoveScope(ctx, "move_notes"), trashLinkMoveKey{}, move)
	other := entries["K/origin-locked.md"].Path
	for _, c := range []struct {
		name    string
		ctx     context.Context
		changes map[string][]byte
		allowed bool
	}{
		{"the planned rewrite", scoped, map[string][]byte{free.Path: []byte(rewrite)}, true},
		{"the planned rewrite without the move", chatMoveScope(ctx, "move_notes"), map[string][]byte{free.Path: []byte(rewrite)}, false},
		{"more than the rewrite", scoped, map[string][]byte{free.Path: []byte(rewrite + "Injected.\n")}, false},
		{"another rewrite", scoped, map[string][]byte{free.Path: []byte("# Linker\n\n[A](../H/a.md)\n")}, false},
		{"the unchanged note", scoped, map[string][]byte{free.Path: []byte("# Linker\n\n[A](../F/a.md)\n")}, false},
		{"removing the note", scoped, map[string][]byte{free.Path: nil}, false},
		{"a note the move did not plan", scoped, map[string][]byte{other: []byte(rewrite)}, false},
		{"a new note in the Trash", scoped, map[string][]byte{"Trash/new.md": []byte(rewrite)}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s.mu.Lock()
			err := backendOf(s).checkMutationChanges(c.ctx, c.changes)
			s.mu.Unlock()
			if c.allowed && err != nil || !c.allowed && !errors.Is(err, ErrForbidden) {
				t.Errorf("checking %s: %v", c.name, err)
			}
		})
	}
	// A lock rule over its original location, set since the move planned
	// it, withdraws the exception.
	setLocks(t, s, "L")
	move.base = headOf(t, s)
	s.mu.Lock()
	if err = backendOf(s).loadLocks(ctx); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	err = backendOf(s).checkMutationChanges(context.WithValue(chatMoveScope(ctx, "move_notes"), trashLinkMoveKey{}, move), map[string][]byte{free.Path: []byte(rewrite)})
	s.mu.Unlock()
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("rewriting a note trashed from a location locked since: %v", err)
	}
}

// A scoped chat move of media that a note in the Trash embeds moves it: a
// note in the Trash is no owner of media outside the scope, and its embeds
// are rewritten as its links are, where an ordinary move could change the
// note, and skipped otherwise. A note outside the scope that embeds the
// media still keeps a scoped move from moving it.
func TestScopedMovesOfMediaNotesInTheTrashEmbed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Save(ctx, "assets/pics/x.png", []byte("image"), "", "owner"); err != nil {
		t.Fatal(err)
	}
	saveNote(t, s, "index.md", "# Index\n\n![X](assets/pics/x.png)\n")
	for _, p := range []string{"L/free.md", "K/locked.md"} {
		saveNote(t, s, p, "# Embedder\n\n![X](../assets/pics/x.png)\n")
	}
	trashed, err := s.TrashNotes(ctx, []string{"L/free.md", "K/locked.md"}, headOf(t, s), "owner")
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]TrashEntry{}
	for _, entry := range trashed.Entries {
		entries[entry.OriginalPath] = entry
	}
	setLocks(t, s, "K")

	moved, err := s.Move(chatMoveScope(ctx, "move_notes"), "assets/pics", "assets/img", headOf(t, s), "agent")
	if err != nil {
		t.Fatal("a scoped move of media a note in the Trash embeds:", err)
	}
	if moved.UpdatedLinks != 2 || moved.TrashLinksUpdated != 1 || moved.TrashLinksSkipped != 1 {
		t.Errorf("the move's counts: %+v, want 2 notes updated, 1 in the Trash, and 1 skipped there", moved)
	}
	for path, want := range map[string]string{
		"index.md":                  "# Index\n\n![X](assets/img/x.png)\n",
		entries["L/free.md"].Path:   "# Embedder\n\n![X](../assets/img/x.png)\n",
		entries["K/locked.md"].Path: "# Embedder\n\n![X](../assets/pics/x.png)\n",
		"assets/img/x.png":          "image",
	} {
		if doc, err := s.Read(ctx, path, ""); err != nil || string(doc.Content) != want {
			t.Errorf("%s after the move: %q %v", path, doc.Content, err)
		}
	}

	saveNote(t, s, "D/draft.md", "---\nfmn-container: draft\n---\n# Draft\n\n![X](../assets/img/x.png)\n")
	head := headOf(t, s)
	if _, err := s.Move(chatMoveScope(ctx, "move_notes"), "assets/img", "assets/pictures", head, "agent"); !errors.Is(err, ErrForbidden) {
		t.Errorf("a scoped move of media a note outside the scope embeds: %v", err)
	}
	if now := headOf(t, s); now != head {
		t.Errorf("a refused move wrote %s", now)
	}
}

// A scoped chat move refused because a note it may not edit links into
// what it moves names no note outside its scope, and does not tell whether
// such a note is locked: one locked outside the scope is refused as an
// unlocked one is. A locked note inside the scope is named, as an ordinary
// move names every locked note it would edit.
func TestScopedMoveRefusalsNameNoNoteOutsideTheScope(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	saveNote(t, s, "F/a.md", "# A\n")
	saveNote(t, s, "D/locked-draft.md", "---\nfmn-container: draft\n---\n# Draft\n\n[A](../F/a.md)\n")
	setLocks(t, s, "D")
	head := headOf(t, s)
	_, scoped := s.Move(chatMoveScope(ctx, "move_notes"), "F", "G", head, "agent")
	if !errors.Is(scoped, ErrForbidden) || strings.Contains(errorText(scoped), "D/") || strings.Contains(errorText(scoped), "draft") {
		t.Errorf("a scoped move a locked note outside the scope links into: %v", scoped)
	}
	if _, err := s.Move(ctx, "F", "G", head, "owner"); !strings.Contains(errorText(err), "would edit locked note D/locked-draft.md") {
		t.Errorf("an ordinary move a locked note links into: %v", err)
	}

	setLocks(t, s)
	head = headOf(t, s)
	_, unlocked := s.Move(chatMoveScope(ctx, "move_notes"), "F", "G", head, "agent")
	if !errors.Is(unlocked, ErrForbidden) || errorText(unlocked) != errorText(scoped) {
		t.Errorf("a scoped move an unlocked note outside the scope links into: %v, and a locked one: %v", unlocked, scoped)
	}

	saveNote(t, s, "L/linker.md", "# Linker\n\n[A](../F/a.md)\n")
	saveNote(t, s, "D/locked-draft.md", "---\nfmn-container: draft\n---\n# Draft\n")
	setLocks(t, s, "L")
	head = headOf(t, s)
	if _, err := s.Move(chatMoveScope(ctx, "move_notes"), "F", "G", head, "agent"); !errors.Is(err, ErrForbidden) || !strings.Contains(errorText(err), "would edit locked note L/linker.md") {
		t.Errorf("a scoped move a locked note inside the scope links into: %v", err)
	}
	if now := headOf(t, s); now != head {
		t.Errorf("a refused move wrote %s", now)
	}
}
