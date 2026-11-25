package workspace

import (
	"context"
	"testing"
)

// Note names may contain pathspec wildcards or start with pathspec magic. A
// change set must attribute only commits that touched the named files.
func TestChangesSinceAttributesNotesNamedLikePathspecsExactly(t *testing.T) {
	for _, name := range []string{"a*.md", ":(exclude)a.md", ":(glob)a?.md"} {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			note, err := s.Save(ctx, name, []byte("# Named\n"), "", "author")
			if err != nil {
				t.Fatal(err)
			}
			other, err := s.Save(ctx, "ab.md", []byte("# Other\n"), "", "author")
			if err != nil {
				t.Fatal(err)
			}
			from := other.Revision
			// Unrelated commits whose net change is empty, so ab.md is not in the change set.
			edited, err := s.Save(ctx, other.Path, []byte("# Other, edited\n"), other.Revision, "author")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Save(ctx, other.Path, []byte("# Other\n"), edited.Revision, "author"); err != nil {
				t.Fatal(err)
			}
			status, err := s.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			changed, err := s.Save(ctx, note.Path, []byte("# Named, edited\n"), status.Revision, "author")
			if err != nil {
				t.Fatal(err)
			}
			changes, err := s.ChangesSince(ctx, from, changed.Revision)
			if err != nil {
				t.Fatal(err)
			}
			if len(changes.Files) != 1 || changes.Files[0].Path != name || changes.Files[0].Kind != "modified" {
				t.Fatalf("change set files = %+v", changes.Files)
			}
			if len(changes.Commits) != 1 || changes.Commits[0].Revision != changed.Revision {
				t.Fatalf("change set attributed commits that did not touch %s: %+v", name, changes.Commits)
			}
		})
	}
}
