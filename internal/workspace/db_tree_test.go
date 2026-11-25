package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// The tree twins answer what their Git originals answer, from the notes
// store: existence, folders, ancestry, path versions, content and snapshots.
func TestTreeTwinsReadTheCommittedNotebook(t *testing.T) {
	s := newTestStoreExcluding(t, []string{"private"})
	backend := s.backend.(*dbNotebook)
	ctx := context.Background()
	first := commitFixture(t, s,
		fixture("b.md", "# B\n"),
		fixture("a/one.md", "---\nfmn-container: archive\n---\n# One\n"),
		fixture("private/secret.md", "# Secret\n"),
		fixture("assets/x.png", "png"),
		notestore.FileChange{Path: "run.md", Content: []byte("#!/bin/sh\n"), Mode: gitobj.ModeExecutable},
	)
	second := commitFixture(t, s, notestore.FileChange{Path: "b.md"}, fixture("c.md", "# C\n"))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range []struct {
		revision, path string
		file, dir      bool
	}{
		{second, "c.md", true, false},
		{second, "b.md", false, false},
		{first, "b.md", true, false},
		{second, "a", false, true},
		{second, "a/", false, true},
		{second, "a/one.md", true, false},
		{"", "assets", false, true},
		{"", "asset", false, false},
	} {
		file, err := backend.exists(ctx, c.revision, c.path)
		if err != nil || file != c.file {
			t.Errorf("exists %s at %s: %v %v", c.path, c.revision, file, err)
		}
		dir, err := backend.isDir(ctx, c.revision, c.path)
		if err != nil || dir != c.dir {
			t.Errorf("isDir %s at %s: %v %v", c.path, c.revision, dir, err)
		}
	}
	for _, c := range []struct {
		revision, head string
		want           bool
	}{
		{first, second, true}, {second, second, true}, {second, first, false},
		{strings.Repeat("0", 40), second, false}, {strings.Repeat("1", 40), second, false}, {"HEAD", second, false},
	} {
		if got, err := backend.isAncestor(ctx, c.revision, c.head); err != nil || got != c.want {
			t.Errorf("isAncestor %s %s: %v %v", c.revision, c.head, got, err)
		}
	}
	for _, c := range []struct{ path, revision, want string }{
		{"b.md", second, second}, {"b.md", first, first}, {"c.md", first, ""}, {"never.md", second, ""},
	} {
		if got, err := backend.fileVersion(ctx, c.path, c.revision); err != nil || got != c.want {
			t.Errorf("fileVersion %s at %s: %q %v", c.path, c.revision, got, err)
		}
	}

	notes := func(p string) bool { return strings.HasSuffix(p, ".md") && p != "run.md" }
	contents, err := backend.readCommittedContents(ctx, second, notes)
	if err != nil || len(contents) != 3 || string(contents["c.md"]) != "# C\n" || contents["private/secret.md"] == nil {
		t.Fatal("committed contents:", contents, err)
	}
	if err := backend.forEachCommittedContent(ctx, second, func(p string) bool { return p == "run.md" }, func(string, []byte) error { return nil }); !errors.Is(err, ErrForbidden) {
		t.Error("an executable selected as content:", err)
	}
	if err := backend.forEachRequiredContent(ctx, second, map[string]bool{"c.md": true, "b.md": true}, func(string, []byte) error { return nil }); !errors.Is(err, os.ErrNotExist) {
		t.Error("a missing required note:", err)
	}
	stop := errors.New("stop")
	if err := backend.forEachCommittedContent(ctx, second, notes, func(string, []byte) error { return stop }); !errors.Is(err, stop) {
		t.Error("a consumer's error:", err)
	}
	t.Setenv("FMN_LIMIT_FILE_BYTES", "4")
	if err := backend.forEachCommittedContent(ctx, second, notes, func(string, []byte) error { return nil }); err == nil {
		t.Error("content beyond the file budget was read")
	}
}

