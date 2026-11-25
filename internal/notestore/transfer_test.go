package notestore_test

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

var fmnID = regexp.MustCompile(`(?m)\A---\n(?:.*\n)*?fmn-id: ([0-9a-f]{32})\n`)

func declaredID(path string, content []byte) string {
	if m := fmnID.FindSubmatch(content); m != nil {
		return string(m[1])
	}
	return ""
}

// buildNotebookHistory commits a notebook's history with Git: notes with
// fmn-ids, renames, a large manuscript, a side branch merged back, a
// submodule entry, a symlink and an executable.
func buildNotebookHistory(t *testing.T) *gitRepo {
	g := newGitRepo(t, false)
	sig := ada
	step := func(subject string) {
		sig.When += 60
		g.commit(sig, subject, []gitobj.Trailer{{Key: "Gitnotes-Source", Value: "mcp"}})
	}
	book := manuscript(400)
	g.write("Inbox/idea.md", "---\nfmn-id: 0123456789abcdef0123456789abcdef\ntitle: Idea\n---\n\nAn idea.\n", 0o644)
	g.write("AGENTS.md", "Instructions.\n", 0o644)
	g.write("scan.bin", strings.Repeat("\x00\x01binary", 3000), 0o644)
	step("Initial notes")
	g.write("Inbox/idea.md", "---\nfmn-id: 0123456789abcdef0123456789abcdef\ntitle: Idea\n---\n\nAn idea, grown.\n", 0o644)
	g.write("book.md", book, 0o644)
	step("Grow the idea and start the book")
	os.Remove(filepath.Join(g.dir, "Inbox/idea.md"))
	g.write("Projects/idea-renamed.md", "---\nfmn-id: 0123456789abcdef0123456789abcdef\ntitle: Idea\n---\n\nAn idea, grown and moved.\n", 0o644)
	os.MkdirAll(filepath.Join(g.dir, "assets"), 0o755)
	os.Rename(filepath.Join(g.dir, "scan.bin"), filepath.Join(g.dir, "assets/scan.bin"))
	step("Move the idea and the scan")
	g.run(nil, "checkout", "-q", "-b", "side")
	g.write("book.md", strings.Replace(book, "Paragraph 200.", "Paragraph 200, edited on a side branch.", 1), 0o644)
	g.write("side.md", "side\n", 0o644)
	step("Edit on a side branch")
	g.run(nil, "checkout", "-q", "main")
	g.write("AGENTS.md", "Instructions, revised.\n", 0o644)
	g.write("tools/run.sh", "#!/bin/sh\n", 0o755)
	os.Symlink("AGENTS.md", filepath.Join(g.dir, "link"))
	step("Revise instructions")
	sig.When += 60
	g.runEnv([]string{"GIT_AUTHOR_NAME=Ada Author", "GIT_AUTHOR_EMAIL=ada@example.com", "GIT_AUTHOR_DATE=1700009999 +0000",
		"GIT_COMMITTER_NAME=Ada Author", "GIT_COMMITTER_EMAIL=ada@example.com", "GIT_COMMITTER_DATE=1700009999 +0000"}, nil, "merge", "-q", "--no-edit", "side")
	g.run(nil, "update-index", "--add", "--cacheinfo", "160000,0123456789012345678901234567890123456789,vendor/module")
	g.run(nil, "rm", "-q", "side.md")
	sig.When += 60
	g.runEnv([]string{"GIT_AUTHOR_NAME=Bob", "GIT_AUTHOR_EMAIL=bob@example.com", "GIT_AUTHOR_DATE=1700019999 +0000",
		"GIT_COMMITTER_NAME=ForgetMeNote", "GIT_COMMITTER_EMAIL=forgetmenote@localhost", "GIT_COMMITTER_DATE=1700019999 +0000"}, nil, "commit", "-q", "-m", "Pin a module, drop side")
	if !strings.Contains(g.run(nil, "ls-tree", "HEAD", "vendor/"), "160000 commit") {
		t.Fatal("the fixture has no submodule entry")
	}
	return g
}

