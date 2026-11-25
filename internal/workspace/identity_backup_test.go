package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNotebookImportRejectsDuplicateIdentities(t *testing.T) {
	s := backupTestStore(t)
	ctx := context.Background()
	content, _ := WithNoteID([]byte("# Imported\n"), strings.Repeat("a", 32))
	if _, err := s.Save(ctx, "one.md", content, "", "author"); err != nil {
		t.Fatal(err)
	}
	// An untrusted bundle can originate outside ForgetMeNote's write boundary.
	commitFixture(t, s, fixture("two.md", string(content)))
	notes := newTestNotes(t)
	if err := importFile(t, notes, "restored", backupFile(t, s)); !errors.Is(err, ErrBundle) || !strings.Contains(err.Error(), "duplicate fmn-id") {
		t.Fatal("import accepted ambiguous note identities:", err)
	}
	absent(t, notes, "restored")
}

// A malformed fmn-id refuses the bundle; legacy front matter that only
// mentions fmn-id is a note without one.
func TestNotebookImportRejectsMalformedIdentities(t *testing.T) {
	notes := newTestNotes(t)
	malformed := gitBundle(t, map[string]string{"notes/bad.md": "---\nfmn-id: not-an-identity\n---\n# Bad\n"})
	if err := importFile(t, notes, "restored", malformed); !errors.Is(err, ErrBundle) || !strings.Contains(err.Error(), "invalid fmn-id in notes/bad.md") {
		t.Fatal("import accepted a malformed note identity:", err)
	}
	absent(t, notes, "restored")
	legacy := gitBundle(t, map[string]string{"legacy.md": "---\nunclosed legacy metadata\nA prose mention of fmn-id: remains editable"})
	if err := importFile(t, notes, "restored", legacy); err != nil {
		t.Fatal("import refused legacy front matter:", err)
	}
}
