package workspace

import (
	"context"
	"errors"
	"os"
	"testing"

	"forgetmenote/internal/pg/pgtest"
)

// An organization whose commit fails, or whose request is cancelled while
// the commit waits, changes nothing and reports the store's failure rather
// than an unknown outcome; the action then applies and undoes normally.
func TestDatabaseOrganizationFailedOrCancelledCommit(t *testing.T) {
	s := newTestStore(t)
	orgSave(t, s, "a.md", "# Original\n")
	action := func() OrganizationAction {
		return OrganizationAction{ID: "failure:1", Kind: "tags", Path: "a.md", Tags: []string{"workshop"}, Allowed: []string{"a.md"}, Sources: []MutationSource{orgSource(t, s, "a.md")}}
	}
	before := headOf(t, s)
	release := failCommits(t, s)
	if _, err := s.Organize(organizationTestContext(), action(), true); err == nil || errors.Is(err, ErrOrganizationOutcomeUnknown) {
		t.Fatal("a failed commit:", err)
	}
	release()

	waiting, held := pgtest.HoldWrites(t, testEnv(t, s).db, "revisions")
	ctx, cancel := context.WithCancel(organizationTestContext())
	done := make(chan error, 1)
	go func() {
		_, err := s.Organize(ctx, action(), true)
		done <- err
	}()
	waiting(1)
	cancel()
	err := <-done
	held()
	if err == nil || errors.Is(err, ErrOrganizationOutcomeUnknown) {
		t.Fatal("a cancelled commit:", err)
	}
	if headOf(t, s) != before {
		t.Fatal("a failed action changed the notebook")
	}
	applied := action()
	result, err := s.Organize(organizationTestContext(), applied, true)
	if err != nil || !revisionPattern.MatchString(result.Revision) {
		t.Fatal(result, err)
	}
	if _, err = s.UndoOrganization(organizationTestContext(), OrganizationUndo{ID: "failure:1", Action: applied, Actor: "organizer-test", Revision: result.Revision}); err != nil {
		t.Fatal("committed outcome not undoable:", err)
	}
}

// A group whose commit fails applies none of its edits, and so does a group
// that loses a race with another gateway's group on the same base.
func TestDatabaseGroupFailedCommitAndRaceApplyNothing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a := saveNote(t, s, "a.md", "A")
	g := EditGroup{ID: "failing", Summary: "Two notes", Edits: []NoteEdit{{Path: "a.md", Content: "changed", Revision: a.Revision, Hash: ContentHash([]byte("A"))}, {Path: "new.md", Content: "new"}}}
	release := failCommits(t, s)
	if _, err := s.ApplyGroup(ctx, g); err == nil {
		t.Fatal("a group whose commit failed succeeded")
	}
	release()
	if headOf(t, s) != a.Revision {
		t.Fatal("a failed group moved the head")
	}
	if _, err := s.Read(ctx, "new.md", ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a failed group's new note:", err)
	}
	other := openGateway(t, s)
	first, err := other.ApplyGroup(ctx, EditGroup{ID: "first", Summary: "First draft", Edits: []NoteEdit{{Path: "a.md", Content: "First", Revision: a.Revision, Hash: ContentHash([]byte("A"))}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplyGroup(ctx, EditGroup{ID: "second", Summary: "Second draft", Edits: []NoteEdit{{Path: "a.md", Content: "Second", Revision: a.Revision, Hash: ContentHash([]byte("A"))}}}); !errors.Is(err, ErrConflict) {
		t.Fatal("a group over another gateway's:", err)
	}
	waitHead(t, s, first.Revision)
	if doc, _ := s.Read(ctx, "a.md", ""); string(doc.Content) != "First" || doc.Revision != first.Revision {
		t.Fatalf("the losing group wrote: %q", doc.Content)
	}
}