// importAll imports g's history into n in topological order, as the
// importer streams it.
func importAll(t *testing.T, g *gitRepo, n *notestore.Notebook) notestore.Head {
	t.Helper()
	var head notestore.Head
	for _, raw := range strings.Fields(g.run(nil, "rev-list", "--reverse", "--topo-order", "HEAD")) {
		commit := []byte(g.run(nil, "cat-file", "commit", raw))
		c, _ := gitobj.ParseCommit(commit)
		trees := map[gitobj.ID][]byte{c.Tree: []byte(g.run(nil, "cat-file", "tree", c.Tree.String()))}
		for _, line := range strings.Split(strings.TrimSpace(g.run(nil, "ls-tree", "-r", "-t", raw)), "\n") {
			if fields := strings.Fields(line); len(fields) >= 3 && fields[1] == "tree" {
				id := g.id(fields[2])
				trees[id] = []byte(g.run(nil, "cat-file", "tree", id.String()))
			}
		}
		var err error
		head, err = n.ImportCommit(t.Context(), commit, trees, func(id gitobj.ID) ([]byte, error) {
			return []byte(g.run(nil, "cat-file", "blob", id.String())), nil
		})
		if err != nil {
			t.Fatalf("import %s: %v", raw, err)
		}
	}
	return head
}

