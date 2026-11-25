package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

func TestNotebookImportRejectsFilesOutsideTheNotebookAllowList(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"git attributes":           {".gitattributes": "*.md filter=notebook diff=notebook merge=notebook\n"},
		"nested git attributes":    {"notes/.gitattributes": "* export-ignore\n"},
		"hidden note directory":    {".github/workflows/notes.md": "# Hidden\n"},
		"hidden asset":             {"assets/.hidden.png": "png"},
		"unsupported top-level":    {"config.yaml": "unsafe: true\n"},
		"misnamed discussion file": {"comments/note.json": "[]"},
	} {
		t.Run(name, func(t *testing.T) {
			files["notes/kept.md"] = "# Kept\n"
			notes := newTestNotes(t)
			if err := importFile(t, notes, "restored", gitBundle(t, files)); !errors.Is(err, ErrBundle) {
				t.Fatalf("import accepted a file outside the notebook allow-list: %v", err)
			}
			// Nothing of the history is stored before the bundle is checked.
			absent(t, notes, "restored")
		})
	}
}

// A notebook export with every kind of notebook file imports, each file kept.
func TestNotebookImportAcceptsEveryNotebookFileKind(t *testing.T) {
	s := backupTestStore(t)
	ctx := context.Background()
	doc, err := s.Save(ctx, "notes/kept.md", []byte("# Kept\n"), "", "author")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(ctx, "legacy.md", []byte("---\nunclosed legacy metadata\nA prose mention of fmn-id: remains editable"), "", "author"); err != nil {
		t.Fatal(err)
	}
	gone, err := s.Save(ctx, "notes/gone.md", []byte("# Gone\n"), "", "author")
	if err != nil {
		t.Fatal(err)
	}
	// The lock policy, a discussion and the Trash as their features write
	// them, with the instructions and media an ordinary export holds.
	discussion, _ := json.MarshalIndent([]Comment{{ID: "c1", Author: "author", Text: "Reviewed", Created: "2026-11-22T10:00:00Z", Anchor: &CommentAnchor{Quote: "Kept", Revision: doc.Revision}}}, "", "  ")
	trashed := TrashRecord{TrashEntry: TrashEntry{ID: strings.Repeat("c", 32), Path: "Trash/" + strings.Repeat("c", 32) + ".md", OriginalPath: gone.Path, Title: "Gone", DeletedAt: "2026-11-22T10:00:00Z", DeletedBy: "author"}, History: map[string][]string{gone.Path: {gone.Revision}}}
	index := map[string][]byte{}
	if err = stageTrashIndex(TrashIndex{Version: 1, Records: []TrashRecord{trashed}}, 0, index); err != nil {
		t.Fatal(err)
	}
	commitFixture(t, s,
		fixture(locksPath, `{"version":1,"paths":["manuscript"]}`+"\n"),
		fixture(commentPath(doc.Path), string(discussion)),
		notestore.FileChange{Path: gone.Path},
		fixture(trashed.Path, "# Gone\n"),
		fixture(trashIndexPath, string(index[trashIndexPath])),
		fixture("AGENTS.md", "# Instructions\n"),
		fixture("assets/diagram.png", "png"),
	)
	restored := restore(t, s)
	for _, path := range []string{locksPath, trashIndexPath, commentPath(doc.Path), trashed.Path, "AGENTS.md", "assets/diagram.png", "notes/kept.md", "legacy.md"} {
		want, _, err := s.notes.Read(ctx, gitobj.ID{}, path)
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := restored.notes.Read(ctx, gitobj.ID{}, path)
		if err != nil || string(got) != string(want) {
			t.Fatalf("import changed or omitted %s: %q %v", path, got, err)
		}
	}
}
