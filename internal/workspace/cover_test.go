package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCoverMovesCopiesLocksAndRevisions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for path, content := range map[string]string{"assets/set/image.png": "image", "assets/set/note.md": "---\ncover: assets/set/image.png\ncustom: keep\n---\n# Note\nExact body.", "outside.md": "---\ncover: assets/set/image.png\n---\nOutside."} {
		if _, err := s.Save(ctx, path, []byte(content), "", "test"); err != nil {
			t.Fatal(err)
		}
	}
	state, _ := s.Status(ctx)
	copied, err := s.Duplicate(ctx, "assets/set", "assets/copy", state.Revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := s.Read(ctx, "assets/copy/note.md", "")
	if !strings.Contains(string(doc.Content), "assets/copy/image.png") {
		t.Fatalf("copy cover did not follow image: %s", doc.Content)
	}
	setLocks(t, s, "outside.md")
	state, _ = s.Status(ctx)
	if _, err = s.Move(ctx, "assets/set", "assets/moved", state.Revision, "test"); !errors.Is(err, ErrForbidden) {
		t.Fatal("move bypassed cover lock", err)
	}
	setLocks(t, s)
	state, _ = s.Status(ctx)
	moved, err := s.Move(ctx, "assets/set", "assets/moved", state.Revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	doc, _ = s.Read(ctx, "outside.md", "")
	if !strings.Contains(string(doc.Content), "assets/moved/image.png") {
		t.Fatalf("incoming cover: %s", doc.Content)
	}
	cover := "assets/copy/image.png"
	if _, err = s.SetMetadata(ctx, doc.Path, Metadata{Cover: &cover}, state.Revision, "test"); !errors.Is(err, ErrConflict) {
		t.Fatal("stale cover accepted", err)
	}
	locked := setLocks(t, s, doc.Path)
	if _, err = s.SetMetadata(ctx, doc.Path, Metadata{Cover: &cover}, locked, "editor"); !errors.Is(err, ErrForbidden) {
		t.Fatal("cover bypassed note lock", err)
	}
	old, err := s.Read(ctx, "assets/set/note.md", copied.Revision)
	if err != nil || !strings.Contains(string(old.Content), "assets/set/image.png") {
		t.Fatal("history changed", err)
	}
	doc, _ = s.Read(ctx, "assets/moved/note.md", moved.Revision)
	if !strings.Contains(string(doc.Content), "custom: keep") || !strings.HasSuffix(string(doc.Content), "# Note\nExact body.") {
		t.Fatal("body changed")
	}
}
