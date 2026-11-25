package workspace

import (
	"context"
	"errors"
	"testing"
)

func TestCommentsAreVersionedWithoutChangingNote(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	note, err := s.Save(ctx, "notes/a.md", []byte("Human text"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Comments(ctx, note.Path)
	if err != nil {
		t.Fatal(err)
	}
	d, err = s.AddComment(ctx, note.Path, "Please clarify", d.Revision, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Comments) != 1 || d.Comments[0].Author != "reviewer" {
		t.Fatal("comment attribution lost")
	}
	got, _ := s.Read(ctx, note.Path, "")
	if string(got.Content) != "Human text" {
		t.Fatal("comment changed note")
	}
	if _, err = s.AddComment(ctx, note.Path, "Stale", note.Revision, "reviewer"); !errors.Is(err, ErrConflict) {
		t.Fatal("stale discussion accepted")
	}
	if _, err = s.ResolveComment(ctx, note.Path, d.Comments[0].ID, d.Revision, "other", false); !errors.Is(err, ErrForbidden) {
		t.Fatal("other commenter resolved discussion")
	}
	d, err = s.ResolveComment(ctx, note.Path, d.Comments[0].ID, d.Revision, "reviewer", false)
	if err != nil || !d.Comments[0].Resolved {
		t.Fatalf("resolve: %v", err)
	}
}

func TestCommentsStayWithTheirNote(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, path := range []string{"notes/a.md", "archive/a.md"} {
		if _, err := s.Save(ctx, path, []byte("Same title"), "", "writer"); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.Comments(ctx, "notes/a.md")
	if err != nil {
		t.Fatal(err)
	}
	first, err = s.AddComment(ctx, "notes/a.md", "Only for this note", first.Revision, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.Comments(ctx, "archive/a.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(other.Comments) != 0 {
		t.Fatal("comments leaked between notes with the same basename")
	}
	if _, err = s.ResolveComment(ctx, "archive/a.md", first.Comments[0].ID, other.Revision, "reviewer", true); err == nil {
		t.Fatal("resolved another note's comment")
	}
	again, err := s.Comments(ctx, "notes/a.md")
	if err != nil || len(again.Comments) != 1 || again.Comments[0].Resolved {
		t.Fatalf("original discussion changed: %+v %v", again, err)
	}
}

// A comment the owner writes rechecks its target's classification then,
// not when the request was prepared: here another editor archives the note
// after the scope was chosen.
func TestMutationScopeRechecksCommentTargetAtOwner(t *testing.T) {
	owner := newTestStoreExcluding(t, []string{"private"})
	ctx := context.Background()
	note, err := owner.Save(ctx, "eligible.md", []byte("# Eligible\n"), "", "author")
	if err != nil {
		t.Fatal(err)
	}
	scope := ContentScope{Policy: ContainerPolicy{Version: 1, Archives: "exclude", Drafts: "exclude"}, Collection: "all"}
	commitFixture(t, owner, fixture(note.Path, "---\nfmn-container: archive\n---\n# Eligible\n"))
	scoped := WithMutationScope(ctx, MutationScope{Version: 1, Content: scope, Target: note.Path})
	discussion, err := owner.Comments(ctx, note.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.AddComment(scoped, note.Path, "must not be written", discussion.Revision, "agent"); err == nil {
		t.Error("owner accepted comment on target excluded after preflight")
	}
	d, _ := owner.Comments(ctx, note.Path)
	if len(d.Comments) != 0 {
		t.Error("rejected action changed discussion")
	}
	if d.Revision != discussion.Revision {
		t.Error("rejected action committed")
	}
}