func TestImportKeepsGitHistoryAndExportsItBack(t *testing.T) {
	e := newEnv(t, notestore.Options{ImportEntryID: declaredID})
	g := buildNotebookHistory(t)
	n := e.store.Notebook("imported")
	head := importAll(t, g, n)
	gitHead := g.id(g.run(nil, "rev-parse", "HEAD"))
	commits := strings.Fields(g.run(nil, "rev-list", "HEAD"))
	if head.Commit != gitHead || head.Seq != int64(len(commits)) {
		t.Fatalf("head %+v, git %s with %d commits", head, gitHead, len(commits))
	}
	again, err := n.ImportCommit(e.ctx, []byte(g.run(nil, "cat-file", "commit", "HEAD~1")), nil, nil)
	if err != nil || again != head {
		t.Fatal("importing a commit again:", again, err)
	}
	// Every file reads as Git has it, from a store with cold caches too.
	for _, store := range []*notestore.Notebook{n, e.open(e.db, notestore.Options{}).Notebook("imported")} {
		files, err := store.Files(e.ctx, gitobj.ID{})
		if err != nil {
			t.Fatal(err)
		}
		listing := strings.Fields(g.run(nil, "ls-tree", "-r", "--name-only", "HEAD"))
		if !slices.Equal(paths(files), listing) {
			t.Fatalf("files %v, git %v", paths(files), listing)
		}
		for _, f := range files {
			if f.Mode == gitobj.ModeGitlink {
				continue
			}
			if got, want := read(t, store, gitobj.ID{}, f.Path), g.run(nil, "cat-file", "blob", "HEAD:"+f.Path); got != want {
				t.Fatalf("%s differs from Git", f.Path)
			}
		}
	}
	idea, err := n.Stat(e.ctx, gitobj.ID{}, "Projects/idea-renamed.md")
	if err != nil || idea.EntryID != "0123456789abcdef0123456789abcdef" {
		t.Fatal("note identity:", idea, err)
	}
	if h, _ := n.History(e.ctx, notestore.HistoryQuery{Path: "Projects/idea-renamed.md"}); len(h) != 3 || h[2].Seq != 1 {
		t.Fatal("the moved note's history:", len(h))
	}
	scan, _ := n.Stat(e.ctx, gitobj.ID{}, "assets/scan.bin")
	if h, _ := n.History(e.ctx, notestore.HistoryQuery{EntryID: scan.EntryID}); len(h) != 2 || h[0].Changes[1].Op != "rename" {
		t.Fatal("an identical file that moved keeps its entry:", h)
	}
	if want := g.id(g.run(nil, "log", "-1", "--format=%H", "--", "AGENTS.md")); mustStat(t, n, "AGENTS.md").Version != want {
		t.Fatal("AGENTS.md version")
	}
	merge, err := n.Commit(e.ctx, g.id(g.run(nil, "rev-parse", "HEAD~1")))
	if err != nil || len(merge.Commit.Parents) != 2 {
		t.Fatal("merge:", merge.Commit.Parents, err)
	}
	var ops []string
	for _, c := range merge.Changes {
		ops = append(ops, c.Op+":"+c.Path)
	}
	if strings.Join(ops, " ") != "modify:book.md add:side.md" {
		t.Fatalf("merge changes against the first parent: %v", ops)
	}
	module := mustStat(t, n, "vendor/module")
	if module.Mode != gitobj.ModeGitlink || module.Blob.String() != "0123456789012345678901234567890123456789" {
		t.Fatal("submodule entry:", module)
	}
	if _, _, err := n.Read(e.ctx, gitobj.ID{}, "vendor/module"); !errors.Is(err, notestore.ErrInvalid) {
		t.Fatal("reading a submodule entry:", err)
	}
	defer func() {
		// Content written over a submodule entry makes it a file.
		apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("vendor/module", "now a file\n")}})
		if f := mustStat(t, n, "vendor/module"); f.Mode != gitobj.ModeFile || f.EntryID != module.EntryID {
			t.Fatal("content over a submodule entry:", f)
		}
	}()

	// The export is the same repository.
	bare := newGitRepo(t, true)
	if err := n.Objects(e.ctx, 0, func(kind string, id gitobj.ID, body []byte) error {
		_, err := gitobj.WriteLoose(bare.dir, kind, body)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	bare.run(nil, "update-ref", "refs/heads/main", head.Commit.String())
	bare.run(nil, "fsck", "--strict", "--no-dangling")
	original := strings.Fields(g.run(nil, "rev-list", "--objects", "HEAD"))
	exported := strings.Fields(bare.run(nil, "rev-list", "--objects", "main"))
	slices.Sort(original)
	slices.Sort(exported)
	if !slices.Equal(original, exported) {
		t.Fatalf("exported objects differ: %d vs %d", len(exported), len(original))
	}
	// An incremental export after a new revision writes only its objects.
	r := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("book.md", strings.Replace(read(t, n, gitobj.ID{}, "book.md"), "Paragraph 10.", "Paragraph ten.", 1))}})
	var kinds []string
	if err := n.Objects(e.ctx, head.Seq, func(kind string, id gitobj.ID, body []byte) error {
		kinds = append(kinds, kind)
		_, err := gitobj.WriteLoose(bare.dir, kind, body)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(kinds, " ") != "blob tree commit" {
		t.Fatal("incremental export:", kinds)
	}
	bare.run(nil, "update-ref", "refs/heads/main", r.Head.Commit.String())
	bare.run(nil, "fsck", "--strict", "--no-dangling")
}

func mustStat(t *testing.T, n *notestore.Notebook, path string) notestore.File {
	t.Helper()
	f, err := n.Stat(t.Context(), gitobj.ID{}, path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// A rename pair from Git's rename detection passes a deleted file's entry to
// the file added in its place, edits included; a pair whose source the commit
// keeps, or never had, passes on nothing, so no entry is ever shared.
func TestImportFollowsGitRenamePairs(t *testing.T) {
	e := newEnv(t, notestore.Options{ImportEntryID: declaredID})
	g := newGitRepo(t, false)
	g.write("a.md", "# A\n\nThe first note.\n", 0o644)
	g.write("keep.md", "# Keep\n", 0o644)
	g.commit(ada, "Add notes", nil)
	os.Remove(filepath.Join(g.dir, "a.md"))
	g.write("c.md", "# A\n\nThe first note, edited.\n", 0o644)
	g.write("keep.md", "# Keep\n\nKept.\n", 0o644)
	g.write("d.md", "# D\n", 0o644)
	g.write("e.md", "# E\n", 0o644)
	g.commit(ada, "Rename and add", nil)
	n := e.store.Notebook("pairs")
	var heads []notestore.Head
	for _, raw := range strings.Fields(g.run(nil, "rev-list", "--reverse", "HEAD")) {
		commit := []byte(g.run(nil, "cat-file", "commit", raw))
		c, _ := gitobj.ParseCommit(commit)
		trees := map[gitobj.ID][]byte{c.Tree: []byte(g.run(nil, "cat-file", "tree", c.Tree.String()))}
		head, err := n.ImportCommit(e.ctx, commit, trees, func(id gitobj.ID) ([]byte, error) {
			return []byte(g.run(nil, "cat-file", "blob", id.String())), nil
		}, map[string]string{"c.md": "a.md", "d.md": "keep.md", "e.md": "missing.md"})
		if err != nil {
			t.Fatal(err)
		}
		heads = append(heads, head)
	}
	entry := func(head notestore.Head, path string) string {
		t.Helper()
		f, err := n.Stat(e.ctx, head.Commit, path)
		if err != nil {
			t.Fatal(path, err)
		}
		return f.EntryID
	}
	a, keep := entry(heads[0], "a.md"), entry(heads[0], "keep.md")
	if entry(heads[1], "c.md") != a || entry(heads[1], "keep.md") != keep {
		t.Fatal("the renamed note or the kept one lost its entry")
	}
	if d, x := entry(heads[1], "d.md"), entry(heads[1], "e.md"); d == keep || d == a || x == a || x == keep || d == x {
		t.Fatal("a pair whose source the commit keeps or never had passed an entry on")
	}
	infos, err := n.History(e.ctx, notestore.HistoryQuery{EntryID: a})
	if err != nil || len(infos) != 2 || !slices.ContainsFunc(infos[0].Changes, func(c notestore.Change) bool {
		return c.Op == notestore.OpRename && c.Path == "c.md" && c.OldPath == "a.md"
	}) {
		t.Fatalf("the rename is not recorded: %+v %v", infos, err)
	}
}

func TestImportRefusesObjectsThatDoNotMatchTheirIDs(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	g := newGitRepo(t, false)
	g.write("a.md", "a\n", 0o644)
	root := g.commit(ada, "Root", nil)
	g.write("a.md", "b\n", 0o644)
	child := g.commit(ada, "Child", nil)
	commit := func(id gitobj.ID) []byte { return []byte(g.run(nil, "cat-file", "commit", id.String())) }
	tree := func(id gitobj.ID) (gitobj.ID, []byte) {
		c, _ := gitobj.ParseCommit(commit(id))
		return c.Tree, []byte(g.run(nil, "cat-file", "tree", c.Tree.String()))
	}
	blobs := func(id gitobj.ID) ([]byte, error) { return []byte(g.run(nil, "cat-file", "blob", id.String())), nil }
	n := e.store.Notebook("imported")
	if _, err := n.ImportCommit(e.ctx, commit(child), nil, blobs); !errors.Is(err, notestore.ErrInvalid) {
		t.Fatal("a first commit with parents:", err)
	}
	rootTree, rootBody := tree(root)
	if _, err := n.ImportCommit(e.ctx, commit(root), map[gitobj.ID][]byte{rootTree: append(bytes.Clone(rootBody), 0)}, blobs); !errors.Is(err, notestore.ErrInvalid) {
		t.Fatal("a tampered tree:", err)
	}
	lying := func(gitobj.ID) ([]byte, error) { return []byte("not the content"), nil }
	if _, err := n.ImportCommit(e.ctx, commit(root), map[gitobj.ID][]byte{rootTree: rootBody}, lying); !errors.Is(err, notestore.ErrInvalid) {
		t.Fatal("a tampered blob:", err)
	}
	if _, err := n.Head(e.ctx); err == nil {
		t.Fatal("a refused import created the notebook")
	}
	if _, err := n.ImportCommit(e.ctx, commit(root), map[gitobj.ID][]byte{rootTree: rootBody}, blobs); err != nil {
		t.Fatal(err)
	}
	orphan := []byte(strings.Replace(string(commit(child)), root.String(), strings.Repeat("1", 40), 1))
	childTree, childBody := tree(child)
	if _, err := n.ImportCommit(e.ctx, orphan, map[gitobj.ID][]byte{childTree: childBody}, blobs); !errors.Is(err, notestore.ErrInvalid) {
		t.Fatal("a commit whose parent is missing:", err)
	}
	if _, err := n.ImportCommit(e.ctx, []byte("tree nonsense\n"), nil, blobs); !errors.Is(err, notestore.ErrInvalid) {
		t.Fatal("a malformed commit:", err)
	}
	head, err := n.ImportCommit(e.ctx, commit(child), map[gitobj.ID][]byte{childTree: childBody}, blobs)
	if err != nil || head.Commit != child || head.Seq != 2 {
		t.Fatal(head, err)
	}
	// FMN writes on top of imported history.
	r := apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("a.md", "c\n")}})
	info, _ := n.Commit(e.ctx, r.Head.Commit)
	if info.Commit.Parents[0] != child || !slices.Equal(slices.Sorted(maps.Keys(r.Versions)), []string{"a.md"}) {
		t.Fatal(info.Commit.Parents)
	}
}

