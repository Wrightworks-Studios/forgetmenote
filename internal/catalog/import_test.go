package catalog

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
)

type importCallbackReader struct {
	io.Reader
	beforeRead func()
}

func (r *importCallbackReader) Read(p []byte) (int, error) {
	if r.beforeRead != nil {
		run := r.beforeRead
		r.beforeRead = nil
		run()
	}
	return r.Reader.Read(p)
}

// unpublished fails unless nothing of a refused import of id remains: no
// catalog entry, notebook, export, state directory or staging directory.
func unpublished(t *testing.T, c *Catalog, data, id string) {
	t.Helper()
	if len(c.List(Bootstrap)) != 1 || c.StorageOwner(id) != "" {
		t.Fatal("a failed import published a notebook")
	}
	if _, err := c.notes.Notebook(id).Head(t.Context()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a failed import left notebook %s in the notes store: %v", id, err)
	}
	for _, path := range []string{filepath.Join(data, "exports", id+".git"), filepath.Join(data, "projects", id)} {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("a failed import left %s: %v", path, err)
		}
	}
	noImportStages(t, data)
}

func TestImportRechecksPermissionsAndRollsBackFailures(t *testing.T) {
	for _, scenario := range []string{"revoked", "catalog-write", "git-repository", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			data := t.TempDir()
			c, err := Open(data, nil, testDB(t, data))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if err := c.SetUser(Bootstrap, User{ID: "alice", CanCreateProjects: true}); err != nil {
				t.Fatal(err)
			}
			source, _ := c.Access(Bootstrap, "default", "edit")
			if _, err := source.Save(context.Background(), "carried.md", []byte("# Carried"), "", ""); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "test.bundle")
			if err := source.Backup(context.Background(), path); err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(path)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reader := &importCallbackReader{Reader: bytes.NewReader(raw)}
			var head string
			switch scenario {
			case "revoked":
				reader.beforeRead = func() {
					if err := c.SetUser(Bootstrap, User{ID: "alice"}); err != nil {
						t.Fatal(err)
					}
				}
			case "catalog-write":
				pgtest.FailWrites(t, c.db, "catalog")
			case "git-repository":
				head = gitNotebook(t, data, "imported")
			case "cancelled":
				reader.beforeRead = cancel
			}
			err = c.Import(ctx, "alice", "imported", "Imported notebook", reader)
			if err == nil {
				t.Fatal("failed import succeeded")
			}
			if scenario == "revoked" && !errors.Is(err, workspace.ErrForbidden) {
				t.Fatal(err)
			}
			if scenario == "git-repository" {
				if !strings.Contains(err.Error(), "forgetmenote admin storage import") {
					t.Fatalf("the refusal does not name the import: %v", err)
				}
				gitUnchanged(t, data, "imported", head)
				os.RemoveAll(filepath.Join(data, "projects", "imported"))
			}
			unpublished(t, c, data, "imported")
		})
	}
}

