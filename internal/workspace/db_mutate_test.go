package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"testing"
	"time"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// setLocks commits paths as the notebook's lock policy, in the format
// SetLock writes, and returns the head.
func setLocks(t *testing.T, s *Store, paths ...string) string {
	t.Helper()
	sort.Strings(paths)
	raw, _ := json.MarshalIndent(lockPolicy{1, append([]string{}, paths...)}, "", "  ")
	return commitFixture(t, s, fixture(locksPath, string(append(raw, '\n'))))
}

// addDiscussion commits a discussion of the note at path, in the format
// AddComment writes, and returns the head.
func addDiscussion(t *testing.T, s *Store, path string, comments ...Comment) string {
	t.Helper()
	raw, _ := json.MarshalIndent(comments, "", "  ")
	return commitFixture(t, s, fixture(commentPath(path), string(raw)))
}

// lockRulesOf is the lock rules over path at the head, as Locks reports
// them, read from the committed policy.
func lockRulesOf(t *testing.T, s *Store, path string) []LockRule {
	t.Helper()
	n := backendOf(s)
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.loadLocks(context.Background()); err != nil {
		t.Fatal(err)
	}
	return n.lockRules(path, n.locksRevision).Rules
}

// entryHistory is the note at path's history as its entry in the notes
// store keeps it, newest first: each revision that changed it and the path
// it had there. History follows a note through renames by this entry.
func entryHistory(t *testing.T, s *Store, path string) []Commit {
	t.Helper()
	ctx := context.Background()
	f, err := s.notes.Stat(ctx, gitobj.ID{}, path)
	if err != nil {
		t.Fatal(err)
	}
	infos, err := s.notes.History(ctx, notestore.HistoryQuery{EntryID: f.EntryID})
	if err != nil {
		t.Fatal(err)
	}
	var history []Commit
	for _, info := range infos {
		change, _ := entryChange(info.Changes, f.EntryID)
		history = append(history, Commit{Path: change.Path, Revision: info.ID.String(), Message: commitSubject(info.Commit.Message)})
	}
	return history
}

// A move or copy whose commit fails changes nothing, and succeeds once the
// store commits again.
func TestDatabaseMoveFailedCommitChangesNothing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	saveNote(t, s, "a.md", "# A\n")
	saveNote(t, s, "ref.md", "[A](a.md)\n")
	discussion := addDiscussion(t, s, "a.md", Comment{ID: "c1", Author: "reader", Text: "Feedback", Created: "2026-11-22T00:00:00Z"})
	release := failCommits(t, s)
	if _, err := s.Move(ctx, "a.md", "b.md", discussion, "owner"); err == nil {
		t.Fatal("a move whose commit failed succeeded")
	}
	if _, err := s.Duplicate(ctx, "a.md", "c.md", discussion, "owner"); err == nil {
		t.Fatal("a copy whose commit failed succeeded")
	}
	release()
	if head := headOf(t, s); head != discussion {
		t.Fatal("a failed commit moved the head")
	}
	for _, path := range []string{"b.md", "c.md"} {
		if _, err := s.Read(ctx, path, ""); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived a failed commit: %v", path, err)
		}
	}
	moved, err := s.Move(ctx, "a.md", "b.md", discussion, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if ref, _ := s.Read(ctx, "ref.md", ""); string(ref.Content) != "[A](b.md)\n" {
		t.Fatalf("backlink: %q", ref.Content)
	}
	if comments, err := s.Comments(ctx, "b.md"); err != nil || len(comments.Comments) != 1 || comments.Revision != moved.Revision {
		t.Fatalf("the discussion did not follow its note: %+v %v", comments, err)
	}
}

// A move commits on the head its checks read. When another gateway commits
// first, the move fails with a conflict instead of writing past it.
func TestDatabaseMoveConflictsWithAnotherGateway(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	doc := saveNote(t, s, "a.md", "# A\n")
	other := openGateway(t, s)
	if _, err := other.Save(ctx, "b.md", []byte("# B\n"), "", "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(ctx, "a.md", "c.md", doc.Revision, "owner"); !errors.Is(err, ErrConflict) {
		t.Fatal("a move past another gateway's commit:", err)
	}
	waitHead(t, s, headOf(t, other))
	if _, err := s.Move(ctx, "a.md", "c.md", headOf(t, other), "owner"); err != nil {
		t.Fatal(err)
	}
}

// waitHead waits until s has heard of head, which another gateway wrote.
func waitHead(t *testing.T, s *Store, head string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); headOf(t, s) != head; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("never heard of another gateway's head", head)
		}
	}
}

// openGateway opens s's notebook through a notes store of its own, as
// another gateway serving the same instance would.
func openGateway(t *testing.T, s *Store) *Store {
	t.Helper()
	env := testEnv(t, s)
	other, err := OpenDB(context.Background(), openTestNotes(t, env), s.notebook, t.TempDir(), s.browseExcluded)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	testNotebooks.Store(other, env)
	return other
}

// A note moved with its folder and then on its own keeps one entry, whose
// history names each of its paths with a readable version; History and
// HistoryScoped follow notes by this entry.
func TestDatabaseMoveHistoryFollowsSuccessiveMoves(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	saveNote(t, s, "outside.md", "outside")
	doc := saveNote(t, s, "one/a.md", "[x](../outside.md)")
	moved, err := s.Move(ctx, "one", "two/deep", doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if history := entryHistory(t, s, "two/deep/a.md"); len(history) != 2 || history[0].Path != "two/deep/a.md" || history[1].Path != "one/a.md" {
		t.Fatalf("folder move history: %+v", history)
	}
	if _, err = s.Move(ctx, "two/deep/a.md", "short.md", moved.Revision, "owner"); err != nil {
		t.Fatal(err)
	}
	history := entryHistory(t, s, "short.md")
	if len(history) != 3 || history[0].Message != `Move "two/deep/a.md" to "short.md"` || history[1].Message != `Move "one" to "two/deep"` {
		t.Fatalf("second rename history: %+v", history)
	}
	for _, version := range history {
		if _, err := s.Read(ctx, version.Path, version.Revision); err != nil {
			t.Fatal("old version cannot be opened", version, err)
		}
	}
}
