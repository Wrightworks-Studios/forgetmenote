package workspace

import (
	"context"
	"reflect"
	"slices"
	"testing"
)

const (
	trashLinker       = "# Linker\n\nSee [A](../notes/a.md), [its part](<../notes/a.md#part>) and [B](../B.md).\n"
	trashLinkerMoved  = "# Linker\n\nSee [A](../dest/a.md), [its part](<../dest/a.md#part>) and [B](../B.md).\n"
	trashLocked       = "# Locked\n\n[A](../notes/a.md)\n"
	trashUnrelated    = "# Unrelated\n\n[B](../B.md)\n"
	trashLinkerOrigin = "sub/linker.md"
	trashLockedOrigin = "L/locked.md"
)

// organizedTrash saves notes/a.md, which an organization move relocates to
// dest/a.md, and moves three notes to the Trash: sub/linker.md and
// L/locked.md, which link to it, and other.md, which does not. It then
// locks L, the folder L/locked.md was trashed from. It returns the move,
// whose selected scope is the moved note alone, and the Trash entries by
// original path.
func organizedTrash(t *testing.T, s *Store) (OrganizationAction, map[string]TrashEntry) {
	t.Helper()
	saveNote(t, s, "notes/a.md", "# A\n\n## Part\n")
	saveNote(t, s, "dest/index.md", "# Destination\n")
	saveNote(t, s, "B.md", "# B\n")
	saveNote(t, s, trashLinkerOrigin, trashLinker)
	saveNote(t, s, trashLockedOrigin, trashLocked)
	saveNote(t, s, "other.md", trashUnrelated)
	trashed, err := s.TrashNotes(context.Background(), []string{trashLinkerOrigin, trashLockedOrigin, "other.md"}, headOf(t, s), "owner")
	if err != nil {
		t.Fatal(err)
	}
	lockPath(t, s, "L")
	entries := map[string]TrashEntry{}
	for _, entry := range trashed.Entries {
		entries[entry.OriginalPath] = entry
	}
	move := OrganizationAction{ID: "run:1", Kind: "move", Path: "notes/a.md", Target: "dest/a.md", Allowed: []string{"notes/a.md"}, Folders: []string{"dest"}, Sources: []MutationSource{orgSource(t, s, "notes/a.md")}}
	return move, entries
}

// wantMoveTrashLinks is what a move of organizedTrash reports of the Trash:
// the linker's links updated and the locked note's left as they were.
func wantMoveTrashLinks(entries map[string]TrashEntry) []OrganizationTrashLink {
	return sortedTrashLinks([]OrganizationTrashLink{
		{Path: entries[trashLinkerOrigin].Path, OriginalPath: trashLinkerOrigin, Updated: true},
		{Path: entries[trashLockedOrigin].Path, OriginalPath: trashLockedOrigin, Reason: "its original location L/locked.md is locked"},
	})
}

func sortedTrashLinks(links []OrganizationTrashLink) []OrganizationTrashLink {
	slices.SortFunc(links, func(a, b OrganizationTrashLink) int {
		if a.Path < b.Path {
			return -1
		}
		if a.Path > b.Path {
			return 1
		}
		return 0
	})
	return links
}

// checkNoTrashFiles fails if result lists a file in the Trash: the notes in
// the Trash an action reaches are its TrashLinks.
func checkNoTrashFiles(t *testing.T, name string, result OrganizationResult) {
	t.Helper()
	for _, f := range result.Files {
		if IsTrash(f.Path) {
			t.Errorf("%s lists %s among its files", name, f.Path)
		}
	}
}

func readNote(t *testing.T, s *Store, path string) string {
	t.Helper()
	doc, err := s.Read(context.Background(), path, "")
	if err != nil {
		t.Fatal(path, err)
	}
	return string(doc.Content)
}

