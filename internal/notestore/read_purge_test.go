package notestore_test

import (
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// HasUnder tells a folder, any file under it, at the head and at an older
// revision: neither a file nor a name that only starts the same is one.
func TestHasUnderTellsFoldersAtEveryRevision(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	old := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a/b.md", "b"), write("ab/c.md", "c"), write("d.md", "d")}}).Head.Commit
	apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{{Path: "a/b.md"}}})
	for _, c := range []struct {
		rev  gitobj.ID
		dir  string
		want bool
	}{
		{zero, "a", false}, {zero, "ab", true}, {zero, "ab/", true}, {zero, "d.md", false}, {zero, "a/b", false},
		{old, "a", true}, {old, "a/", true}, {old, "ab", true}, {old, "d.md", false}, {old, "x", false},
	} {
		if got, err := n.HasUnder(e.ctx, c.rev, c.dir); err != nil || got != c.want {
			t.Errorf("HasUnder(%s, %q) = %v %v, want %v", c.rev, c.dir, got, err, c.want)
		}
	}
}

// RewrittenAll maps every commit purges rewrote to the one standing for it
// now, through successive purges, as Rewritten does one by one; each purge
// starts a new incarnation of the history.
func TestRewrittenAllFollowsSuccessivePurges(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	n := e.create("notes")
	const secret, again = "sk-live-first", "sk-live-second"
	var commits []gitobj.ID
	for _, content := range []string{"plain", "key " + secret + " and " + again, "more", "last"} {
		commits = append(commits, apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", content)}}).Head.Commit)
	}
	if all, err := n.RewrittenAll(e.ctx); err != nil || len(all) != 0 {
		t.Fatal("before any purge:", all, err)
	}
	before, err := n.Incarnation(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{secret, again} {
		if _, err := n.Rewrite(e.ctx, 3, []string{"purge-1", "purge-2"}[i], redact(text), purgeOptions()); err != nil {
			t.Fatal(err)
		}
		inc, err := n.Incarnation(e.ctx)
		if err != nil || inc <= before {
			t.Fatal("the incarnation after a purge:", inc, before, err)
		}
		before = inc
	}
	all, err := n.RewrittenAll(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Revision 3, the first purged, is the second commit after the
	// notebook's first.
	for _, c := range commits[:1] {
		if _, ok := all[c]; ok {
			t.Fatal("a commit no purge rewrote maps:", c)
		}
	}
	for _, c := range commits[1:] {
		rec, ok, err := n.Rewritten(e.ctx, c)
		if err != nil || !ok || all[c] != rec.Commit || len(rec.Purges) != 2 {
			t.Fatalf("%s maps to %s, Rewritten %+v %v %v", c, all[c], rec, ok, err)
		}
		if _, err := n.Commit(e.ctx, all[c]); err != nil {
			t.Fatal("the commit standing for", c, err)
		}
	}
}
