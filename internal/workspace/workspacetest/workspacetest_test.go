package workspacetest_test

import (
	"path/filepath"
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace/workspacetest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestReopeningADirectoryFindsItsNotebook(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	store, nb := workspacetest.New(t, dir, nil)
	if nb.ID() != "journal" || store.Root != filepath.Join(dir, "workspace") {
		t.Fatal("notebook", nb.ID(), store.Root)
	}
	saved, err := store.Save(t.Context(), "note.md", []byte("# Kept\n"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	again, nb := workspacetest.New(t, dir, nil)
	doc, err := again.Read(t.Context(), "note.md", "")
	if err != nil || string(doc.Content) != "# Kept\n" || doc.Revision != saved.Revision {
		t.Fatal("after reopening:", doc, err)
	}
	if content, _, err := nb.Read(t.Context(), gitobj.ID{}, "note.md"); err != nil || string(content) != "# Kept\n" {
		t.Fatal("the notes store's notebook:", string(content), err)
	}
	if other, _ := workspacetest.New(t, t.TempDir(), nil); other.Root == again.Root {
		t.Fatal("two directories share a notebook")
	}
}
