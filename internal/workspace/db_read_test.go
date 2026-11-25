package workspace

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// A treeChange is one path's change in a fixture revision.
type treeChange struct {
	path, content, from string
	mode                uint32 // 0 keeps a file's mode, or makes it regular
	delete              bool
}

// commitTree commits changes to s as one fixture revision and returns it.
func commitTree(t *testing.T, s *Store, changes ...treeChange) string {
	t.Helper()
	var files []notestore.FileChange
	for _, c := range changes {
		file := notestore.FileChange{Path: c.path, From: c.from, Mode: c.mode}
		if c.from == "" && !c.delete {
			file.Content = []byte(c.content)
		}
		files = append(files, file)
	}
	return commitFixture(t, s, files...)
}

// Search finds what production's "git grep -i -I -F" found, in its order:
// Unicode case folding, each query line a pattern, binary notes left out,
// and then the notes whose path contains the query.
func TestDatabaseSearchMatchesGitGrep(t *testing.T) {
	db := newTestStoreExcluding(t, []string{"private"})
	commitTree(t, db,
		treeChange{path: "accent.md", content: "Straße ÉTÉ\n"},
		treeChange{path: "kelvin.md", content: "K\n"},
		treeChange{path: "longs.md", content: "ſ\n"},
		treeChange{path: "sigma.md", content: "Σ\n"},
		treeChange{path: "final-sigma.md", content: "ς\n"},
		treeChange{path: "empty.md", content: ""},
		treeChange{path: "newline.md", content: "\n"},
		treeChange{path: "binary.md", content: "needle\x00\n"},
		treeChange{path: "late-nul.md", content: strings.Repeat("a", 8000) + "\x00 needle\n"},
		treeChange{path: "early-nul.md", content: strings.Repeat("a", 7999) + "\x00 needle\n"},
		treeChange{path: "lines.md", content: "line one\nline two\n"},
		treeChange{path: "invalid.md", content: "invalid \xc3( text \xff\n"},
		treeChange{path: "folder/Needle in path.md", content: "nothing\n"},
		treeChange{path: "media/needle.txt", content: "needle\n"},
		treeChange{path: "run.md", content: "needle run\n", mode: gitobj.ModeExecutable},
		treeChange{path: "link.md", content: "needle", mode: gitobj.ModeSymlink},
		treeChange{path: "private/needle.md", content: "needle\n"},
		treeChange{path: "long.md", content: "---\ntags: [long]\n---\n" + strings.Repeat("Filler paragraph.\n\n", 1000) + "The NEEDLE is late.\n"},
	)
	want := map[string][]string{
		"été":      {"accent.md"},
		"ÉTÉ":      {"accent.md"},
		"k":        {"kelvin.md"},
		"S":        {"accent.md", "long.md", "longs.md", "final-sigma.md", "lines.md", "sigma.md"},
		"ſ":        {"accent.md", "long.md", "longs.md"},
		"σ":        {"final-sigma.md", "sigma.md"},
		"ss":       nil,
		"needle":   {"late-nul.md", "long.md", "run.md", "folder/Needle in path.md"},
		"one\nzzz": {"lines.md"},
		"invalid":  {"invalid.md"},
		"\xc3(":    nil,
		"":         {"accent.md", "final-sigma.md", "folder/Needle in path.md", "invalid.md", "kelvin.md", "late-nul.md", "lines.md", "long.md", "longs.md", "newline.md", "run.md", "sigma.md", "binary.md", "early-nul.md", "empty.md"},
		"e\n":      {"accent.md", "final-sigma.md", "folder/Needle in path.md", "invalid.md", "kelvin.md", "late-nul.md", "lines.md", "long.md", "longs.md", "newline.md", "run.md", "sigma.md"},
	}
	ctx := context.Background()
	for query, paths := range want {
		found, err := db.Find(ctx, query, "", "", "")
		if query == "\xc3(" {
			if err == nil {
				t.Fatal("searched with invalid UTF-8, which Git refuses")
			}
			continue
		}
		if err != nil {
			t.Fatalf("find %q: %v", query, err)
		}
		var got []string
		for _, e := range found {
			got = append(got, e.Path)
		}
		if !reflect.DeepEqual(got, paths) {
			t.Errorf("find %q = %q, want %q", query, got, paths)
		}
	}
	if found, err := db.Find(ctx, "needle", "long", "", ""); err != nil || len(found) != 1 || found[0].Path != "long.md" {
		t.Fatal("filters after search:", found, err)
	}
	if _, err := db.Find(ctx, strings.Repeat("x", 201), "", "", ""); err == nil {
		t.Fatal("searched beyond 200 characters")
	}
}

