package catalog

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
)

// testNotes opens the notes store of a stopped catalog's data directory, as
// an operator's tool would, for fixtures and checks that bypass the catalog.
func testNotes(t *testing.T, data string, db *pg.DB) *notestore.Store {
	t.Helper()
	blobs, err := blob.OpenDir(data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	notes, err := notestore.Open(t.Context(), db, blobs, notestore.Options{ImportEntryID: workspace.ImportEntryID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(notes.Close)
	return notes
}

// testGit runs Git in dir as an operator would, with no global configuration.
func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Operator", "GIT_AUTHOR_EMAIL=operator@example.test", "GIT_COMMITTER_NAME=Operator", "GIT_COMMITTER_EMAIL=operator@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Every notebook the catalog creates lives in the notes store, never in a Git
// working tree, and a restarted catalog serves it from there.
func TestCatalogKeepsNotebooksInTheNotesStore(t *testing.T) {
	data := t.TempDir()
	db := testDB(t, data)
	c, err := Open(data, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Create(Bootstrap, "alpha", "Alpha"); err != nil {
		c.Close()
		t.Fatal(err)
	}
	store, err := c.Access(Bootstrap, "alpha", "edit")
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	saved, err := store.Save(t.Context(), "proof.md", []byte("# Kept in the database\n"), "", "test")
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"default", "alpha"} {
		if _, err = os.Lstat(filepath.Join(data, "projects", id, "workspace")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("notebook %s has a working tree: %v", id, err)
		}
	}
	raw, _, err := testNotes(t, data, db).Notebook("alpha").Read(t.Context(), gitobj.ID{}, "proof.md")
	if err != nil || string(raw) != "# Kept in the database\n" {
		t.Fatalf("the notes store holds %q: %v", raw, err)
	}
	c, err = Open(data, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	store, err = c.Access(Bootstrap, "alpha", "read")
	if err != nil {
		t.Fatal(err)
	}
	if doc, err := store.Read(t.Context(), "proof.md", ""); err != nil || doc.Revision != saved.Revision {
		t.Fatalf("the restarted catalog reads %+v: %v", doc, err)
	}
}

// A catalog notebook the notes store lacks, with no repository to import, is
// refused rather than replaced by an empty notebook.
func TestCatalogRefusesANotebookTheNotesStoreLacks(t *testing.T) {
	data := t.TempDir()
	db := testDB(t, data)
	c, err := Open(data, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if err = testNotes(t, data, db).Delete(t.Context(), "default"); err != nil {
		t.Fatal(err)
	}
	if c, err = Open(data, nil, db); err == nil {
		c.Close()
		t.Fatal("the catalog replaced a missing notebook")
	}
	if !strings.Contains(err.Error(), "default") || !strings.Contains(err.Error(), "notes store") {
		t.Fatalf("the refusal does not name the missing notebook: %v", err)
	}
	if _, err = testNotes(t, data, db).Notebook("default").Head(t.Context()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the refusal created the notebook: %v", err)
	}
}

// A creation the catalog fails to publish removes the new notebook and any
// Git export of it, so the ID can be created again and backs up.
func TestFailedNotebookCreationLeavesNothingBehind(t *testing.T) {
	data := t.TempDir()
	c, err := Open(data, nil, testDB(t, data))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	// An export a lost removal left of an earlier notebook alpha.
	if _, err = c.notes.Create(ctx, "alpha", gitobj.Signature{Name: "Earlier", Email: "earlier@example.test", When: time.Now().Unix(), TZ: "+0000"}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.own.exporter.Sync(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err = c.notes.Delete(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	restore := pgtest.FailWrites(t, c.db, "catalog")
	if err = c.Create(Bootstrap, "alpha", "Alpha"); !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("an unpublished creation = %v", err)
	}
	restore()
	if _, err = c.notes.Notebook("alpha").Head(ctx); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the unpublished notebook stayed in the notes store: %v", err)
	}
	if _, err = os.Lstat(filepath.Join(data, "exports", "alpha.git")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the unpublished notebook's export stayed: %v", err)
	}
	if err = c.Create(Bootstrap, "alpha", "Alpha"); err != nil {
		t.Fatal("the ID cannot be created again:", err)
	}
	store, err := c.Access(Bootstrap, "alpha", "edit")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Backup(ctx, filepath.Join(t.TempDir(), "alpha.bundle")); err != nil {
		t.Fatal("the recreated notebook does not back up:", err)
	}
}

// A creation whose publication may have committed keeps its notebook, which
// the catalog serves once it sees the commit.
func TestCreationWithALostAcknowledgementKeepsItsNotebook(t *testing.T) {
	data := t.TempDir()
	c, err := Open(data, nil, testDB(t, data))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.commitFault = func() error { return errors.New("injected lost commit acknowledgement") }
	if err = c.Create(Bootstrap, "alpha", "Alpha"); !errors.Is(err, errCommitUnknown) {
		t.Fatalf("an unacknowledged creation = %v", err)
	}
	c.commitFault = nil
	store, err := c.Access(Bootstrap, "alpha", "edit")
	if err != nil {
		t.Fatal("the committed notebook is missing:", err)
	}
	if _, err = store.Save(t.Context(), "after.md", []byte("# After"), "", "test"); err != nil {
		t.Fatal(err)
	}
}

// A notebook a crash left in the notes store before the catalog published it
// is adopted by the next creation of its ID only while it is empty; one with
// content is never handed to whoever creates the ID next.
func TestCreationAdoptsOnlyAnEmptyUnpublishedNotebook(t *testing.T) {
	data := t.TempDir()
	c, err := Open(data, nil, testDB(t, data))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	for _, id := range []string{"empty", "full"} {
		if _, err = workspace.CreateDB(ctx, c.notes, id, t.TempDir(), nil); err != nil {
			t.Fatal(err)
		}
	}
	full, err := workspace.OpenDB(ctx, c.notes, "full", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = full.Save(ctx, "private.md", []byte("# Someone else's"), "", "test"); err != nil {
		t.Fatal(err)
	}
	full.Close()
	if err = c.Create(Bootstrap, "full", "Full"); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("a creation adopted an unpublished notebook with content: %v", err)
	}
	if raw, _, err := c.notes.Notebook("full").Read(ctx, gitobj.ID{}, "private.md"); err != nil || string(raw) != "# Someone else's" {
		t.Fatalf("the refused creation changed the notebook: %q %v", raw, err)
	}
	if err = c.Create(Bootstrap, "empty", "Empty"); err != nil {
		t.Fatal("a creation refused an empty unpublished notebook:", err)
	}
	if _, err = c.Access(Bootstrap, "empty", "edit"); err != nil {
		t.Fatal(err)
	}
}

// sharedGateway opens a second gateway's catalog over db's org, with its own
// data directory and notes store over the blob store first keeps.
func sharedGateway(t *testing.T, db *pg.DB, first string) *Catalog {
	t.Helper()
	gateway, _ := pgtest.Gateway(t, db)
	blobs, err := blob.OpenDir(first)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	notes, err := notestore.Open(t.Context(), gateway, blobs, notestore.Options{ImportEntryID: workspace.ImportEntryID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(notes.Close)
	c, err := OpenWithNotes(t.TempDir(), nil, gateway, notes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	pgtest.Listening(t, gateway, "notebook_revisions")
	return c
}

// Gateways that share an instance's database serve the same notebooks, and a
// mutation through one wakes another's watch at once rather than at its next
// fallback poll.
func TestGatewaysShareNotebooksAndHearEachOthersRevisions(t *testing.T) {
	db := pgtest.Org(t)
	first := t.TempDir()
	a, err := Open(first, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err = a.Create(Bootstrap, "alpha", "Alpha"); err != nil {
		t.Fatal(err)
	}
	b := sharedGateway(t, db, first)
	ctx := context.Background()
	watched, err := b.Access(Bootstrap, "alpha", "read")
	if err != nil {
		t.Fatal("the second gateway does not serve the notebook:", err)
	}
	// The watch hears only of revisions after the one the gateway knows.
	if _, err = watched.Status(ctx); err != nil {
		t.Fatal(err)
	}
	hints, stop, err := watched.Watch()
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	select {
	case <-hints:
		t.Fatal("the watch hinted at a change nobody made")
	case <-time.After(200 * time.Millisecond):
	}
	began := time.Now()
	store, err := a.Access(Bootstrap, "alpha", "edit")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Save(ctx, "woken.md", []byte("# Woken"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-hints:
	case <-time.After(10 * time.Second):
		t.Fatal("the other gateway's watch never heard of the mutation")
	}
	// The notes store's fallback poll runs every five seconds.
	if time.Since(began) >= 4*time.Second {
		t.Fatal("the watch waited for its fallback poll")
	}
	if doc, err := watched.Read(ctx, "woken.md", ""); err != nil || doc.Revision != saved.Revision {
		t.Fatalf("the other gateway reads %+v: %v", doc, err)
	}
}
