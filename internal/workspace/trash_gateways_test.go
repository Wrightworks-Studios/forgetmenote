package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// pendingLiveEdit gives s accepted live edits to path that are not
// checkpointed yet, as while someone types.
func pendingLiveEdit(t *testing.T, s *Store, path string) {
	t.Helper()
	ctx := context.Background()
	base, err := s.LiveRead(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LivePatch(ctx, TargetedPatch{Path: path, Base: base.LiveRevision, OperationID: "typing", Edits: []TextEdit{{Start: 0, End: 0, Text: "typed "}}}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	pending := s.backend.hasLivePending()
	s.mu.Unlock()
	if !pending {
		t.Fatal("no live edit is pending")
	}
}

// otherGateway is s's notebook through a notes store of its own, as
// another gateway writes it. It has no workspace store, which would load
// s's pending live edits when it hears of them.
func otherGateway(t *testing.T, s *Store) *notestore.Notebook {
	t.Helper()
	return openTestNotes(t, testEnv(t, s)).Notebook(s.notebook)
}

// commitTrashChange commits, through other, another gateway, a Trash
// change as that gateway's TrashNotes or RestoreTrash writes it: edit
// applied to the Trash index at the head, with files.
func commitTrashChange(t *testing.T, other *notestore.Notebook, subject string, edit func(*TrashIndex), files ...notestore.FileChange) string {
	t.Helper()
	ctx := context.Background()
	raw, _, err := other.Read(ctx, gitobj.ID{}, trashIndexPath)
	if err != nil {
		t.Fatal(err)
	}
	var index TrashIndex
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	edit(&index)
	changes := map[string][]byte{}
	if err := stageTrashIndex(index, 0, changes); err != nil {
		t.Fatal(err)
	}
	r, err := other.Apply(ctx, notestore.ChangeSet{Subject: subject, Author: fixtureAuthor, Files: append(files, fixture(trashIndexPath, string(changes[trashIndexPath])))})
	if err != nil {
		t.Fatal(err)
	}
	return r.Head.Commit.String()
}

// restoreDuringWrite restores entry on s, which holds pending live edits,
// while another gateway's write lands in the window between the restore's
// start and the checkpoint of those edits: the checkpoint waits on the live
// edits' storage until write has committed and s has heard of it.
func restoreDuringWrite(t *testing.T, s *Store, entry string, write func() string) (TrashResult, error) {
	t.Helper()
	ctx := context.Background()
	db := testEnv(t, s).db
	head := headOf(t, s)
	waiting, release := holdReads(t, db, "live_updates")
	type outcome struct {
		result TrashResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := s.RestoreTrash(ctx, entry, "", head, "owner")
		done <- outcome{result, err}
	}()
	waiting(1)
	waitHeard(t, s, write())
	release()
	o := <-done
	return o.result, o.err
}

// A restore on one gateway whose live edits are checkpointed after another
// gateway moved a note into the Trash keeps that gateway's record: the
// note stays listed, and emptying the Trash removes it.
func TestRestoreKeepsATrashMoveAnotherGatewayMadeMeanwhile(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	saveNote(t, s, "live.md", "# Live\n")
	y := saveNote(t, s, "y.md", "# Y\n")
	doc := saveNote(t, s, "restore-me.md", "# R\n")
	trashed, err := s.TrashNotes(ctx, []string{"restore-me.md"}, doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	other := otherGateway(t, s)
	pendingLiveEdit(t, s, "live.md")
	moved := TrashEntry{ID: "0123456789abcdef0123456789abcdef", OriginalPath: "y.md", Title: "Y", DeletedAt: "2026-11-23T00:00:00Z", DeletedBy: "owner"}
	moved.Path = "Trash/" + moved.ID + ".md"
	if _, err = restoreDuringWrite(t, s, trashed.Entries[0].ID, func() string {
		return commitTrashChange(t, other, "Move notes to Trash", func(index *TrashIndex) {
			index.Records = append(index.Records, TrashRecord{TrashEntry: moved, History: map[string][]string{"y.md": {y.Revision}}})
		}, notestore.FileChange{Path: moved.Path, From: "y.md"})
	}); err != nil {
		t.Fatal("the restore:", err)
	}
	listing, err := s.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(listing.Entries, func(e TrashEntry) bool { return e.ID == moved.ID }) {
		t.Fatalf("the Trash lost the other gateway's record of y.md: %+v", listing.Entries)
	}
	if _, err := s.EmptyTrash(ctx, listing.Revision, listing.Confirmation); err != nil {
		t.Fatal("emptying the Trash:", err)
	}
	if _, err := s.notes.Stat(ctx, gitobj.ID{}, moved.Path); err == nil {
		t.Fatal("emptying the Trash left", moved.Path)
	}
}

// A restore on one gateway whose live edits are checkpointed after another
// gateway restored a different note keeps that restore: the note is not
// listed in the Trash again, and emptying the Trash still works.
func TestRestoreKeepsARestoreAnotherGatewayMadeMeanwhile(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	saveNote(t, s, "live.md", "# Live\n")
	saveNote(t, s, "w.md", "# W\n")
	doc := saveNote(t, s, "restore-me.md", "# R\n")
	trashed, err := s.TrashNotes(ctx, []string{"restore-me.md", "w.md"}, doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	mine, theirs := trashed.Entries[0], trashed.Entries[1]
	other := otherGateway(t, s)
	pendingLiveEdit(t, s, "live.md")
	if _, err = restoreDuringWrite(t, s, mine.ID, func() string {
		return commitTrashChange(t, other, "Restore note from Trash", func(index *TrashIndex) {
			for i := range index.Records {
				if index.Records[i].ID == theirs.ID {
					index.Records[i].Restored = true
				}
			}
		}, notestore.FileChange{Path: "w.md", From: theirs.Path})
	}); err != nil {
		t.Fatal("the restore:", err)
	}
	listing, err := s.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(listing.Entries, func(e TrashEntry) bool { return e.ID == theirs.ID }) {
		t.Fatalf("w.md, restored by the other gateway, is listed in the Trash again: %+v", listing.Entries)
	}
	if _, err := s.EmptyTrash(ctx, listing.Revision, listing.Confirmation); err != nil {
		t.Fatal("emptying the Trash:", err)
	}
	if _, err := s.RestoreTrash(ctx, theirs.ID, "", headOf(t, s), "owner"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("restoring w.md again:", err)
	}
}