// An organization move updates the links of notes in the Trash that link
// to the moved note, outside its selected scope, as an ordinary move does,
// so a note restored afterwards reaches the new location. A note in the
// Trash it may not change, here one trashed from a location now locked, is
// left as it was and named, and the move still succeeds. The preview, the
// result, a replay and the receipt name the same notes.
func TestOrganizationMoveUpdatesLinksOfNotesInTheTrash(t *testing.T) {
	s := newTestStore(t)
	ctx := organizationTestContext()
	move, entries := organizedTrash(t, s)
	want := wantMoveTrashLinks(entries)
	head := headOf(t, s)

	preview, err := s.Organize(ctx, move, false)
	if err != nil {
		t.Fatal("preview:", err)
	}
	if now := headOf(t, s); now != head {
		t.Fatal("the preview wrote", now)
	}
	if !reflect.DeepEqual(preview.TrashLinks, want) {
		t.Errorf("the preview names %+v, want %+v", preview.TrashLinks, want)
	}
	checkNoTrashFiles(t, "the preview", preview)

	applied, err := s.Organize(ctx, move, true)
	if err != nil {
		t.Fatal("the move:", err)
	}
	if !reflect.DeepEqual(applied.TrashLinks, want) {
		t.Errorf("the move names %+v, want %+v", applied.TrashLinks, want)
	}
	if !reflect.DeepEqual(applied.Files, preview.Files) {
		t.Errorf("the move changed %+v, its preview %+v", applied.Files, preview.Files)
	}
	checkNoTrashFiles(t, "the move", applied)
	if got := readNote(t, s, entries[trashLinkerOrigin].Path); got != trashLinkerMoved {
		t.Errorf("the linker in the Trash: %q, want %q", got, trashLinkerMoved)
	}
	if got := readNote(t, s, entries[trashLockedOrigin].Path); got != trashLocked {
		t.Errorf("the locked note in the Trash changed: %q", got)
	}
	if got := readNote(t, s, entries["other.md"].Path); got != trashUnrelated {
		t.Errorf("the unrelated note in the Trash changed: %q", got)
	}

	replay, err := s.Organize(ctx, move, true)
	if err != nil || !replay.Replayed || replay.Revision != applied.Revision || !reflect.DeepEqual(replay.TrashLinks, want) || !reflect.DeepEqual(replay.Files, applied.Files) {
		t.Errorf("the replay: %+v %v, want the move's result %+v", replay, err, applied)
	}
	receipt, err := s.OrganizationReceipt(ctx, move, "organizer-test")
	if err != nil || receipt.Revision != applied.Revision || !reflect.DeepEqual(receipt.TrashLinks, want) || !reflect.DeepEqual(receipt.Files, applied.Files) {
		t.Errorf("the receipt: %+v %v, want the move's result %+v", receipt, err, applied)
	}

	if _, err := s.RestoreTrash(context.Background(), entries[trashLinkerOrigin].ID, "", headOf(t, s), "owner"); err != nil {
		t.Fatal(err)
	}
	checkLinksResolve(t, s, trashLinkerOrigin)
}