// ChangesSince reports, for each range, the files and the commits that
// touched them, as the Git backend reported them for the same history (its
// answers are recorded here): renames by entry, a path deleted and written
// again as modified, mode changes, excluded folders left out, and a fresh
// review from no checkpoint or one that is not an ancestor.
func TestDatabaseChangesSinceMatchesGit(t *testing.T) {
	db := newTestStoreExcluding(t, []string{"private"})
	var revisions []string
	commit := func(changes ...treeChange) { revisions = append(revisions, commitTree(t, db, changes...)) }
	body := func(name string) string {
		return "# " + name + "\n\n" + strings.Repeat("A paragraph of "+name+" that stays the same.\n", 20)
	}
	commit(
		treeChange{path: "a.md", content: body("a")},
		treeChange{path: "b.md", content: body("b")},
		treeChange{path: "c.md", content: body("c")},
		treeChange{path: "d.md", content: body("d")},
		treeChange{path: "keep.md", content: body("keep")},
		treeChange{path: "private/q.md", content: body("q")},
		treeChange{path: "private/p.md", content: body("p")},
		treeChange{path: "comments/x.json", content: "[]"},
		treeChange{path: "assets/map.png", content: "\x89PNG\x00"},
		treeChange{path: "a*.md", content: body("star")},
	)
	commit(treeChange{path: "moved/a.md", from: "a.md"})
	commit(treeChange{path: "b.md", delete: true})
	commit(treeChange{path: "b.md", content: "# A different b\n"})
	commit(treeChange{path: "c.md", content: body("c"), mode: gitobj.ModeExecutable})
	commit(treeChange{path: "d.md", content: "c.md", mode: gitobj.ModeSymlink})
	commit(treeChange{path: "private/p.md", content: body("p") + "More.\n"})
	commit(treeChange{path: "comments/x.json", content: `[{"id":"x"}]`})
	commit(treeChange{path: "private/keep.md", from: "keep.md"})
	commit(treeChange{path: "q.md", from: "private/q.md"})
	commit(treeChange{path: "assets/map.png", content: "\x89PNG\x01"}, treeChange{path: "a*.md", content: body("star") + "Edited.\n"})
	ctx := context.Background()
	modified := func(path string) FileChange { return FileChange{Kind: "modified", Path: path} }
	added := func(path string) FileChange { return FileChange{Kind: "added", Path: path} }
	deleted := func(path string) FileChange { return FileChange{Kind: "deleted", Path: path} }
	renamed := FileChange{Kind: "renamed", Path: "moved/a.md", From: "a.md"}
	for _, c := range []struct {
		from, to int
		files    []FileChange
		commits  []int
	}{
		{0, 10, []FileChange{modified("a*.md"), modified("assets/map.png"), modified("b.md"), modified("c.md"), renamed, deleted("keep.md"), added("q.md")}, []int{10, 9, 8, 4, 3, 2, 1}},
		{0, 1, []FileChange{renamed}, []int{1}},
		{1, 3, []FileChange{modified("b.md")}, []int{3, 2}},
		{1, 2, []FileChange{deleted("b.md")}, []int{2}},
		{3, 5, []FileChange{modified("c.md")}, []int{4}},
		{5, 9, []FileChange{deleted("keep.md"), added("q.md")}, []int{9, 8}},
		{7, 8, []FileChange{deleted("keep.md")}, []int{8}},
		{8, 9, []FileChange{added("q.md")}, []int{9}},
		{9, 10, []FileChange{modified("a*.md"), modified("assets/map.png")}, []int{10}},
		{4, 4, []FileChange{}, nil},
	} {
		changes, err := db.ChangesSince(ctx, revisions[c.from], revisions[c.to])
		if err != nil {
			t.Fatal(c.from, c.to, err)
		}
		if !reflect.DeepEqual(changes.Files, c.files) || changes.FreshReview || changes.CommitsTruncated {
			t.Errorf("changes %d..%d: %+v, want files %+v", c.from, c.to, changes, c.files)
		}
		var commits []int
		for _, commit := range changes.Commits {
			commits = append(commits, slices.Index(revisions, commit.Revision))
			if commit.Source != "" || commit.Message != "" || commit.AuthorName != fixtureAuthor.Name || commit.CommitterName != "ForgetMeNote" || commit.AuthoredAt == "" {
				t.Errorf("commit %+v", commit)
			}
		}
		if !reflect.DeepEqual(commits, c.commits) {
			t.Errorf("changes %d..%d attribute commits %v, want %v", c.from, c.to, commits, c.commits)
		}
	}
	head := []FileChange{added("a*.md"), added("assets/map.png"), added("b.md"), added("moved/a.md"), added("q.md")}
	for from, reason := range map[string]string{
		"":                      "No valid checkpoint; review the current snapshot.",
		"not-a-revision":        "No valid checkpoint; review the current snapshot.",
		strings.Repeat("1", 40): "Checkpoint is missing or is not an ancestor; a fresh review is required.",
		revisions[10]:           "Checkpoint is missing or is not an ancestor; a fresh review is required.",
	} {
		fresh, err := db.ChangesSince(ctx, from, revisions[9])
		if err != nil || !fresh.FreshReview || fresh.Reason != reason || !reflect.DeepEqual(fresh.Files, head) {
			t.Errorf("fresh review from %q: %+v %v", from, fresh, err)
		}
	}
	if _, err := db.ChangesSince(ctx, revisions[0], strings.Repeat("1", 40)); err == nil {
		t.Fatal("changes to an unknown revision")
	}
	// d.md is a symbolic link at the head, so reads of the revision that held
	// it as a note leave it out as the Git backend did.
	snapshot, err := db.Snapshot(ctx, revisions[4])
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range snapshot.Files {
		paths = append(paths, f.Path)
	}
	if want := []string{"a*.md", "b.md", "keep.md", "moved/a.md"}; !reflect.DeepEqual(paths, want) {
		t.Errorf("snapshot %v, want %v", paths, want)
	}
	if _, err := db.KnowledgeRead(ctx, "d.md", revisions[4]); !errors.Is(err, ErrForbidden) {
		t.Error("knowledge read of a path linked at the head:", err)
	}
	if visible, err := db.VisibleHistoryPaths(ctx, []string{"d.md", "c.md"}); err != nil || !reflect.DeepEqual(visible, map[string]bool{"c.md": true}) {
		t.Error("visible history paths:", visible, err)
	}
	for _, folder := range []string{"d.md", "c.md/x"} {
		if _, err := db.BrowseCollection(ctx, folder, ""); !errors.Is(err, ErrForbidden) {
			t.Error("browsed", folder, err)
		}
	}
}

