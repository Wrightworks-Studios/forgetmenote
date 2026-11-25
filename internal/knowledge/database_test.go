package knowledge

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
	"forgetmenote/internal/workspace/workspacetest"
)

// database returns the org a notebook's engine directory keeps, so an engine
// reopened on the same store finds what the previous one stored. The test
// notebooks keep their content in the notes store of the same org.
func database(t testing.TB, store *workspace.Store) *pg.DB {
	t.Helper()
	return pgtest.Dir(t, filepath.Dir(store.Root))
}

// testNotebooks has the notes store's notebook behind each test store.
var testNotebooks sync.Map // *workspace.Store to *notestore.Notebook

// openNotebook opens the notebook kept in the notes store whose state
// directory is dir, creating it on first use, with excluded hidden as the
// instance configuration hides it.
func openNotebook(t testing.TB, dir string, excluded []string) *workspace.Store {
	t.Helper()
	store, nb := workspacetest.New(t, dir, excluded)
	testNotebooks.Store(store, nb)
	return store
}

// newNotebook opens a new, empty notebook kept in the notes store.
func newNotebook(t testing.TB, excluded []string) *workspace.Store {
	t.Helper()
	return openNotebook(t, t.TempDir(), excluded)
}

var fixtureAuthor = gitobj.Signature{Name: "History fixture", Email: "history@example.invalid"}

// commitFixture commits changes straight into store's notebook as one
// revision, past every workspace check and limit, as an import or another
// gateway would, and returns the new head.
func commitFixture(t testing.TB, store *workspace.Store, subject string, changes ...notestore.FileChange) string {
	t.Helper()
	nb, ok := testNotebooks.Load(store)
	if !ok {
		t.Fatal("not a test notebook kept in the notes store")
	}
	r, err := nb.(*notestore.Notebook).Apply(context.Background(), notestore.ChangeSet{Subject: subject, Author: fixtureAuthor, Files: changes})
	if err != nil {
		t.Fatal(err)
	}
	return r.Head.Commit.String()
}

// importedNotebook builds a history with build in a new Git repository,
// running git there, then imports what its HEAD reaches into a new notebook
// kept in the notes store, as an import of Git history does, and opens
// it.
func importedNotebook(t *testing.T, build func(git func(args ...string) string, repo string)) *workspace.Store {
	t.Helper()
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		base := []string{"-C", repo, "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "user.name=History test", "-c", "user.email=history@example.invalid"}
		out, err := exec.Command("git", append(base, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("fixture git %v: %s (%v)", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet", "--initial-branch=main")
	build(git, repo)
	dir := filepath.Join(t.TempDir(), "imported")
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0700); err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.OpenDir(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	defer blobs.Close()
	notes, err := notestore.Open(context.Background(), pgtest.Dir(t, dir), blobs, notestore.Options{ImportEntryID: workspace.ImportEntryID})
	if err != nil {
		t.Fatal(err)
	}
	defer notes.Close()
	if _, err = gitexport.ImportGitDir(context.Background(), notes.Notebook(filepath.Base(dir)), filepath.Join(repo, ".git")); err != nil {
		t.Fatal(err)
	}
	return openNotebook(t, dir, nil)
}

// holdRevisionReads makes every read of the notes store's revisions wait, as
// a stalled database would, until release runs or the test ends; waiting
// returns once a read waits. It locks the table for every org, so it suits
// only tests that do not run in parallel.
func holdRevisionReads(t testing.TB, db *pg.DB) (waiting func(), release func()) {
	t.Helper()
	ctx := context.Background()
	hold, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { hold.Rollback(ctx) }) }
	t.Cleanup(release)
	if _, err = hold.Exec(ctx, "LOCK TABLE revisions IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	waiting = func() {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); pgtest.LockWaiters(t, db) == 0; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("nothing read the held revisions")
			}
		}
	}
	return waiting, release
}

// vectorSetVersion is the stored version of the engine's vector set, 0 when
// there is none.
func vectorSetVersion(t testing.TB, e *Engine) int64 {
	t.Helper()
	var version int64
	err := e.db.Pool.QueryRow(context.Background(), `SELECT coalesce((SELECT version FROM knowledge_vector_sets WHERE org = $1 AND notebook = $2), 0)`, e.db.Org, e.notebook).Scan(&version)
	if err != nil {
		t.Fatal(err)
	}
	return version
}

// storedVectors returns the engine's stored vector rows by content hash.
func storedVectors(t testing.TB, e *Engine) map[string][]byte {
	t.Helper()
	rows, err := e.db.Pool.Query(context.Background(), `SELECT hash, vector FROM knowledge_vectors WHERE org = $1 AND notebook = $2`, e.db.Org, e.notebook)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	vectors := map[string][]byte{}
	for rows.Next() {
		var hash string
		var raw []byte
		if err = rows.Scan(&hash, &raw); err != nil {
			t.Fatal(err)
		}
		vectors[hash] = raw
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return vectors
}

// failVectors makes Postgres refuse the engine's vector writes, as a full disk
// or lost primary would, until the returned function runs.
func failVectors(t testing.TB, e *Engine) func() {
	t.Helper()
	return pgtest.FailTable(t, e.db, "knowledge_vector_sets")
}

// storedDocument returns one of the engine's documents, version 0 when unwritten.
func storedDocument(t testing.TB, e *Engine, kind string) (int64, string) {
	t.Helper()
	version, raw, err := e.db.ReadDocument(context.Background(), e.db.Pool, kind, e.notebook, -1)
	if err != nil {
		t.Fatal(err)
	}
	return version, string(raw)
}

// writeDocument replaces one of the engine's documents, as a restore or another
// gateway would.
func writeDocument(t testing.TB, e *Engine, kind, body string) {
	t.Helper()
	version, _ := storedDocument(t, e, kind)
	if _, err := e.db.WriteDocument(context.Background(), e.db.Pool, kind, e.notebook, version, []byte(body)); err != nil {
		t.Fatal(err)
	}
}
