// Package workspacetest opens notebooks kept in the notes store for other
// packages' tests. A package that uses it runs pgtest.Main from its
// TestMain.
package workspacetest

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
)

// New opens the notebook whose state directory is dir, creating it on first
// use, in a notes store of its own over dir's test database and blob
// directory, as a process starting on that data would. Opening the same dir
// again in one test binary finds what the earlier store wrote. The notebook
// ID is dir's base name, or "notes" when that is not a valid ID. It returns
// the store and its notebook in the notes store, for fixtures and checks
// that bypass the workspace.
func New(t testing.TB, dir string, excluded []string) (*workspace.Store, *notestore.Notebook) {
	t.Helper()
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0700); err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.OpenDir(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	notes, err := notestore.Open(ctx, pgtest.Dir(t, dir), blobs, notestore.Options{ImportEntryID: workspace.NoteEntryID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(notes.Close)
	id := filepath.Base(dir)
	if !workspace.ValidNotebookID(id) {
		id = "notes"
	}
	store, err := workspace.OpenDB(ctx, notes, id, dir, excluded)
	if errors.Is(err, fs.ErrNotExist) {
		store, err = workspace.CreateDB(ctx, notes, id, dir, excluded)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, notes.Notebook(id)
}
