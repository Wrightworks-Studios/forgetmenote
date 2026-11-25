package workspace

import (
	"context"
	"strings"
	"testing"

	"forgetmenote/internal/notestore"
)

func TestSnapshotUsesOperatorIndexBudget(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Save(t.Context(), "large.md", []byte(strings.Repeat("content ", 128)), "", "test"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FMN_LIMIT_INDEX_BYTES", "512")
	if _, err := s.Snapshot(t.Context(), ""); err == nil || !strings.Contains(err.Error(), "FMN_LIMIT_INDEX_BYTES") {
		t.Fatalf("snapshot budget not enforced: %v", err)
	}
	t.Setenv("FMN_LIMIT_INDEX_BYTES", "4096")
	if snapshot, err := s.Snapshot(t.Context(), ""); err != nil || len(snapshot.Files) != 1 {
		t.Fatalf("larger budget did not restore coverage: %v", err)
	}
}

func TestCommittedSnapshotAndGitChanges(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a, _ := s.Save(ctx, "a.md", []byte("# Original\n"), "", "test")
	b, _ := s.Save(ctx, "b.md", []byte("# Second\n"), "", "test")
	// The rename a move commits.
	moved := commitFixture(t, s, notestore.FileChange{Path: "renamed.md", From: "a.md"})
	if _, err := s.Save(ctx, b.Path, []byte("LATER"), b.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot(ctx, moved)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Files) != 2 || snap.Files[0].Content != "# Second\n" || snap.Files[0].Hash == "" {
		t.Fatalf("not a committed snapshot: %+v", snap)
	}
	changes, err := s.ChangesSince(ctx, a.Revision, moved)
	if err != nil {
		t.Fatal(err)
	}
	if changes.FreshReview || len(changes.Files) != 2 {
		t.Fatalf("changes %+v", changes)
	}
	renamed := false
	for _, c := range changes.Files {
		if c.Kind == "renamed" && c.From == "a.md" && c.Path == "renamed.md" {
			renamed = true
		}
	}
	if !renamed {
		t.Fatalf("rename missing %+v", changes.Files)
	}
	old, err := s.Snapshot(ctx, a.Revision)
	if err != nil || len(old.Files) != 1 || old.Files[0].Path != "a.md" {
		t.Fatal(old, err)
	}
	lost, err := s.ChangesSince(ctx, "1111111111111111111111111111111111111111", "")
	if err != nil || !lost.FreshReview {
		t.Fatal("missing checkpoint not explicit", lost, err)
	}
}
func TestSnapshotExclusionsAndDeletedChanges(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.browseExcluded = []string{"private"}
	s.Save(ctx, "private/secret.md", []byte("secret"), "", "test")
	a, _ := s.Save(ctx, "public.md", []byte("public"), "", "test")
	snap, err := s.Snapshot(ctx, "")
	if err != nil || len(snap.Files) != 1 {
		t.Fatal(snap, err)
	}
	commitFixture(t, s, notestore.FileChange{Path: "public.md"})
	c, err := s.ChangesSince(ctx, a.Revision, "")
	if err != nil || len(c.Files) != 1 || c.Files[0].Kind != "deleted" {
		t.Fatal(c, err)
	}
}

func TestChangesIncludeMediaWithoutReadingBinaryInputs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	before, _ := s.Status(ctx)
	if _, err := s.Save(ctx, "assets/map.png", []byte{0, 1, 2, 3}, "", ""); err != nil {
		t.Fatal(err)
	}
	changes, err := s.ChangesSince(ctx, before.Revision, "")
	if err != nil || len(changes.Files) != 1 || changes.Files[0].Path != "assets/map.png" {
		t.Fatal("media change missing", changes, err)
	}
	snapshot, err := s.Snapshot(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range snapshot.Files {
		if f.Path == "assets/map.png" {
			t.Fatal("binary became embedding input")
		}
	}
}
