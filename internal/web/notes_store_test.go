package web

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
)

// The catalog keeps every notebook in the notes store, where each revision
// is a Git commit. Checks beneath the workspace, such as a commit's
// identities and full message, read that store the way another gateway of
// the instance does: a notes store of their own over the instance's database
// and blob directory.

var instanceNotesByApp sync.Map // *Server → *notestore.Store

// instanceNotes opens the notes store over the database and blob directory
// of the catalog that fixture gave app, once per app and test.
func instanceNotes(t testing.TB, app *Server) *notestore.Store {
	t.Helper()
	if notes, ok := instanceNotesByApp.Load(app); ok {
		return notes.(*notestore.Store)
	}
	blobs, err := blob.OpenDir(app.data)
	if err != nil {
		t.Fatal(err)
	}
	notes, err := notestore.Open(context.Background(), pgtest.Dir(t, app.data), blobs, notestore.Options{ImportEntryID: workspace.ImportEntryID})
	if err != nil {
		blobs.Close()
		t.Fatal(err)
	}
	instanceNotesByApp.Store(app, notes)
	t.Cleanup(func() {
		instanceNotesByApp.Delete(app)
		notes.Close()
		blobs.Close()
	})
	return notes
}

// notebookOf returns the ID of the catalog notebook that store is.
func notebookOf(t testing.TB, app *Server, store *workspace.Store) string {
	t.Helper()
	for _, p := range app.catalog.List(catalog.Bootstrap) {
		if s, err := app.catalog.Access(catalog.Bootstrap, p.ID, "read"); err == nil && s == store {
			return p.ID
		}
	}
	t.Fatal("the store is no notebook of the catalog")
	return ""
}

// commitOf returns revision of store's notebook as the notes store keeps
// it: the Git commit, with its identities and full message.
func commitOf(t testing.TB, app *Server, store *workspace.Store, revision string) notestore.CommitInfo {
	t.Helper()
	id, err := gitobj.ParseID(revision)
	if err != nil {
		t.Fatal(err)
	}
	info, err := instanceNotes(t, app).Notebook(notebookOf(t, app, store)).Commit(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// gitIdentity returns the author name and email, the committer name and
// email and the whole message of the commit at store's head, as
// git log -1 --format=%an%n%ae%n%cn%n%ce%n%B printed them.
func gitIdentity(t testing.TB, app *Server, store *workspace.Store) []string {
	t.Helper()
	status, err := store.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := commitOf(t, app, store, status.Revision).Commit
	return []string{c.Author.Name, c.Author.Email, c.Committer.Name, c.Committer.Email, strings.TrimSpace(c.Message)}
}

// commitElsewhere commits cs to store's notebook through another notes
// store, as another gateway or an import would, past every workspace check,
// and returns the new revision once store serves it as its head.
func commitElsewhere(t testing.TB, app *Server, store *workspace.Store, cs notestore.ChangeSet) string {
	t.Helper()
	ctx := context.Background()
	result, err := instanceNotes(t, app).Notebook(notebookOf(t, app, store)).Apply(ctx, cs)
	if err != nil {
		t.Fatal(err)
	}
	revision := result.Head.Commit.String()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		status, err := store.Status(ctx)
		if err == nil && status.Revision == revision {
			return revision
		}
		if time.Now().After(deadline) {
			t.Fatalf("the store did not serve revision %s committed elsewhere: %+v %v", revision, status, err)
		}
	}
}

// fixtureAuthor signs fixture commits made beneath the workspace.
var fixtureAuthor = gitobj.Signature{Name: "Fixture", Email: "fixture@example.invalid", When: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Unix(), TZ: "+0000"}

// git runs Git in dir for a repository a test builds, such as an upstream
// or a bundle to import, without hooks, signing or the user's identity.
func git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid"}, args...)...)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// previousReleaseTree makes store's Root the directory it was in the
// previous release, the notebook's Git working tree, which the cutover to
// the notes store imports and leaves in place, and returns it.
func previousReleaseTree(t testing.TB, store *workspace.Store) string {
	t.Helper()
	if err := os.MkdirAll(store.Root, 0700); err != nil {
		t.Fatal(err)
	}
	return store.Root
}
