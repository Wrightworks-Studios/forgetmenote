package workspace

import (
	"context"
	"errors"
	"testing"
)

var excludingScope = ContentScope{Policy: ContainerPolicy{Version: 1, Archives: "exclude", Drafts: "exclude"}, Collection: "all"}

// A scoped save checks its committed target, sources and media when it
// writes, not at the caller's preflight.
func TestMutationScopeRechecksSavesWhenTheyWrite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	note, err := s.Save(ctx, "eligible.md", []byte("# Eligible\n"), "", "author")
	if err != nil {
		t.Fatal(err)
	}
	scoped := WithMutationScope(ctx, MutationScope{Version: 1, Content: excludingScope, Target: note.Path})
	edited, err := s.Save(scoped, note.Path, []byte("# Eligible, edited\n"), note.Revision, "agent")
	if err != nil {
		t.Fatal("an eligible target:", err)
	}
	// Another editor archives the note after the caller's preflight.
	archived := commitFixture(t, s, fixture(note.Path, "---\nfmn-container: archive\n---\n# Eligible, edited\n"))
	if _, err = s.Save(scoped, note.Path, []byte("# Promotion\n"), archived, "agent"); err == nil {
		t.Error("a generic write promoted an excluded target")
	}
	other, err := s.Save(ctx, "other.md", []byte("# Other\n"), "", "author")
	if err != nil {
		t.Fatal(err)
	}
	unscopedTarget := WithMutationScope(ctx, MutationScope{Version: 1, Content: excludingScope})
	if _, err = s.Save(unscopedTarget, other.Path, []byte("---\nfmn-container: draft\n---\n# Other\n"), other.Revision, "agent"); err == nil {
		t.Error("a scoped save moved a note out of its scope")
	}
	source := MutationSource{Path: other.Path, Revision: other.Revision, Hash: ContentHash(other.Content)}
	if _, err = s.Save(WithMutationScope(ctx, MutationScope{Version: 1, Content: excludingScope, Sources: []MutationSource{source}}), "derived.md", []byte("# Derived\n"), "", "agent"); err != nil {
		t.Fatal("an unchanged source:", err)
	}
	source.Hash = ContentHash([]byte("what the caller read earlier"))
	if _, err = s.Save(WithMutationScope(ctx, MutationScope{Version: 1, Content: excludingScope, Sources: []MutationSource{source}}), "derived-again.md", []byte("# Derived\n"), "", "agent"); !errors.Is(err, ErrConflict) {
		t.Error("a changed source:", err)
	}
	head := commitFixture(t, s, fixture("assets/shared.png", "png"), fixture("hidden.md", "---\nfmn-container: archive\n---\n![figure](assets/shared.png)\n"))
	if _, err = s.Save(WithMutationScope(ctx, MutationScope{Version: 1, Content: excludingScope, Control: "upload_attachment"}), "assets/shared.png", []byte("replaced"), head, "agent"); !errors.Is(err, ErrForbidden) {
		t.Error("replaced media an excluded note uses:", err)
	}
	if doc, _ := s.Read(ctx, note.Path, ""); doc.Revision != head || string(doc.Content) == "# Promotion\n" || string(edited.Content) != "# Eligible, edited\n" {
		t.Error("a refused save changed the notebook")
	}
}

// A scoped lock rule checks every note it would cover, excluded ones too,
// so one excluded descendant refuses the whole rule.
func TestMutationScopeChecksEveryNoteALockCovers(t *testing.T) {
	s := newTestStore(t)
	commitFixture(t, s, fixture("folder/a.md", "# A\n"), fixture("folder/b.md", "---\nfmn-container: archive\n---\n# B\n"), fixture("open/c.md", "# C\n"))
	backend := s.backend
	check := func(ctx context.Context, path string) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		return backend.checkMutationLockTarget(ctx, path)
	}
	ctx := context.Background()
	lock := WithMutationScope(ctx, MutationScope{Version: 1, Content: excludingScope, Control: "set_note_lock"})
	if err := check(lock, "open"); err != nil {
		t.Error("an eligible folder:", err)
	}
	for _, path := range []string{"folder", "missing"} {
		if err := check(lock, path); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: %v", path, err)
		}
	}
	if err := check(WithMutationScope(ctx, MutationScope{Version: 1, Content: excludingScope}), "open"); !errors.Is(err, ErrForbidden) {
		t.Error("a scope for another control:", err)
	}
	if err := check(ctx, "folder"); err != nil {
		t.Error("an unscoped lock:", err)
	}
	s.SetReadOnly(true)
	if err := check(lock, "open"); !errors.Is(err, ErrReadOnly) {
		t.Error("a read-only notebook:", err)
	}
}
