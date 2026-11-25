package notestore_test

import (
	"errors"
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// Apply refuses a revision Git refuses: a NUL anywhere in its message or
// signature, or a file written under a name git fsck takes for .git. An
// import refuses a tree holding such a name.
func TestApplyAndImportRefuseWhatGitFsckRefuses(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	first := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "a")}})
	for name, cs := range map[string]notestore.ChangeSet{
		"subject":         {Subject: "Fix\x00typo", Files: []notestore.FileChange{write("b.md", "b")}},
		"body":            {Body: "Gitnotes-Source: a\x00b", Files: []notestore.FileChange{write("b.md", "b")}},
		"trailer key":     {Trailers: []gitobj.Trailer{{Key: "Na\x00me", Value: "v"}}, Files: []notestore.FileChange{write("b.md", "b")}},
		"trailer value":   {Trailers: []gitobj.Trailer{{Key: "Name", Value: "v\x00"}}, Files: []notestore.FileChange{write("b.md", "b")}},
		"author":          {Author: gitobj.Signature{Name: "Ada\x00Author", Email: "ada@example.com"}, Files: []notestore.FileChange{write("b.md", "b")}},
		"short name":      {Files: []notestore.FileChange{write("git~1/x.md", "x")}},
		"stream name":     {Files: []notestore.FileChange{write("notes/.git:x", "x")}},
		"ignorable":       {Files: []notestore.FileChange{write("\u200c.git/x.md", "x")}},
		"rename into one": {Files: []notestore.FileChange{{Path: "GIT~1 /a.md", From: "a.md"}}},
	} {
		if cs.Subject == "" {
			cs.Subject = "x"
		}
		if cs.Author.Name == "" {
			cs.Author = ada
		}
		if _, err := n.Apply(e.ctx, cs); !errors.Is(err, notestore.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if h, err := n.Head(e.ctx); err != nil || h != first.Head {
		t.Fatal("a refused change moved the head", h, err)
	}

	blob := []byte("x\n")
	inner := gitobj.EncodeTree([]gitobj.TreeEntry{{Name: "x.md", Mode: gitobj.ModeFile, ID: gitobj.BlobID(blob)}})
	root := gitobj.EncodeTree([]gitobj.TreeEntry{{Name: "git~1", Mode: gitobj.ModeTree, ID: gitobj.HashObject("tree", inner)}})
	rootID := gitobj.HashObject("tree", root)
	commit := gitobj.EncodeCommit(gitobj.Commit{Tree: rootID, Author: ada, Committer: ada, Message: "Outside\n"})
	trees := map[gitobj.ID][]byte{rootID: root, gitobj.HashObject("tree", inner): inner}
	imported := e.store.Notebook("imported")
	if _, err := imported.ImportCommit(e.ctx, commit, trees, func(gitobj.ID) ([]byte, error) { return blob, nil }); !errors.Is(err, notestore.ErrInvalid) {
		t.Fatal("an import of a tree Git refuses:", err)
	}
}
