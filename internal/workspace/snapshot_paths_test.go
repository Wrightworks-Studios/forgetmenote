package workspace

import (
	"strings"
	"testing"
)

// Handoff validation admits the notebook file allow-list and rejects each other
// kind of path with its own message. Assets skip note content checks.
func TestNotebookSnapshotPathRules(t *testing.T) {
	revision := strings.Repeat("a", 40)
	discussion := "comments/" + strings.Repeat("0f", 32)
	for p, want := range map[string]string{
		"comments/short.json":      "unsupported notebook file: comments/short.json",
		discussion + ".md":         "unsupported notebook file: " + discussion + ".md",
		"assets/.hidden/image.png": ErrForbidden.Error(),
		"assets/.note.md":          ErrForbidden.Error(),
		".hidden/note.md":          "unsupported hidden notebook file: .hidden/note.md",
		"notes/.draft.md":          "unsupported hidden notebook file: notes/.draft.md",
		".gitattributes":           "unsupported notebook file: .gitattributes",
		"script.sh":                "unsupported notebook file: script.sh",
	} {
		err := ValidateNotebookSnapshot(t.Context(), NotebookSnapshot{Revision: revision, Files: map[string][]byte{p: []byte("x")}})
		if err == nil || err.Error() != want {
			t.Errorf("%s: got %v, want %q", p, err, want)
		}
	}
	files := map[string][]byte{
		discussion + ".json": []byte("[]"),
		"assets/image.png":   {0xff, 0},
		"assets/raw.md":      {0xff, 0},
		"notes/plan.md":      []byte("# Plan\n"),
		locksPath:            []byte(`{"version":1,"paths":[]}`),
		trashIndexPath:       []byte(`{"version":1,"records":[]}`),
	}
	if err := ValidateNotebookSnapshot(t.Context(), NotebookSnapshot{Revision: revision, Files: files}); err != nil {
		t.Fatal(err)
	}
}
