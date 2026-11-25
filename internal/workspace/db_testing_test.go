package workspace

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// A testNotebook is where a test's notebook lives: its org's database and
// its blob directory, which a reopened store finds again.
type testNotebook struct {
	db    *pg.DB
	blobs *blob.Dir
}

var testNotebooks sync.Map // *Store to *testNotebook

// newTestStore opens a new notebook kept in the notes store, in an org and
// blob directory of its own. Each test's notebook is "notes".
func newTestStore(t testing.TB) *Store {
	t.Helper()
	return newTestStoreExcluding(t, nil)
}

func newTestStoreExcluding(t testing.TB, excluded []string) *Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "blobs"), 0700); err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.OpenDir(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	env := &testNotebook{db: pgtest.Org(t), blobs: blobs}
	s, err := CreateDB(context.Background(), openTestNotes(t, env), "notes", filepath.Join(dir, "projects", "notes"), excluded)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	testNotebooks.Store(s, env)
	return s
}

func openTestNotes(t testing.TB, env *testNotebook) *notestore.Store {
	t.Helper()
	notes, err := notestore.Open(context.Background(), env.db, env.blobs, notestore.Options{ImportEntryID: NoteEntryID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(notes.Close)
	return notes
}

func testEnv(t testing.TB, s *Store) *testNotebook {
	t.Helper()
	env, ok := testNotebooks.Load(s)
	if !ok {
		t.Fatal("not a test store kept in the notes store")
	}
	return env.(*testNotebook)
}

// reopenTestStore closes s and opens its notebook again through a new notes
// store with cold caches, as a restarted process would.
func reopenTestStore(t testing.TB, s *Store) *Store {
	t.Helper()
	return reopenTestStoreExcluding(t, s, s.browseExcluded)
}

// reopenTestStoreExcluding is reopenTestStore for a restart with the
// browse exclusions excluded.
func reopenTestStoreExcluding(t testing.TB, s *Store, excluded []string) *Store {
	t.Helper()
	env := testEnv(t, s)
	s.Close()
	again, err := OpenDB(context.Background(), openTestNotes(t, env), s.notebook, filepath.Dir(s.Root), excluded)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { again.Close() })
	testNotebooks.Store(again, env)
	return again
}

var fixtureAuthor = gitobj.Signature{Name: "Fixture", Email: "fixture@example.com"}

// fixture writes path with content; use notestore.FileChange directly for
// modes, renames and deletions.
func fixture(path, content string) notestore.FileChange {
	return notestore.FileChange{Path: path, Content: []byte(content)}
}

// commitFixture commits changes straight into s's notebook, as an import or
// another gateway would, past every workspace check, and returns the head.
func commitFixture(t testing.TB, s *Store, changes ...notestore.FileChange) string {
	t.Helper()
	r, err := s.notes.Apply(context.Background(), notestore.ChangeSet{Subject: "Fixture", Author: fixtureAuthor, Files: changes, AllowEmpty: len(changes) == 0})
	if err != nil {
		t.Fatal(err)
	}
	return r.Head.Commit.String()
}

// commitFiles commits files, path to content, as one fixture revision.
func commitFiles(t testing.TB, s *Store, files map[string]string) string {
	t.Helper()
	var changes []notestore.FileChange
	for p, content := range files {
		changes = append(changes, fixture(p, content))
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return commitFixture(t, s, changes...)
}

// failCommits makes every commit to s's org fail inside its transaction,
// as a full disk or a lost primary would, until the returned function runs.
func failCommits(t testing.TB, s *Store) func() {
	t.Helper()
	return pgtest.FailTable(t, testEnv(t, s).db, "revisions")
}