func TestSnapshotTwinsSelectVisibleNotesInPathOrder(t *testing.T) {
	s := newTestStoreExcluding(t, []string{"private"})
	backend := s.backend.(*dbNotebook)
	ctx := context.Background()
	first := commitFixture(t, s,
		fixture("z.md", "# Z\n"),
		fixture("a-b.md", "# Dash\n"),
		fixture("a.md", "# Dot\n"),
		fixture("a/one.md", "---\nfmn-container: archive\n---\n# One\n"),
		fixture("private/secret.md", "# Secret\n"),
		fixture("comments/x.json", "[]"),
		notestore.FileChange{Path: "run.md", Content: []byte("#!/bin/sh\n"), Mode: gitobj.ModeExecutable},
	)
	commitFixture(t, s, fixture("m.md", "# M\n"))
	s.mu.Lock()
	defer s.mu.Unlock()
	paths := func(snap Snapshot) []string {
		var out []string
		for _, f := range snap.Files {
			out = append(out, f.Path)
		}
		return out
	}
	snap, err := backend.snapshot(ctx, "")
	if want := []string{"a-b.md", "a.md", "a/one.md", "m.md", "z.md"}; err != nil || !slices.Equal(paths(snap), want) {
		t.Fatal("head snapshot:", paths(snap), err)
	}
	// The order is Git's own tree order.
	bare := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	if err := s.notes.Objects(ctx, 0, func(kind string, _ gitobj.ID, body []byte) error {
		_, err := gitobj.WriteLoose(bare, kind, body)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "--git-dir", bare, "ls-tree", "-r", "--name-only", snap.Revision).Output()
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, p := range strings.Fields(string(out)) {
		if slices.Contains(paths(snap), p) {
			listed = append(listed, p)
		}
	}
	if !slices.Equal(listed, paths(snap)) {
		t.Fatal("snapshot order", paths(snap), "git", listed)
	}
	if snap.Files[0].Hash != ContentHash([]byte("# Dash\n")) || snap.Visibility != historyVisibilityID([]string{"private"}) {
		t.Fatal("snapshot file:", snap.Files[0], snap.Visibility)
	}
	scoped, err := backend.snapshotWithScope(ctx, first, &excludingScope)
	if want := []string{"a-b.md", "a.md", "z.md"}; err != nil || !slices.Equal(paths(scoped), want) || scoped.Revision != first || scoped.ScopeID != excludingScope.ID() {
		t.Fatal("scoped snapshot at the first revision:", paths(scoped), err)
	}
	if _, err := backend.snapshot(ctx, strings.Repeat("1", 40)); err == nil {
		t.Fatal("a snapshot at an unknown revision")
	}
	if _, err := backend.snapshot(ctx, "HEAD"); !errors.Is(err, ErrForbidden) {
		t.Fatal("a snapshot at a symbolic revision:", err)
	}
}

// The identity index rereads only notes whose content changed.
func TestIdentityIndexRereadsOnlyChangedNotes(t *testing.T) {
	s := newTestStore(t)
	backend := s.backend.(*dbNotebook)
	ctx := context.Background()
	one, _ := WithNoteID([]byte("# One\n"), strings.Repeat("1", 32))
	two, _ := WithNoteID([]byte("# Two\n"), strings.Repeat("2", 32))
	commitFixture(t, s, fixture("one.md", string(one)), fixture("two.md", string(two)), fixture("plain.md", "# Plain\n"), fixture("AGENTS.md", string(one)))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := backend.identityIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if s.identityIDs["one.md"] != strings.Repeat("1", 32) || s.identityIDs["two.md"] != strings.Repeat("2", 32) || s.identityIDs["plain.md"] != "" || len(s.identityIDs) != 3 {
		t.Fatal("identities:", s.identityIDs)
	}
	// A cached identity is kept for an unchanged blob even when its content
	// is no longer read: corrupt the cache and see it survive a new head.
	s.identityIDs["one.md"] = "cached"
	commitFixture(t, s, fixture("two.md", "# Two, no longer identified\n"))
	if err := backend.identityIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if s.identityIDs["one.md"] != "cached" || s.identityIDs["two.md"] != "" {
		t.Fatal("identities after a change:", s.identityIDs)
	}
}