func TestImportRetainsContentAfterAmbiguousPublishedCatalog(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	source, err := c.Access(Bootstrap, "default", "edit")
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = source.Save(ctx, "important.md", []byte("must survive publication failure"), "", ""); err != nil {
		c.Close()
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "source.bundle")
	if err = source.Backup(ctx, bundle); err != nil {
		c.Close()
		t.Fatal(err)
	}
	raw, err := os.ReadFile(bundle)
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	// The Git bundle, restore and catalog commit stay real. Only the commit's
	// acknowledgement is lost after the database applied it.
	c.commitFault = func() error { return errors.New("injected lost commit acknowledgement") }
	if err = c.Import(ctx, Bootstrap, "restored", "Restored", bytes.NewReader(raw)); err == nil {
		c.Close()
		t.Fatal("lost commit acknowledgement was ignored")
	}
	c.Close()
	c, err = Open(dir, nil, testDB(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	restored, err := c.Access(Bootstrap, "restored", "read")
	if err != nil {
		t.Fatal("published import missing after recovery", err)
	}
	note, err := restored.Read(ctx, "important.md", "")
	if err != nil || string(note.Content) != "must survive publication failure" {
		t.Fatal("uncertain publication discarded imported content", err)
	}
}

// importSource is a notebook with history to import, made with Git as any
// earlier release or other tool would: its bundle, HEAD, its note's revisions
// and the note's first and current content.
type importSource struct {
	bundle   []byte
	revision string
	history  []string
	first    string
	content  string
}

func newImportSource(t *testing.T, content string) importSource {
	t.Helper()
	tree := t.TempDir()
	testGit(t, tree, "init", "--quiet", "--initial-branch=main")
	testGit(t, tree, "commit", "--quiet", "--allow-empty", "-m", "Initialize notes notebook")
	if err := os.MkdirAll(filepath.Join(tree, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	source := importSource{content: content}
	for _, text := range []string{"# Original\n", content} {
		if err := os.WriteFile(filepath.Join(tree, "notes", "proof.md"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		testGit(t, tree, "add", "notes/proof.md")
		testGit(t, tree, "commit", "--quiet", "-m", "Update notes/proof.md")
		if source.first == "" {
			source.first = testGit(t, tree, "rev-parse", "HEAD")
		}
	}
	source.revision = testGit(t, tree, "rev-parse", "HEAD")
	source.history = strings.Fields(testGit(t, tree, "log", "--format=%H", "--", "notes/proof.md"))
	path := filepath.Join(t.TempDir(), "source.bundle")
	testGit(t, tree, "bundle", "create", "--quiet", path, "--all")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source.bundle = raw
	return source
}

// check fails unless store holds the source's history at its original revision.
func (source importSource) check(t *testing.T, store *workspace.Store) {
	t.Helper()
	ctx := context.Background()
	status, err := store.Status(ctx)
	if err != nil || status.Dirty || status.Revision != source.revision {
		t.Fatalf("the imported notebook is not at the bundle's revision %s: %+v %v", source.revision, status, err)
	}
	history, err := store.History(ctx, "notes/proof.md")
	var revisions []string
	for _, commit := range history {
		revisions = append(revisions, commit.Revision)
	}
	if err != nil || !slices.Equal(revisions, source.history) {
		t.Fatalf("the imported note's history %v differs from the bundle's %v: %v", revisions, source.history, err)
	}
	if doc, err := store.Read(ctx, "notes/proof.md", ""); err != nil || string(doc.Content) != source.content {
		t.Fatalf("imported content: %q %v", doc.Content, err)
	}
	if doc, err := store.Read(ctx, "notes/proof.md", source.first); err != nil || string(doc.Content) != "# Original\n" {
		t.Fatalf("imported old revision: %q %v", doc.Content, err)
	}
}

func noImportStages(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if stages, _ := filepath.Glob(filepath.Join(dir, ".notebook-import-*")); len(stages) != 0 {
			t.Fatal("staging directories leaked", stages)
		}
	}
}

// An import keeps the bundle's history in the notes store, every commit with
// its ID, and every gateway of the instance serves it.
func TestImportKeepsTheBundlesHistoryInTheNotesStore(t *testing.T) {
	db := pgtest.Org(t)
	data := t.TempDir()
	gateway, err := Open(data, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	source := newImportSource(t, "# Restored\n")
	if err = gateway.Import(context.Background(), Bootstrap, "restored", "Restored", bytes.NewReader(source.bundle)); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(filepath.Join(data, "projects", "restored", "workspace")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the import made a working tree: %v", err)
	}
	store, err := gateway.Access(Bootstrap, "restored", "owner")
	if err != nil {
		t.Fatal(err)
	}
	source.check(t, store)
	other := sharedGateway(t, db, data)
	store, err = other.Access(Bootstrap, "restored", "read")
	if err != nil {
		t.Fatal(err)
	}
	source.check(t, store)
	noImportStages(t, data)
}

// A refused import publishes nothing, and importing the bundle again
// succeeds: nothing is left that would block the retry.
func TestFailedImportsPublishNothingAndCanBeRetried(t *testing.T) {
	for _, scenario := range []string{"invalid", "oversized", "held", "catalog-write"} {
		t.Run(scenario, func(t *testing.T) {
			data := t.TempDir()
			gateway, err := Open(data, nil, testDB(t, data))
			if err != nil {
				t.Fatal(err)
			}
			defer gateway.Close()
			ctx := context.Background()
			source := newImportSource(t, "# Restored\n")
			attempt, id, retry := source.bundle, "restored", func() {}
			var want error
			switch scenario {
			case "invalid":
				attempt, want = []byte("# not a Git bundle\n"), workspace.ErrBundle
			case "oversized":
				t.Setenv("FMN_LIMIT_BUNDLE_BYTES", strconv.Itoa(len(source.bundle)))
				attempt, want = bytes.Repeat([]byte("x"), len(source.bundle)+1), workspace.ErrBundleTooLarge
			case "held":
				// A creation that never published left this ID's notebook,
				// with content, in the notes store.
				held, err := workspace.CreateDB(ctx, gateway.notes, id, t.TempDir(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = held.Save(ctx, "kept.md", []byte("# Kept"), "", "operator"); err != nil {
					t.Fatal(err)
				}
				held.Close()
				want = workspace.ErrConflict
			case "catalog-write":
				want, retry = ErrStorageUnavailable, pgtest.FailWrites(t, gateway.db, "catalog")
			}
			if err = gateway.Import(ctx, Bootstrap, id, "Restored", bytes.NewReader(attempt)); !errors.Is(err, want) {
				t.Fatalf("import: %v, want %v", err, want)
			}
			retry()
			if scenario == "held" {
				if raw, _, err := gateway.notes.Notebook(id).Read(ctx, gitobj.ID{}, "kept.md"); err != nil || string(raw) != "# Kept" {
					t.Fatalf("the import replaced the held notebook: %q %v", raw, err)
				}
				id = "restored-copy"
			}
			unpublished(t, gateway, data, id)
			if err = gateway.Import(ctx, Bootstrap, id, "Restored", bytes.NewReader(source.bundle)); err != nil {
				t.Fatal("retry:", err)
			}
			store, err := gateway.Access(Bootstrap, id, "owner")
			if err != nil {
				t.Fatal(err)
			}
			source.check(t, store)
			noImportStages(t, data)
		})
	}
}

// A lost acknowledgement of the catalog commit leaves the import published.
// A gateway that has not yet seen the commit still refuses to import that ID
// again rather than replace it.
func TestImportSurvivesALostCatalogAcknowledgement(t *testing.T) {
	db := pgtest.Org(t)
	data := t.TempDir()
	gateway, err := Open(data, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	stale := sharedGateway(t, db, data)
	stale.freshness = time.Hour
	ctx := context.Background()
	source := newImportSource(t, "# Restored\n")
	gateway.commitFault = func() error { return errors.New("injected lost commit acknowledgement") }
	if err = gateway.Import(ctx, Bootstrap, "restored", "Restored", bytes.NewReader(source.bundle)); err == nil {
		t.Fatal("lost commit acknowledgement was ignored")
	}
	gateway.commitFault = nil
	rival := newImportSource(t, "# Rival\n")
	for _, g := range []*Catalog{stale, gateway} {
		if err = g.Import(ctx, Bootstrap, "restored", "Rival", bytes.NewReader(rival.bundle)); !errors.Is(err, workspace.ErrConflict) {
			t.Fatalf("a second import of a published notebook: %v", err)
		}
	}
	other := sharedGateway(t, db, data)
	store, err := other.Access(Bootstrap, "restored", "read")
	if err != nil {
		t.Fatal("published import missing after recovery", err)
	}
	source.check(t, store)
}

// A creation while an import of its ID restores takes nothing: the ID stays
// the import's.
func TestCreationRefusesAnIDBeingImported(t *testing.T) {
	data := t.TempDir()
	c, err := Open(data, nil, testDB(t, data))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	source := newImportSource(t, "# Restored\n")
	reader := &importCallbackReader{Reader: bytes.NewReader(source.bundle), beforeRead: func() {
		if err := c.Create(Bootstrap, "restored", "Rival"); !errors.Is(err, workspace.ErrConflict) {
			t.Errorf("a creation took an ID being imported: %v", err)
		}
	}}
	if err = c.Import(context.Background(), Bootstrap, "restored", "Restored", reader); err != nil {
		t.Fatal(err)
	}
	store, err := c.Access(Bootstrap, "restored", "read")
	if err != nil {
		t.Fatal(err)
	}
	source.check(t, store)
}
