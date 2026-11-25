package catalog

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
)

// failingUpload runs beforeRead once, then fails with err, as an upload whose
// client disconnects does.
type failingUpload struct {
	beforeRead func()
	err        error
}

func (r *failingUpload) Read(p []byte) (int, error) {
	if r.beforeRead != nil {
		run := r.beforeRead
		r.beforeRead = nil
		run()
	}
	return 0, r.err
}

// instanceIntact fails unless the instance on db and the blob directory data
// still serves: a notes store of its own holds every notebook the catalog
// lists, want maps a notebook path to the content that notebook research
// holds (nil when research must be in neither), b serves its untouched
// notebook default after reloading its catalog, and a gateway starting now
// opens the catalog.
func instanceIntact(t *testing.T, db *pg.DB, data string, b *Catalog, want map[string]string) {
	t.Helper()
	ctx := t.Context()
	gateway, _ := pgtest.Gateway(t, db)
	blobs, err := blob.OpenDir(data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	notes, err := notestore.Open(ctx, gateway, blobs, notestore.Options{ImportEntryID: workspace.ImportEntryID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(notes.Close)
	_, headErr := notes.Notebook("research").Head(ctx)
	switch {
	case want == nil && !errors.Is(headErr, fs.ErrNotExist):
		t.Errorf("the notes store holds research, which no one published: %v", headErr)
	case want != nil && headErr != nil:
		t.Errorf("the notes store lost research, which a gateway published: %v", headErr)
	}
	for path, content := range want {
		if raw, _, err := notes.Notebook("research").Read(ctx, gitobj.ID{}, path); err != nil || string(raw) != content {
			t.Errorf("research/%s holds %q, want %q: %v", path, raw, content, err)
		}
	}
	b.mu.Lock()
	b.checked = time.Time{}
	b.mu.Unlock()
	if _, err := b.Access(Bootstrap, "default", "read"); err != nil {
		t.Errorf("gateway B, its catalog reloaded, opens the untouched notebook default: %v", err)
	}
	if listed := b.StorageOwner("research") != ""; listed != (want != nil) {
		t.Errorf("gateway B lists research: %v, want %v", listed, want != nil)
	}
	restarted, err := OpenWithNotes(t.TempDir(), nil, gateway, notes)
	if err != nil {
		t.Fatalf("a gateway starting on the instance: %v", err)
	}
	restarted.Close()
}

// A notebook ID one gateway is importing is reserved on every gateway: a
// creation or import of it on another gateway is a conflict while the import
// reads, checks and stores its upload, and the import's failure never
// deletes a notebook another gateway published.
func TestImportOnOneGatewayNeverDeletesANotebookAnotherPublished(t *testing.T) {
	for _, scenario := range []string{"damaged bundle", "disconnect", "import of the same ID", "valid bundle"} {
		t.Run(scenario, func(t *testing.T) {
			db := pgtest.Org(t)
			data := t.TempDir()
			a, err := Open(data, nil, db)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(a.Close)
			b := sharedGateway(t, db, data)
			ctx := t.Context()
			var onA error
			published := map[string]string{}
			createOnA := func() {
				if onA = a.Create(Bootstrap, "research", "Research"); onA != nil {
					return
				}
				store, err := a.Access(Bootstrap, "research", "edit")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.Save(ctx, "plan.md", []byte("# Plan\n\nOnly copy.\n"), "", "test"); err != nil {
					t.Fatal(err)
				}
				published["plan.md"] = "# Plan\n\nOnly copy.\n"
			}
			var upload io.Reader
			var imported map[string]string
			switch scenario {
			case "damaged bundle":
				upload = &importCallbackReader{Reader: bytes.NewReader([]byte("# not a Git bundle\n")), beforeRead: createOnA}
			case "disconnect":
				upload = &failingUpload{beforeRead: createOnA, err: io.ErrUnexpectedEOF}
			case "import of the same ID":
				source := newImportSource(t, "# Plan\n\nOnly copy.\n")
				upload = &failingUpload{beforeRead: func() {
					if onA = a.Import(ctx, Bootstrap, "research", "Research", bytes.NewReader(source.bundle)); onA == nil {
						published["notes/proof.md"] = source.content
					}
				}, err: io.ErrUnexpectedEOF}
			case "valid bundle":
				source := newImportSource(t, "# Restored\n")
				upload = &importCallbackReader{Reader: bytes.NewReader(source.bundle), beforeRead: createOnA}
				imported = map[string]string{"notes/proof.md": source.content}
			}
			err = b.Import(ctx, Bootstrap, "research", "Research", upload)
			if onA != nil && !errors.Is(onA, workspace.ErrConflict) {
				t.Fatalf("gateway A's notebook research while B imports it: %v", onA)
			}
			if onA == nil {
				t.Error("gateway A published research while gateway B was importing it")
			}
			if imported != nil && onA != nil {
				if err != nil {
					t.Fatalf("gateway B's import: %v", err)
				}
				published = imported
			} else if imported == nil && err == nil {
				t.Fatal("gateway B imported an upload that failed")
			}
			if len(published) == 0 {
				published = nil
			}
			instanceIntact(t, db, data, b, published)
		})
	}
}

// A creation on gateway A while gateway B's import stores its history never
// adopts the half-imported notebook, which B's failure would then delete
// under A's published entry.
func TestCreationOnAnotherGatewayNeverAdoptsANotebookBeingImported(t *testing.T) {
	db := pgtest.Org(t)
	data := t.TempDir()
	a, err := Open(data, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	ctx := t.Context()
	var once sync.Once
	var onA error
	gateway, _ := pgtest.Gateway(t, db)
	blobs, err := blob.OpenDir(data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	// B's import stores the bundle's empty root commit first, then asks for
	// the identity of the file its second commit adds.
	notes, err := notestore.Open(ctx, gateway, blobs, notestore.Options{ImportEntryID: func(path string, content []byte) string {
		once.Do(func() {
			done := make(chan error, 1)
			go func() { done <- a.Create(Bootstrap, "research", "Research") }()
			select {
			case onA = <-done:
			case <-time.After(30 * time.Second):
				t.Error("gateway A's creation waited for B's import")
			}
		})
		return workspace.ImportEntryID(path, content)
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(notes.Close)
	b, err := OpenWithNotes(t.TempDir(), nil, gateway, notes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	source := newImportSource(t, "# Imported by B\n")
	if err := b.Import(ctx, Bootstrap, "research", "Research", bytes.NewReader(source.bundle)); err != nil {
		t.Fatalf("gateway B's import: %v (gateway A's creation: %v)", err, onA)
	}
	if !errors.Is(onA, workspace.ErrConflict) {
		t.Fatalf("gateway A's creation of research while B imports it: %v", onA)
	}
	instanceIntact(t, db, data, b, map[string]string{"notes/proof.md": source.content})
}

// An import whose publication fails after another gateway listed its ID
// keeps the notebook that catalog entry names.
func TestFailedImportPublicationKeepsANotebookAnotherGatewayListed(t *testing.T) {
	db := pgtest.Org(t)
	data := t.TempDir()
	a, err := Open(data, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	b := sharedGateway(t, db, data)
	source := newImportSource(t, "# Imported by B\n")
	// Gateway A lists research, as a publication that outran B's
	// reservation would, just before B commits its catalog change.
	b.mu.Lock()
	b.checked = time.Now().Add(time.Hour)
	b.mu.Unlock()
	upload := &importCallbackReader{Reader: bytes.NewReader(source.bundle), beforeRead: func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.state.Projects["research"] = Project{ID: "research", Name: "Research", Members: map[string]string{Bootstrap: "owner"}}
		if err := a.persist(); err != nil {
			t.Fatal(err)
		}
	}}
	err = b.Import(t.Context(), Bootstrap, "research", "Research", upload)
	if err == nil {
		t.Fatal("gateway B published over a catalog another gateway changed")
	}
	instanceIntact(t, db, data, b, map[string]string{"notes/proof.md": source.content})
}