// A path deleted and written again, or renamed while another entry takes
// its place, is one modification, as Git compares paths; an entry the
// store kept is a rename.
func TestGitDiffComparesPathsAndFollowsEntries(t *testing.T) {
	for _, c := range []struct {
		name    string
		changes []notestore.Change
		want    []FileChange
	}{
		{"recreated", []notestore.Change{{Op: "delete", Path: "x.md"}, {Op: "add", Path: "x.md"}}, []FileChange{{Kind: "modified", Path: "x.md"}}},
		{"renamed", []notestore.Change{{Op: "rename", Path: "z.md", OldPath: "a.md"}, {Op: "add", Path: "b.md"}}, []FileChange{{Kind: "added", Path: "b.md"}, {Kind: "renamed", From: "a.md", Path: "z.md"}}},
		{"replaced", []notestore.Change{{Op: "rename", Path: "y.md", OldPath: "x.md"}, {Op: "add", Path: "x.md"}}, []FileChange{{Kind: "modified", Path: "x.md"}, {Kind: "added", Path: "y.md"}}},
		{"onto", []notestore.Change{{Op: "rename", Path: "y.md", OldPath: "x.md"}, {Op: "delete", Path: "y.md"}}, []FileChange{{Kind: "deleted", Path: "x.md"}, {Kind: "modified", Path: "y.md"}}},
		{"swapped", []notestore.Change{{Op: "rename", Path: "y.md", OldPath: "x.md"}, {Op: "rename", Path: "x.md", OldPath: "y.md"}}, []FileChange{{Kind: "modified", Path: "x.md"}, {Kind: "modified", Path: "y.md"}}},
	} {
		if got := gitDiff(c.changes); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// A change set names at most 256 of the commits that made it, newest first,
// and says when it names fewer than made it.
func TestDatabaseChangesSinceTruncatesCommits(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	from := commitFixture(t, s, fixture("busy.md", "0"), fixture("quiet.md", "quiet"))
	var revisions []string
	for i := 1; i <= 258; i++ {
		revisions = append(revisions, commitFixture(t, s, fixture("busy.md", fmt.Sprint(i))))
	}
	commitFixture(t, s, fixture("private.md", "unrelated"))
	s.browseExcluded = []string{"private.md"}
	changes, err := s.ChangesSince(ctx, from, "")
	if err != nil || len(changes.Files) != 1 || len(changes.Commits) != 256 || !changes.CommitsTruncated {
		t.Fatalf("changes: %d files, %d commits, truncated %t, %v", len(changes.Files), len(changes.Commits), changes.CommitsTruncated, err)
	}
	if changes.Commits[0].Revision != revisions[257] || changes.Commits[255].Revision != revisions[2] {
		t.Fatal("not the newest commits, newest first")
	}
	changes, err = s.ChangesSince(ctx, revisions[250], revisions[255])
	if err != nil || len(changes.Commits) != 5 || changes.CommitsTruncated || changes.Commits[4].Revision != revisions[251] {
		t.Fatalf("a short range: %+v %v", changes.Commits, err)
	}
}

// Snapshots, knowledge reads and snapshot assertions read only revisions of
// the notebook, and only what its exclusions leave visible.
func TestDatabaseSnapshotsAndKnowledgeReads(t *testing.T) {
	s := newTestStoreExcluding(t, []string{"private"})
	ctx := context.Background()
	first := commitFixture(t, s, fixture("a.md", "# A\n"), fixture("private/b.md", "# B\n"), fixture("assets/x.png", "png"))
	second := commitFixture(t, s, fixture("a.md", "# A, later\n"))
	other := newTestStore(t)
	foreign := commitFixture(t, other, fixture("a.md", "# Elsewhere\n"))
	if doc, err := s.KnowledgeRead(ctx, "a.md", first); err != nil || string(doc.Content) != "# A\n" || doc.Revision != first || doc.Version != first {
		t.Fatal("knowledge read at a revision:", doc, err)
	}
	if doc, err := s.KnowledgeRead(ctx, "a.md", ""); err != nil || string(doc.Content) != "# A, later\n" || doc.Revision != second {
		t.Fatal("knowledge read at the head:", doc, err)
	}
	for _, p := range []string{"private/b.md", "assets/x.png", "../a.md", ".git/config"} {
		if _, err := s.KnowledgeRead(ctx, p, ""); !errors.Is(err, ErrForbidden) {
			t.Fatal("knowledge read of", p, err)
		}
	}
	if _, err := s.KnowledgeRead(ctx, "a.md", foreign); err == nil {
		t.Fatal("read another notebook's revision")
	}
	if _, err := s.Snapshot(ctx, foreign); err == nil || !strings.Contains(err.Error(), "snapshot commit is unavailable") {
		t.Fatal("snapshot of another notebook's revision:", err)
	}
	if _, err := s.Snapshot(ctx, "HEAD"); !errors.Is(err, ErrForbidden) {
		t.Fatal("snapshot of a symbolic revision:", err)
	}
	if snap, err := s.Snapshot(ctx, first); err != nil || snap.Revision != first || len(snap.Files) != 1 || snap.Files[0].Content != "# A\n" {
		t.Fatal("snapshot at a revision:", snap, err)
	}
	for _, revision := range []string{first, second} {
		if err := s.AssertSnapshot(ctx, revision); err != nil {
			t.Fatal("assert", revision, err)
		}
	}
	for _, revision := range []string{foreign, "HEAD", ""} {
		if err := s.AssertSnapshot(ctx, revision); !errors.Is(err, ErrConflict) {
			t.Fatal("asserted", revision, err)
		}
	}
	s.SetAuthorityGeneration(3)
	if _, err := s.KnowledgeRead(ctx, "a.md", ""); err != nil {
		t.Fatal("a read after the generation changed:", err)
	}
}

// The metadata of a note follows its content: a renamed or copied note is
// listed at its new path with the metadata it had, and callers that hold mu
// list what List lists.
func TestDatabaseListingFollowsContentAcrossPaths(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	note := "---\ntitle: Kept title\ntags: [kept]\n---\n# Heading\n\nBody.\n"
	commitFixture(t, s, fixture("a.md", note))
	if _, err := s.List(ctx); err != nil {
		t.Fatal(err)
	}
	commitFixture(t, s, notestore.FileChange{Path: "folder/b.md", From: "a.md"}, fixture("copy.md", note))
	entries, err := s.List(ctx)
	if err != nil || len(entries) != 2 {
		t.Fatal(entries, err)
	}
	for i, p := range []string{"copy.md", "folder/b.md"} {
		if e := entries[i]; e.Path != p || e.Metadata == nil || e.Metadata.Title != "Kept title" || e.Title != "Heading" || e.Excerpt != "Body." || !e.Editable {
			t.Fatalf("entry %d: %+v", i, e)
		}
	}
	backend := s.backend
	s.mu.Lock()
	locked, err := backend.list(ctx)
	s.mu.Unlock()
	if err != nil || !reflect.DeepEqual(locked, entries) {
		t.Fatal("list under mu:", locked, err)
	}
	if found, err := s.Find(ctx, "", "kept", "", "folder/"); err != nil || len(found) != 1 || found[0].Path != "folder/b.md" {
		t.Fatal("filtered after the rename:", found, err)
	}
}
