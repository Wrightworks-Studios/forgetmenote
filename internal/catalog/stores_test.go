package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/gitexport"
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

// gitNotebook leaves the working tree of a notebook the previous release kept
// in Git under data, as a cutover finds it, and returns its HEAD.
func gitNotebook(t *testing.T, data, id string) string {
	t.Helper()
	tree := filepath.Join(data, "projects", id, "workspace")
	if err := os.MkdirAll(tree, 0700); err != nil {
		t.Fatal(err)
	}
	testGit(t, tree, "init", "--quiet", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(tree, "kept.md"), []byte("# Kept in Git\n"), 0600); err != nil {
		t.Fatal(err)
	}
	testGit(t, tree, "add", "kept.md")
	testGit(t, tree, "commit", "--quiet", "-m", "Keep a note")
	return testGit(t, tree, "rev-parse", "HEAD")
}

// gitUnchanged fails unless the Git notebook is still exactly as it was left.
func gitUnchanged(t *testing.T, data, id, head string) {
	t.Helper()
	tree := filepath.Join(data, "projects", id, "workspace")
	if got := testGit(t, tree, "rev-parse", "HEAD"); got != head {
		t.Fatalf("the Git notebook moved from %s to %s", head, got)
	}
	if status := testGit(t, tree, "status", "--porcelain", "--untracked-files=all", "--ignored"); status != "" {
		t.Fatalf("the Git notebook's working tree changed:\n%s", status)
	}
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

// A catalog notebook still in the previous release's Git repository, which
// the notes store lacks, stops the catalog from opening, with the command
// that imports it. The catalog never converts or changes the repository; once
// imported, it serves the notebook with the repository's history.
func TestCatalogRefusesAGitNotebookTheNotesStoreLacks(t *testing.T) {
	data := t.TempDir()
	db := testDB(t, data)
	c, err := Open(data, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	err = c.Create(Bootstrap, "beta", "Beta")
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	notes := testNotes(t, data, db)
	if err = notes.Delete(t.Context(), "beta"); err != nil {
		t.Fatal(err)
	}
	head := gitNotebook(t, data, "beta")
	if c, err = Open(data, nil, db); err == nil {
		c.Close()
		t.Fatal("the catalog opened without the Git notebook it lists")
	}
	if !strings.Contains(err.Error(), "beta") || !strings.Contains(err.Error(), "forgetmenote admin storage import") {
		t.Fatalf("the refusal does not name the notebook and the import: %v", err)
	}
	gitUnchanged(t, data, "beta", head)
	if _, err = notes.Notebook("beta").Head(t.Context()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the refusal stored the notebook: %v", err)
	}
	// What the cutover's import does.
	if _, err = gitexport.ImportGitDir(t.Context(), notes.Notebook("beta"), filepath.Join(data, "projects", "beta", "workspace", ".git")); err != nil {
		t.Fatal(err)
	}
	c, err = Open(data, nil, db)
	if err != nil {
		t.Fatal("the imported notebook is still refused:", err)
	}
	defer c.Close()
	store, err := c.Access(Bootstrap, "beta", "read")
	if err != nil {
		t.Fatal(err)
	}
	if status, err := store.Status(t.Context()); err != nil || status.Revision != head {
		t.Fatalf("the imported notebook is at %+v, not at the repository's %s: %v", status, head, err)
	}
	gitUnchanged(t, data, "beta", head)
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

// A notebook the previous release kept on a notebook owner process lives in
// that owner's data directory, which no gateway of this release sees. The
// catalog refuses it, naming the owner and the import, until the cutover has
// imported it from there; then it serves it at the owner's head. The catalog
// keeps the owner record, so the previous release can still serve the
// instance until migrate -finish-rollout makes it refuse.
func TestCatalogServesAnOwnerNotebookOnceImported(t *testing.T) {
	data := t.TempDir()
	db := testDB(t, data)
	c, err := Open(data, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	err = c.Create(Bootstrap, "beta", "Beta")
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	notes := testNotes(t, data, db)
	if err = notes.Delete(t.Context(), "beta"); err != nil {
		t.Fatal(err)
	}
	owner := t.TempDir()
	head := gitNotebook(t, owner, "beta")
	var envelope map[string]any
	version, raw, err := db.ReadDocument(t.Context(), db.Pool, catalogKind, catalogID, -1)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope["data"].(map[string]any)["remoteProjects"] = map[string]string{"beta": "pool"}
	raw, _ = json.Marshal(envelope)
	if _, err = db.WriteDocument(t.Context(), db.Pool, catalogKind, catalogID, version, raw); err != nil {
		t.Fatal(err)
	}
	if c, err = Open(data, nil, db); err == nil {
		c.Close()
		t.Fatal("the catalog opened a notebook its owner holds and the notes store lacks")
	}
	for _, want := range []string{"beta", "pool", "forgetmenote admin storage import", "owner's data directory"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not say %q: %v", want, err)
		}
	}
	// What the cutover's import does on the owner's data directory.
	if _, err = gitexport.ImportGitDir(t.Context(), notes.Notebook("beta"), filepath.Join(owner, "projects", "beta", "workspace", ".git")); err != nil {
		t.Fatal(err)
	}
	c, err = Open(data, nil, db)
	if err != nil {
		t.Fatal("the imported owner notebook is still refused:", err)
	}
	defer c.Close()
	store, err := c.Access(Bootstrap, "beta", "read")
	if err != nil {
		t.Fatal(err)
	}
	if status, err := store.Status(t.Context()); err != nil || status.Revision != head {
		t.Fatalf("the imported notebook is at %+v, not at the owner's %s: %v", status, head, err)
	}
	if err = c.Create(Bootstrap, "gamma", "Gamma"); err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Data struct {
			RemoteProjects map[string]string `json:"remoteProjects"`
		} `json:"data"`
	}
	if err = json.Unmarshal([]byte(storedCatalog(t, c)), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Data.RemoteProjects["beta"] != "pool" || len(stored.Data.RemoteProjects) != 1 {
		t.Fatalf("a catalog change rewrote the owner record: %v", stored.Data.RemoteProjects)
	}
	gitUnchanged(t, owner, "beta", head)
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

// A Git repository the previous release left for an ID blocks creating a new
// notebook over it, and stays as it was.
func TestCreationRefusesAnUnimportedGitNotebook(t *testing.T) {
	data := t.TempDir()
	c, err := Open(data, nil, testDB(t, data))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	head := gitNotebook(t, data, "gamma")
	if err = c.Create(Bootstrap, "gamma", "Gamma"); err == nil || !strings.Contains(err.Error(), "forgetmenote admin storage import") {
		t.Fatalf("a creation went over a Git notebook: %v", err)
	}
	gitUnchanged(t, data, "gamma", head)
	if _, err = c.notes.Notebook("gamma").Head(t.Context()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the refused creation stored the notebook: %v", err)
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