// Undoing an organization move reverts the notes in the Trash it updated
// on a best-effort basis: an unchanged one is reverted, and one changed,
// emptied, restored or locked since is left as it is and named, without
// refusing the undo, which still brings the moved note back. A replay of
// the undo names the same notes.
func TestOrganizationMoveUndoRevertsNotesInTheTrashOnABestEffortBasis(t *testing.T) {
	ctx := organizationTestContext()
	for _, c := range []struct {
		name    string
		perturb func(t *testing.T, s *Store, linker TrashEntry)
		reason  string // the linker's, or "" when the undo reverts it
		linker  string // its content after the undo, or "" when it is gone
	}{
		{"unchanged", func(*testing.T, *Store, TrashEntry) {}, "", trashLinker},
		{"changed", func(t *testing.T, s *Store, _ TrashEntry) {
			if _, err := s.Move(context.Background(), "B.md", "C.md", headOf(t, s), "owner"); err != nil {
				t.Fatal(err)
			}
		}, "it changed since the action", "# Linker\n\nSee [A](../dest/a.md), [its part](<../dest/a.md#part>) and [B](../C.md).\n"},
		{"changed and changed back", func(t *testing.T, s *Store, _ TrashEntry) {
			for _, m := range [][2]string{{"B.md", "C.md"}, {"C.md", "B.md"}} {
				if _, err := s.Move(context.Background(), m[0], m[1], headOf(t, s), "owner"); err != nil {
					t.Fatal(err)
				}
			}
		}, "it changed since the action", trashLinkerMoved},
		{"emptied", func(t *testing.T, s *Store, _ TrashEntry) {
			setLocks(t, s) // Empty Trash refuses notes from locked locations.
			listing, err := s.ListTrash(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.EmptyTrash(context.Background(), listing.Revision, listing.Confirmation); err != nil {
				t.Fatal(err)
			}
		}, "it was emptied from the Trash since", ""},
		{"restored", func(t *testing.T, s *Store, linker TrashEntry) {
			if _, err := s.RestoreTrash(context.Background(), linker.ID, "", headOf(t, s), "owner"); err != nil {
				t.Fatal(err)
			}
		}, "it was restored from the Trash since", ""},
		{"locked since", func(t *testing.T, s *Store, _ TrashEntry) {
			lockPath(t, s, "sub")
		}, "its original location sub/linker.md is locked", trashLinkerMoved},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newTestStore(t)
			move, entries := organizedTrash(t, s)
			linker := entries[trashLinkerOrigin]
			applied, err := s.Organize(ctx, move, true)
			if err != nil {
				t.Fatal(err)
			}
			c.perturb(t, s, linker)

			undo := OrganizationUndo{ID: "run:undo-1", Action: move, Actor: "organizer-test", Revision: applied.Revision}
			restored, err := s.UndoOrganization(ctx, undo)
			if err != nil {
				t.Fatal("the undo:", err)
			}
			want := []OrganizationTrashLink{{Path: linker.Path, OriginalPath: trashLinkerOrigin, Updated: c.reason == "", Reason: c.reason}}
			if !reflect.DeepEqual(restored.TrashLinks, want) {
				t.Errorf("the undo names %+v, want %+v", restored.TrashLinks, want)
			}
			checkNoTrashFiles(t, "the undo", restored)
			if got := readNote(t, s, "notes/a.md"); got != "# A\n\n## Part\n" {
				t.Errorf("the moved note after the undo: %q", got)
			}
			if _, err := s.Read(ctx, "dest/a.md", ""); err == nil {
				t.Error("the moved note is still at its destination")
			}
			doc, err := s.Read(ctx, linker.Path, "")
			switch {
			case c.linker == "" && err == nil:
				t.Errorf("the linker is still in the Trash: %q", doc.Content)
			case c.linker != "" && (err != nil || string(doc.Content) != c.linker):
				t.Errorf("the linker after the undo: %q %v, want %q", doc.Content, err, c.linker)
			}

			replay, err := s.UndoOrganization(ctx, undo)
			if err != nil || !replay.Replayed || replay.Revision != restored.Revision || !reflect.DeepEqual(replay.TrashLinks, want) || !reflect.DeepEqual(replay.Files, restored.Files) {
				t.Errorf("the undo's replay: %+v %v, want %+v", replay, err, restored)
			}
		})
	}
}

