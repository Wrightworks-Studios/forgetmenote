package workspace

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// lockManuscript locks the manuscript folder, for tests of locked paths.
func lockManuscript(t *testing.T, s *Store) {
	t.Helper()
	state, err := s.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetLock(context.Background(), "manuscript", true, state.Revision, "test setup"); err != nil {
		t.Fatal(err)
	}
}

func TestSavePersistsHistoryAndRejectsStaleRevision(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a, err := s.Save(ctx, "notes/idea.md", []byte("# Original\n"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Save(ctx, a.Path, []byte("# Revised\n"), a.Revision, "writer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(ctx, a.Path, []byte("overwrite"), a.Revision, "assistant"); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	now, err := s.Read(ctx, a.Path, "")
	if err != nil || string(now.Content) != "# Revised\n" || b.Revision == a.Revision || now.Version != b.Revision {
		t.Fatalf("bad saved result: %+v %v", now, err)
	}
	old, err := s.Read(ctx, a.Path, a.Revision)
	if err != nil || string(old.Content) != "# Original\n" || old.Version != a.Revision {
		t.Fatalf("history missing: %+v %v", old, err)
	}
	history, err := s.notes.History(ctx, notestore.HistoryQuery{Path: a.Path})
	if err != nil || len(history) != 2 || history[0].ID.String() != b.Revision || history[1].ID.String() != a.Revision {
		t.Fatalf("history: %d %v", len(history), err)
	}
}

func TestConcurrentEditsHaveOneWinner(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a, err := s.Save(ctx, "notes/a.md", []byte("a"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, value := range []string{"b", "c"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Save(ctx, a.Path, []byte(value), a.Revision, "test")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
}

// Paths outside the notebook, hidden ones and locked ones are refused, and a
// symbolic link in the tree is never followed.
func TestPathsAndReadOnlyAreas(t *testing.T) {
	s := newTestStore(t)
	commitFixture(t, s,
		fixture(locksPath, `{"version":1,"paths":["manuscript"]}`),
		notestore.FileChange{Path: "linked", Content: []byte("manuscript"), Mode: gitobj.ModeSymlink},
		fixture("manuscript/secret.md", "secret"),
	)
	ctx := context.Background()
	for _, p := range []string{"../outside.md", "/tmp/out.md", ".git/config", "notes/../x.md", "notes/.hidden.md", "manuscript/chapter.md", locksPath, "AGENTS.md"} {
		if _, err := s.Save(ctx, p, []byte("x"), "", "test"); err == nil {
			t.Fatalf("accepted %q", p)
		}
	}
	if _, err := s.Read(ctx, "linked/secret.md", ""); err == nil {
		t.Fatal("followed symlink")
	}
	if _, err := s.Read(ctx, locksPath, ""); !errors.Is(err, ErrForbidden) {
		t.Fatal("read a service-managed policy file:", err)
	}
}

// A commit that fails leaves the note, the head and the watchers as they
// were, and the same save succeeds once commits work again.
func TestCommitFailureRestoresOriginal(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a, err := s.Save(ctx, "notes/a.md", []byte("a"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	hints, cancel, err := s.Watch()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	restore := failCommits(t, s)
	if _, err = s.Save(ctx, a.Path, []byte("b"), a.Revision, "test"); err == nil {
		t.Fatal("expected failure")
	}
	if doc, err := s.Read(ctx, a.Path, ""); err != nil || string(doc.Content) != "a" || doc.Revision != a.Revision {
		t.Fatalf("failed save left %q at %s: %v", doc.Content, doc.Revision, err)
	}
	select {
	case <-hints:
		t.Fatal("a failed commit was hinted")
	default:
	}
	restore()
	if _, err = s.Save(ctx, a.Path, []byte("b"), a.Revision, "test"); err != nil {
		t.Fatal("the retry after the failure:", err)
	}
	if again := reopenTestStore(t, s); readContent(t, again, a.Path) != "b" {
		t.Fatal("the retried save did not persist")
	}
}

func readContent(t testing.TB, s *Store, path string) string {
	t.Helper()
	doc, err := s.Read(context.Background(), path, "")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(doc.Content)
}
