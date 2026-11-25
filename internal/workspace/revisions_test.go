package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestUnrelatedChangesDoNotConflictWithNoteSave(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a, err := s.Save(ctx, "notes/a.md", []byte("A"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(ctx, "notes/b.md", []byte("B"), "", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(ctx, a.Path, []byte("A revised"), a.Revision, "test"); err != nil {
		t.Fatalf("unrelated note caused conflict: %v", err)
	}
	if _, err = s.Save(ctx, a.Path, []byte("stale"), a.Revision, "test"); !errors.Is(err, ErrConflict) {
		t.Fatalf("same note lost update: %v", err)
	}
}

func TestConcurrentDifferentNotesBothSave(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, path := range []string{"a.md", "b.md"} {
		if _, err := s.Save(ctx, path, []byte("old"), "", "test"); err != nil {
			t.Fatal(err)
		}
	}
	state, _ := s.Status(ctx)
	results := make(chan error, 2)
	for _, path := range []string{"a.md", "b.md"} {
		go func() { _, err := s.Save(ctx, path, []byte("new"), state.Revision, "writer"); results <- err }()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal("independent save blocked", err)
		}
	}
}
func TestChangedThenRevertedNoteStillConflicts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a, err := s.Save(ctx, "a.md", []byte("original"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Save(ctx, a.Path, []byte("revised"), a.Revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(ctx, a.Path, a.Content, b.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(ctx, a.Path, []byte("stale"), a.Revision, "test"); !errors.Is(err, ErrConflict) {
		t.Fatal("intervening edits were ignored", err)
	}
	if _, err = s.Save(ctx, a.Path, []byte("forged"), strings.Repeat("a", 40), "test"); !errors.Is(err, ErrConflict) {
		t.Fatal("invalid revision accepted", err)
	}
}

// Reads at one commit may be reused, but each caller gets bytes of its own and
// the byte budget configured now, not the one in force at the first read.
func TestRepeatedRevisionReadsKeepPrivateContentAndCurrentBudget(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	saved, err := s.Save(ctx, "note.md", []byte("# Original\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		doc, err := s.Read(ctx, "note.md", saved.Revision)
		if err != nil || string(doc.Content) != "# Original\n" || doc.Version != saved.Version {
			t.Fatalf("reread at the same commit: %q %s %v", doc.Content, doc.Version, err)
		}
		doc.Content[2] = 'X'
	}
	t.Setenv("FMN_LIMIT_FILE_BYTES", "4")
	if _, err = s.Read(ctx, "note.md", saved.Revision); err == nil {
		t.Fatal("a reread ignored the lowered file budget")
	}
}