// Two moves of one run that each update the links of a note in the Trash
// are undone in reverse order, the later one named, and the note in the
// Trash returns to what it was before either.
func TestOrganizationUndoRunRevertsANoteInTheTrashTwoMovesUpdated(t *testing.T) {
	s := newTestStore(t)
	ctx := organizationTestContext()
	first, entries := organizedTrash(t, s)
	linker := entries[trashLinkerOrigin]
	appliedFirst, err := s.Organize(ctx, first, true)
	if err != nil {
		t.Fatal(err)
	}
	second := OrganizationAction{ID: "run:2", Kind: "move", Path: "B.md", Target: "dest/B.md", Allowed: []string{"B.md"}, Folders: []string{"dest"}, Sources: []MutationSource{orgSource(t, s, "B.md")}}
	appliedSecond, err := s.Organize(ctx, second, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := readNote(t, s, linker.Path), "# Linker\n\nSee [A](../dest/a.md), [its part](<../dest/a.md#part>) and [B](../dest/B.md).\n"; got != want {
		t.Fatalf("the linker after both moves: %q, want %q", got, want)
	}
	undoSecond := OrganizationUndo{ID: "run:undo-2", Action: second, Actor: "organizer-test", Revision: appliedSecond.Revision}
	if _, err := s.UndoOrganization(ctx, undoSecond); err != nil {
		t.Fatal(err)
	}
	restored, err := s.UndoOrganization(ctx, OrganizationUndo{ID: "run:undo-1", Action: first, Actor: "organizer-test", Revision: appliedFirst.Revision, Later: []OrganizationUndo{undoSecond}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []OrganizationTrashLink{{Path: linker.Path, OriginalPath: trashLinkerOrigin, Updated: true}}; !reflect.DeepEqual(restored.TrashLinks, want) {
		t.Errorf("the first undo names %+v, want %+v", restored.TrashLinks, want)
	}
	if got := readNote(t, s, linker.Path); got != trashLinker {
		t.Errorf("the linker after both undos: %q, want %q", got, trashLinker)
	}
}

// A note in the Trash a move updated and a purge then erased leaves the
// move's receipt with every other version of it: the receipt and the undo,
// which still brings the moved note back, never name it again.
func TestOrganizationMoveUndoAfterAPurgeErasedANoteInTheTrash(t *testing.T) {
	s := newTestStore(t)
	ctx := organizationTestContext()
	move, entries := organizedTrash(t, s)
	applied, err := s.Organize(ctx, move, true)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.PreparePurge(t.Context(), PurgeScope{Kind: PurgeNote, Path: entries[trashLinkerOrigin].Path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExecutePurge(t.Context(), plan, plan.Confirmation, "owner", "erase the linker", nil); err != nil {
		t.Fatal(err)
	}
	revision, _ := resolved(t, s, "", applied.Revision)
	locked := []OrganizationTrashLink{{Path: entries[trashLockedOrigin].Path, OriginalPath: trashLockedOrigin, Reason: "its original location L/locked.md is locked"}}
	receipt, err := s.OrganizationReceipt(ctx, move, "organizer-test")
	if err != nil || receipt.Revision != revision || !reflect.DeepEqual(receipt.TrashLinks, locked) {
		t.Errorf("the receipt after the purge: %+v %v, want %s naming %+v", receipt, err, revision, locked)
	}
	restored, err := s.UndoOrganization(ctx, OrganizationUndo{ID: "run:undo-1", Action: move, Actor: "organizer-test", Revision: revision})
	if err != nil {
		t.Fatal("the undo:", err)
	}
	if len(restored.TrashLinks) != 0 {
		t.Errorf("the undo names %+v", restored.TrashLinks)
	}
	if got := readNote(t, s, "notes/a.md"); got != "# A\n\n## Part\n" {
		t.Errorf("the moved note after the undo: %q", got)
	}
}

// A note in the Trash whose links to the moved note a move cannot update
// safely, an HTML link or a CommonMark link the rewriter does not
// recognise, is left as it is and named; the move, which would refuse such
// a backlink in the notebook, goes ahead.
func TestOrganizationMoveLeavesNotesInTheTrashItCannotUpdateSafely(t *testing.T) {
	s := newTestStore(t)
	ctx := organizationTestContext()
	saveNote(t, s, "notes/a.md", "# A\n")
	saveNote(t, s, "dest/index.md", "# Destination\n")
	// The escaped link, which moving the note to the Trash does not adjust
	// either, reaches notes/a.md from sub and from the Trash alike.
	contents := map[string]string{"html.md": "# HTML\n\n<a href=\"notes/a.md\">A</a>\n", "sub/escaped.md": "# Escaped\n\n[A \\]](../notes/a.md)\n"}
	for p, content := range contents {
		saveNote(t, s, p, content)
	}
	trashed, err := s.TrashNotes(context.Background(), []string{"html.md", "sub/escaped.md"}, headOf(t, s), "owner")
	if err != nil {
		t.Fatal(err)
	}
	var want []OrganizationTrashLink
	for _, entry := range trashed.Entries {
		reason := trashLinkUnsupported
		if entry.OriginalPath == "sub/escaped.md" {
			reason = trashLinkUnsafe
		}
		want = append(want, OrganizationTrashLink{Path: entry.Path, OriginalPath: entry.OriginalPath, Reason: reason})
	}
	sortedTrashLinks(want)
	before := map[string]string{}
	for _, entry := range trashed.Entries {
		before[entry.Path] = readNote(t, s, entry.Path)
	}
	move := OrganizationAction{ID: "run:1", Kind: "move", Path: "notes/a.md", Target: "dest/a.md", Allowed: []string{"notes/a.md"}, Folders: []string{"dest"}, Sources: []MutationSource{orgSource(t, s, "notes/a.md")}}
	applied, err := s.Organize(ctx, move, true)
	if err != nil {
		t.Fatal("the move:", err)
	}
	if !reflect.DeepEqual(applied.TrashLinks, want) {
		t.Errorf("the move names %+v, want %+v", applied.TrashLinks, want)
	}
	for p, content := range before {
		if got := readNote(t, s, p); got != content {
			t.Errorf("%s in the Trash changed: %q, was %q", p, got, content)
		}
	}
}

// purgeTrashNote erases the note at p, in the Trash, from the notebook's
// history.
func purgeTrashNote(t *testing.T, s *Store, p string) {
	t.Helper()
	plan, err := s.PreparePurge(t.Context(), PurgeScope{Kind: PurgeNote, Path: p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExecutePurge(t.Context(), plan, plan.Confirmation, "owner", "erase "+p, nil); err != nil {
		t.Fatal(err)
	}
}

// A replay of the undo of an organization move, as the retry of an undo
// whose outcome was unknown makes, answers with the undo's revision after
// a purge gave the move and its undo new revisions: here a purge of an
// unrelated note in the Trash. It does so whether or not the move updated
// a note in the Trash.
func TestOrganizationMoveUndoReplaysAfterAPurge(t *testing.T) {
	ctx := organizationTestContext()
	for _, c := range []struct {
		name string
		move func(OrganizationAction, *Store) OrganizationAction
		want func(map[string]TrashEntry) []OrganizationTrashLink
	}{
		{"updating a note in the Trash", func(move OrganizationAction, _ *Store) OrganizationAction { return move },
			func(entries map[string]TrashEntry) []OrganizationTrashLink {
				return []OrganizationTrashLink{{Path: entries[trashLinkerOrigin].Path, OriginalPath: trashLinkerOrigin, Updated: true}}
			}},
		{"of a note nothing in the Trash links to", func(_ OrganizationAction, s *Store) OrganizationAction {
			saveNote(t, s, "lone.md", "# Lone\n")
			return OrganizationAction{ID: "run:1", Kind: "move", Path: "lone.md", Target: "dest/lone.md", Allowed: []string{"lone.md"}, Folders: []string{"dest"}, Sources: []MutationSource{orgSource(t, s, "lone.md")}}
		}, func(map[string]TrashEntry) []OrganizationTrashLink { return nil }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newTestStore(t)
			move, entries := organizedTrash(t, s)
			move = c.move(move, s)
			applied, err := s.Organize(ctx, move, true)
			if err != nil {
				t.Fatal(err)
			}
			undo := OrganizationUndo{ID: "run:undo-1", Action: move, Actor: "organizer-test", Revision: applied.Revision}
			restored, err := s.UndoOrganization(ctx, undo)
			if err != nil {
				t.Fatal("the undo:", err)
			}
			want := c.want(entries)
			if !reflect.DeepEqual(restored.TrashLinks, want) {
				t.Errorf("the undo names %+v, want %+v", restored.TrashLinks, want)
			}
			purgeTrashNote(t, s, entries["other.md"].Path)
			revision, _ := resolved(t, s, "", restored.Revision)
			if revision == restored.Revision {
				t.Fatal("the purge kept the undo's revision")
			}
			replay, err := s.UndoOrganization(ctx, undo)
			if err != nil || !replay.Replayed || replay.Revision != revision || !reflect.DeepEqual(replay.TrashLinks, want) || !reflect.DeepEqual(replay.Files, restored.Files) {
				t.Errorf("the undo's replay after the purge: %+v %v, want %s naming %+v", replay, err, revision, want)
			}
		})
	}
}

// An organization move names only the notes in the Trash that refer to the
// moved note: one that merely has a link the renderer cannot parse, an HTML
// image, a wiki link to another note or the moved note's file name ending
// another's is not named. One whose
// link to it is escaped is updated, or named when its link cannot be
// updated safely, and one with an unrelated link the renderer cannot parse
// beside its link to the moved note is updated.
func TestOrganizationMoveNamesOnlyNotesInTheTrashThatReferToIt(t *testing.T) {
	s := newTestStore(t)
	ctx := organizationTestContext()
	saveNote(t, s, "notes/a.md", "# A\n")
	saveNote(t, s, "dest/index.md", "# Destination\n")
	cases := []struct {
		origin, content string
		reason          string // "" when it is updated
		after           string // its content after the move when it is updated
	}{
		{"sub/bad-escape.md", "# Bad escape\n\n[x](%zz)\n", "-", ""},
		{"sub/picture.md", "# Picture\n\n<img src=\"pic.png\"> a picture\n", "-", ""},
		{"sub/html-other.md", "# HTML\n\n<a href=\"../notes/data.md\">Data</a>\n", "-", ""},
		{"sub/html.md", "# HTML\n\n<A HREF='../notes/a.md'>A</A>\n", trashLinkUnsupported, ""},
		{"sub/wiki-other.md", "# Wiki\n\n[[another]] and [[a note]]\n", "-", ""},
		{"sub/wiki.md", "# Wiki\n\nSee [[notes/a|A]].\n", trashLinkUnsupported, ""},
		{"sub/percent.md", "# Percent\n\n[A](../notes/%61.md)\n", "", "# Percent\n\n[A](../dest/a.md)\n"},
		{"sub/entity.md", "# Entity\n\n[A](../notes/a&#46;md)\n", trashLinkUnsafe, ""},
		{"sub/backslash.md", "# Backslash\n\n[A](../notes/a\\.md)\n", trashLinkUnsafe, ""},
		{"sub/beside.md", "# Beside\n\n[A](../notes/a.md) and [y](https://example.com/50%off)\n", "", "# Beside\n\n[A](../dest/a.md) and [y](https://example.com/50%off)\n"},
	}
	var paths []string
	for _, c := range cases {
		saveNote(t, s, c.origin, c.content)
		paths = append(paths, c.origin)
	}
	trashed, err := s.TrashNotes(context.Background(), paths, headOf(t, s), "owner")
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]TrashEntry{}
	for _, entry := range trashed.Entries {
		entries[entry.OriginalPath] = entry
	}
	var want []OrganizationTrashLink
	for _, c := range cases {
		if c.reason != "-" {
			want = append(want, OrganizationTrashLink{Path: entries[c.origin].Path, OriginalPath: c.origin, Updated: c.reason == "", Reason: c.reason})
		}
	}
	sortedTrashLinks(want)
	move := OrganizationAction{ID: "run:1", Kind: "move", Path: "notes/a.md", Target: "dest/a.md", Allowed: []string{"notes/a.md"}, Folders: []string{"dest"}, Sources: []MutationSource{orgSource(t, s, "notes/a.md")}}
	preview, err := s.Organize(ctx, move, false)
	if err != nil {
		t.Fatal("the preview:", err)
	}
	if !reflect.DeepEqual(preview.TrashLinks, want) {
		t.Errorf("the preview names %+v, want %+v", preview.TrashLinks, want)
	}
	applied, err := s.Organize(ctx, move, true)
	if err != nil {
		t.Fatal("the move:", err)
	}
	if !reflect.DeepEqual(applied.TrashLinks, want) {
		t.Errorf("the move names %+v, want %+v", applied.TrashLinks, want)
	}
	for _, c := range cases {
		wantContent := c.content
		if c.reason == "" {
			wantContent = c.after
		}
		if got := readNote(t, s, entries[c.origin].Path); got != wantContent {
			t.Errorf("%s in the Trash: %q, want %q", c.origin, got, wantContent)
		}
	}
	receipt, err := s.OrganizationReceipt(ctx, move, "organizer-test")
	if err != nil || !reflect.DeepEqual(receipt.TrashLinks, want) {
		t.Errorf("the receipt: %+v %v, want %+v", receipt.TrashLinks, err, want)
	}
}
