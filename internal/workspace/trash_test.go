package workspace

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestTrashDirectoryRejectsOrdinaryWrites(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Save(context.Background(), "Trash/injected.md", []byte("not system managed"), "", "writer"); err == nil {
		t.Fatal("ordinary save wrote the system Trash directory")
	}
}

func TestTrashRestoreAndConfirmedEmpty(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	doc, err := s.Save(ctx, "notes/one.md", []byte("# One\n[link](../other.md)\n"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	discussion, err := s.Comments(ctx, doc.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddComment(ctx, doc.Path, "Keep this discussion", discussion.Revision, "writer"); err != nil {
		t.Fatal(err)
	}
	status, _ := s.Status(ctx)
	trashed, err := s.TrashNotes(ctx, []string{doc.Path}, status.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if len(trashed.Entries) != 1 {
		t.Fatal(trashed)
	}
	entry := trashed.Entries[0]
	if _, err = s.Read(ctx, doc.Path, ""); err == nil {
		t.Fatal("trashed note remains at original path")
	}
	d, err := s.Comments(ctx, entry.Path)
	if err != nil || len(d.Comments) != 1 {
		t.Fatalf("discussion missing: %+v %v", d, err)
	}
	if _, err = s.Save(ctx, entry.Path, []byte("changed"), trashed.Revision, "writer"); err == nil {
		t.Fatal("trash was editable")
	}
	if _, err = s.Move(ctx, "Trash", "renamed", trashed.Revision, "owner"); err == nil {
		t.Fatal("system Trash was renamed")
	}
	if _, err = s.Save(ctx, doc.Path, []byte("new occupant"), "", "writer"); err != nil {
		t.Fatal(err)
	}
	status, _ = s.Status(ctx)
	if _, err = s.RestoreTrash(ctx, entry.ID, "", status.Revision, "owner"); err == nil {
		t.Fatal("restore overwrote destination")
	}
	restored, err := s.RestoreTrash(ctx, entry.ID, "restored.md", status.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	note, err := s.Read(ctx, "restored.md", "")
	if err != nil || !strings.Contains(string(note.Content), "[link](other.md)") {
		t.Fatalf("restore content: %s %v", note.Content, err)
	}
	if _, err = s.Read(ctx, doc.Path, doc.Revision); err != nil {
		t.Fatal("Git history missing", err)
	}
	again, err := s.TrashNotes(ctx, []string{"restored.md"}, restored.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EmptyTrash(ctx, preview.Revision, ""); err == nil {
		t.Fatal("empty accepted without confirmation")
	}
	if _, err = s.TrashNotes(ctx, []string{doc.Path}, again.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.EmptyTrash(ctx, preview.Revision, preview.Confirmation); err == nil {
		t.Fatal("stale confirmation emptied new items")
	}
	preview, err = s.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EmptyTrash(ctx, preview.Revision, preview.Confirmation); err != nil {
		t.Fatal(err)
	}
	listing, err := s.ListTrash(ctx)
	if err != nil || len(listing.Entries) != 0 {
		t.Fatal("Trash not empty", listing, err)
	}
	if _, err = s.Read(ctx, again.Entries[0].Path, ""); err == nil {
		t.Fatal("emptied note remains")
	}
	if _, err = s.Read(ctx, doc.Path, doc.Revision); err != nil {
		t.Fatal("empty erased Git history", err)
	}
}

// A Trash move checkpoints the note's accepted live edits first, so the
// Trash keeps them, and a restore after a restart brings them back.
func TestTrashPreservesLiveEditsAndSurvivesRestart(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	doc, err := s.Save(ctx, "live-trash.md", []byte("before"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	live, err := s.LiveRead(ctx, doc.Path)
	if err != nil {
		t.Fatal(err)
	}
	patch := TargetedPatch{Path: doc.Path, Base: live.LiveRevision, OperationID: "trash-live", Edits: []TextEdit{{Start: 0, End: 6, Old: "before", Text: "accepted live text"}}}
	if _, err = s.LivePatch(ctx, patch); err != nil {
		t.Fatal(err)
	}
	state, _ := s.Status(ctx)
	result, err := s.TrashNotes(ctx, []string{doc.Path}, state.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.Read(ctx, result.Entries[0].Path, "")
	if err != nil || string(saved.Content) != "accepted live text" {
		t.Fatal("accepted edits lost", err)
	}
	if _, err = s.LivePatch(ctx, patch); err == nil {
		t.Fatal("late update revived trashed note")
	}
	reopened := reopenTestStore(t, s)
	listing, err := reopened.ListTrash(ctx)
	if err != nil || len(listing.Entries) != 1 {
		t.Fatal("Trash lost on restart", err)
	}
	if _, err = reopened.RestoreTrash(ctx, listing.Entries[0].ID, "", listing.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	restored, err := reopened.LiveRead(ctx, doc.Path)
	if err != nil || restored.Content != "accepted live text" {
		t.Fatal("restored live content", restored, err)
	}
}
func TestTrashLocksAndAtomicFailure(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	doc, err := s.Save(ctx, "locked.md", []byte("original"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	locked, err := s.SetLock(ctx, doc.Path, true, doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.TrashNotes(ctx, []string{doc.Path}, locked.Revision, "owner"); err == nil {
		t.Fatal("trashed locked note")
	}
	state, err := s.SetLock(ctx, doc.Path, false, locked.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	restore := failCommits(t, s)
	if _, err = s.TrashNotes(ctx, []string{doc.Path}, state.Revision, "owner"); err == nil {
		t.Fatal("commit unexpectedly succeeded")
	}
	restore()
	current, err := s.Read(ctx, doc.Path, "")
	if err != nil || string(current.Content) != "original" {
		t.Fatal("failed move lost note", err)
	}
	listing, err := s.ListTrash(ctx)
	if err != nil || len(listing.Entries) != 0 || listing.Revision != state.Revision {
		t.Fatal("failed move changed Trash or HEAD", listing, err)
	}
	if moved, err := s.TrashNotes(ctx, []string{doc.Path}, state.Revision, "owner"); err != nil || len(moved.Entries) != 1 {
		t.Fatal("the retried move:", moved, err)
	}
}

func TestTrashReadsAndEmptyBatchCommittedNotes(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	paths := []string{}
	revision := ""
	for i := 0; i < 12; i++ {
		path := fmt.Sprintf("discard-%02d.md", i)
		doc, err := s.Save(ctx, path, []byte("# Discard\nretained needle\n"), "", "")
		if err != nil {
			t.Fatal(err)
		}
		revision = doc.Revision
		paths = append(paths, path)
	}
	if _, err := s.TrashNotes(ctx, paths, revision, ""); err != nil {
		t.Fatal(err)
	}
	preview, err := s.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := findTrashOf(s, "needle", "", "", "")
	if err != nil || len(entries) != 12 {
		t.Fatalf("entries=%d error=%v", len(entries), err)
	}
	result, err := s.EmptyTrash(ctx, preview.Revision, preview.Confirmation)
	if err != nil || len(result.Entries) != 12 {
		t.Fatalf("entries=%d error=%v", len(result.Entries), err)
	}
}

func TestTrashNotesBatchContentDiscussionsAndHistory(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	paths := []string{}
	for i := 0; i < 12; i++ {
		path := fmt.Sprintf("discard-%02d.md", i)
		doc, err := s.Save(ctx, path, []byte("# Retained history\n"), "", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.AddComment(ctx, path, "Retained discussion", doc.Revision, "owner"); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	status, err := s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.TrashNotes(ctx, paths, status.Revision, "owner")
	if err != nil || len(result.Entries) != 12 {
		t.Fatalf("entries=%d error=%v", len(result.Entries), err)
	}
	policy, err := s.TrashPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range result.Entries {
		discussion, err := s.Comments(ctx, entry.Path)
		if err != nil || len(discussion.Comments) != 1 || discussion.Comments[0].Text != "Retained discussion" {
			t.Fatal("discussion lost", err)
		}
		found := false
		for _, record := range policy.Records {
			if record.ID == entry.ID {
				found = len(record.History[entry.OriginalPath]) > 0
			}
		}
		if !found {
			t.Fatal("historical generations lost", entry.OriginalPath)
		}
	}
}
