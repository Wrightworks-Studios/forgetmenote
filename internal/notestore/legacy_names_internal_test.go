package notestore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/pg/pgtest"
)

// A notebook an earlier build stored with a name git fsck refuses, which
// that build's rule accepted, still opens, reads and exports. A revision can
// delete the file or move it elsewhere, and write other files, but not
// write it again.
func TestNotebookHoldingANameGitRefusesStaysReadableAndRepairable(t *testing.T) {
	ctx := context.Background()
	dir, err := blob.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	db := pgtest.Org(t)
	earlierBuild, err := Open(ctx, db, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer earlierBuild.Close()
	sig := gitobj.Signature{Name: "Ada", Email: "ada@example.com", When: 1700000000, TZ: "+0000"}
	if _, err := earlierBuild.Create(ctx, "notes", sig); err != nil {
		t.Fatal(err)
	}
	rule := writableName
	writableName = func(name string) bool {
		return name != "" && name != "." && name != ".." && !strings.EqualFold(name, ".git") && !strings.ContainsAny(name, "/\x00")
	}
	_, err = earlierBuild.Notebook("notes").Apply(ctx, ChangeSet{Subject: "Earlier build", Author: sig, Files: []FileChange{
		{Path: "git~1/a.md", Content: []byte("a")}, {Path: "git~1/b.md", Content: []byte("b")}, {Path: "keep.md", Content: []byte("k")}}})
	writableName = rule
	if err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, db, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n := s.Notebook("notes")
	head, err := n.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if content, _, err := n.Read(ctx, head.Commit, "git~1/a.md"); err != nil || string(content) != "a" {
		t.Fatalf("reading the file: %q %v", content, err)
	}
	if err := n.Objects(ctx, 0, func(string, gitobj.ID, []byte) error { return nil }); err != nil {
		t.Fatal("exporting the notebook:", err)
	}
	if _, err := n.Apply(ctx, ChangeSet{Subject: "Edit", Author: sig, Files: []FileChange{{Path: "git~1/a.md", Content: []byte("changed")}}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("writing the file again:", err)
	}
	if _, err := n.Apply(ctx, ChangeSet{Subject: "Other", Author: sig, Files: []FileChange{{Path: "other.md", Content: []byte("o")}}}); err != nil {
		t.Fatal("writing another file:", err)
	}
	repaired, err := n.Apply(ctx, ChangeSet{Subject: "Repair", Author: sig, Files: []FileChange{{Path: "fixed/a.md", From: "git~1/a.md"}, {Path: "git~1/b.md"}}})
	if err != nil {
		t.Fatal("moving the file away and deleting the other:", err)
	}
	files, err := n.Files(ctx, repaired.Head.Commit)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	if strings.Join(paths, " ") != "fixed/a.md keep.md other.md" {
		t.Fatalf("files after the repair: %v", paths)
	}
}