// Files at a revision of imported history with branches come from that
// revision's first-parent line, never from a branch imported between its
// commits.
func TestHistoricalFilesFollowTheFirstParentLine(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	g := newGitRepo(t, false)
	sig := ada
	commit := func(subject string) gitobj.ID {
		sig.When += 60
		return g.commit(sig, subject, nil)
	}
	g.write("x.md", "x0\n", 0o644)
	g.write("y.md", "y0\n", 0o644)
	root := commit("Root")
	g.run(nil, "checkout", "-q", "-b", "side")
	g.run(nil, "rm", "-q", "x.md")
	s1 := commit("Drop x")
	g.write("x.md", "x again, and longer\n", 0o644)
	s2 := commit("Write x again")
	g.run(nil, "checkout", "-q", "main")
	g.write("y.md", "y1\n", 0o644)
	m1 := commit("Edit y")
	g.run(nil, "checkout", "-q", "side")
	g.write("s.md", "s\n", 0o644)
	s3 := commit("Add s")
	g.run(nil, "checkout", "-q", "main")
	g.write("y.md", "y2\n", 0o644)
	q := commit("Edit y again")
	g.runEnv([]string{"GIT_AUTHOR_NAME=Ada Author", "GIT_AUTHOR_EMAIL=ada@example.com", "GIT_AUTHOR_DATE=1700009999 +0000",
		"GIT_COMMITTER_NAME=Ada Author", "GIT_COMMITTER_EMAIL=ada@example.com", "GIT_COMMITTER_DATE=1700009999 +0000"}, nil, "merge", "-q", "--no-edit", "side")
	merge := g.id(g.run(nil, "rev-parse", "HEAD"))
	n := e.store.Notebook("imported")
	// Branches interleave: each side commit comes between main's.
	for _, id := range []gitobj.ID{root, s1, s2, m1, s3, q, merge} {
		raw := []byte(g.run(nil, "cat-file", "commit", id.String()))
		c, _ := gitobj.ParseCommit(raw)
		trees := map[gitobj.ID][]byte{c.Tree: []byte(g.run(nil, "cat-file", "tree", c.Tree.String()))}
		if _, err := n.ImportCommit(e.ctx, raw, trees, func(b gitobj.ID) ([]byte, error) {
			return []byte(g.run(nil, "cat-file", "blob", b.String())), nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	x, err := n.Stat(e.ctx, root, "x.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, rev := range []gitobj.ID{m1, q} {
		files, err := n.Files(e.ctx, rev)
		if err != nil || strings.Join(paths(files), " ") != "x.md y.md" {
			t.Fatal(paths(files), err)
		}
		f := files[0]
		if f.EntryID != x.EntryID || f.Size != 3 || f.Version != root {
			t.Fatalf("x.md at %s: %+v, want entry %s, 3 bytes, Version %s", rev, f, x.EntryID, root)
		}
		if want := g.id(g.run(nil, "log", "--first-parent", "-1", "--format=%H", rev.String(), "--", "y.md")); files[1].Version != want {
			t.Fatalf("y.md at %s: Version %s, want %s", rev, files[1].Version, want)
		}
		if st, err := n.Stat(e.ctx, rev, "x.md"); err != nil || st != f {
			t.Fatal("stat:", st, err)
		}
	}
	// The merge changed x.md on the first-parent line, keeping its entry.
	if f := mustStat(t, n, "x.md"); f.EntryID != x.EntryID || f.Size != int64(len("x again, and longer\n")) || f.Version != merge {
		t.Fatalf("x.md at the head: %+v", f)
	}
	if f, err := n.Stat(e.ctx, s3, "x.md"); err != nil || f.Size != int64(len("x again, and longer\n")) || f.Version != s2 {
		t.Fatalf("x.md on the side branch: %+v %v", f, err)
	}
}

// A tree an import leaves out must be the notebook's own already, even when
// another notebook's import put it in the shared tree cache.
func TestImportNeedsEveryTreeTheNotebookLacks(t *testing.T) {
	e := newEnv(t, notestore.Options{})
	g := newGitRepo(t, false)
	g.write("dir/a.md", "a\n", 0o644)
	root := g.commit(ada, "Root", nil)
	raw := []byte(g.run(nil, "cat-file", "commit", root.String()))
	c, _ := gitobj.ParseCommit(raw)
	dir := g.id(g.run(nil, "rev-parse", root.String()+":dir"))
	all := map[gitobj.ID][]byte{
		c.Tree: []byte(g.run(nil, "cat-file", "tree", c.Tree.String())),
		dir:    []byte(g.run(nil, "cat-file", "tree", dir.String())),
	}
	blobs := func(id gitobj.ID) ([]byte, error) { return []byte(g.run(nil, "cat-file", "blob", id.String())), nil }
	first := e.store.Notebook("first")
	if _, err := first.ImportCommit(e.ctx, raw, all, blobs); err != nil {
		t.Fatal(err)
	}
	// Exporting it reads its trees into the cache.
	if err := first.Objects(e.ctx, 0, func(string, gitobj.ID, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	second := e.store.Notebook("second")
	if _, err := second.ImportCommit(e.ctx, raw, map[gitobj.ID][]byte{c.Tree: all[c.Tree]}, blobs); !errors.Is(err, notestore.ErrInvalid) {
		t.Fatal("an import without a tree only another notebook has:", err)
	}
	if _, err := second.ImportCommit(e.ctx, raw, all, blobs); err != nil {
		t.Fatal(err)
	}
	if got := read(t, e.open(e.db, notestore.Options{}).Notebook("second"), gitobj.ID{}, "dir/a.md"); got != "a\n" {
		t.Fatal(got)
	}
}
