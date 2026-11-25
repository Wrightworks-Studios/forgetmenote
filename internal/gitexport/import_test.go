package gitexport

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// workTree is a Git working tree a test commits notebook history into.
type workTree struct {
	t    *testing.T
	dir  string
	when int64
}

func newWorkTree(t *testing.T) *workTree {
	w := &workTree{t: t, dir: t.TempDir(), when: 1700000000}
	git(t, w.dir, "init", "--quiet", "--initial-branch=main")
	return w
}

func (w *workTree) write(path, content string) {
	w.t.Helper()
	full := filepath.Join(w.dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *workTree) commit(subject string) gitobj.ID {
	w.t.Helper()
	if out, err := gitErr(w.dir, nil, "add", "--all"); err != nil {
		w.t.Fatal(out)
	}
	return w.commitIndex(subject)
}

// commitIndex commits the index as it is, which keeps a submodule entry
// that has no directory.
func (w *workTree) commitIndex(subject string) gitobj.ID {
	w.t.Helper()
	w.when += 60
	date := []string{"GIT_AUTHOR_DATE=" + strconv.FormatInt(w.when, 10) + " +0100", "GIT_COMMITTER_DATE=" + strconv.FormatInt(w.when, 10) + " +0100"}
	if out, err := gitErr(w.dir, date, "commit", "--quiet", "--allow-empty", "-m", subject, "-m", "Gitnotes-Source: mcp"); err != nil {
		w.t.Fatal(out)
	}
	return mustID(w.t, git(w.t, w.dir, "rev-parse", "HEAD"))
}

func (w *workTree) bundle(args ...string) []byte {
	w.t.Helper()
	file := filepath.Join(w.t.TempDir(), "x.bundle")
	git(w.t, w.dir, append([]string{"bundle", "create", "--quiet", file}, args...)...)
	raw, err := os.ReadFile(file)
	if err != nil {
		w.t.Fatal(err)
	}
	return raw
}

// branchyHistory is Git history beyond what FMN writes: a side branch merged
// back, a rename, a symlink, an executable and a submodule entry.
func branchyHistory(t *testing.T) *workTree {
	w := newWorkTree(t)
	w.write("Inbox/idea.md", note("0123456789abcdef0123456789abcdef", "Idea", "An idea.\n"))
	w.write("AGENTS.md", "Instructions.\n")
	w.commit("Initial notes")
	git(t, w.dir, "checkout", "--quiet", "-b", "side")
	w.write("side.md", "side\n")
	w.commit("Side work")
	git(t, w.dir, "checkout", "--quiet", "main")
	os.Mkdir(filepath.Join(w.dir, "Projects"), 0o755)
	git(t, w.dir, "mv", "Inbox/idea.md", "Projects/idea.md")
	w.write("tools/run.sh", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(w.dir, "tools/run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("AGENTS.md", filepath.Join(w.dir, "link")); err != nil {
		t.Fatal(err)
	}
	w.commit("Move the idea")
	git(t, w.dir, "merge", "--quiet", "--no-edit", "side")
	git(t, w.dir, "update-index", "--add", "--cacheinfo", "160000,0123456789012345678901234567890123456789,vendor/module")
	w.commitIndex("Pin a module")
	if !strings.Contains(git(t, w.dir, "ls-tree", "-r", "HEAD"), "160000 commit") {
		t.Fatal("the history has no submodule entry")
	}
	return w
}

func TestImportGitDirKeepsHistoryAndVerifies(t *testing.T) {
	e := newEnv(t)
	w := branchyHistory(t)
	gitDir := filepath.Join(w.dir, ".git")
	n := e.store.Notebook("imported")
	head, err := ImportGitDir(e.ctx, n, gitDir)
	if err != nil {
		t.Fatal(err)
	}
	commits := strings.Fields(git(t, w.dir, "rev-list", "HEAD"))
	if head.Commit.String() != commits[0] || head.Seq != int64(len(commits)) {
		t.Fatalf("head %+v, Git %v", head, commits)
	}
	rep, err := Verify(e.ctx, n, gitDir)
	if err != nil {
		t.Fatal(err)
	}
	objects := sortedFields(git(t, w.dir, "rev-list", "--objects", "--no-object-names", "HEAD"))
	if rep.Head != head.Commit || rep.Commits != len(commits) || rep.Commits+rep.Trees+rep.Blobs != len(objects) {
		t.Fatalf("report %+v for %d objects", rep, len(objects))
	}
	idea, err := n.Stat(e.ctx, gitobj.ID{}, "Projects/idea.md")
	if err != nil || idea.EntryID != "0123456789abcdef0123456789abcdef" {
		t.Fatal("the note keeps its fmn-id:", idea, err)
	}
	if _, err := e.ex.Sync(e.ctx, "imported"); err != nil {
		t.Fatal(err)
	}
	repo := e.ex.Path("imported")
	git(t, repo, "fsck", "--strict", "--no-dangling")
	if exported := sortedFields(git(t, repo, "rev-list", "--objects", "--no-object-names", "main")); !slices.Equal(exported, objects) {
		t.Fatalf("the export has %d objects, Git %d", len(exported), len(objects))
	}
	if _, err := Verify(e.ctx, n, repo); err != nil {
		t.Fatal("the export verifies too:", err)
	}

	// A notebook with history takes no import; a Git history that moved
	// on no longer verifies.
	if _, err := ImportGitDir(e.ctx, n, gitDir); !errors.Is(err, ErrNotEmpty) {
		t.Fatal("an import into a notebook with history:", err)
	}
	w.write("more.md", "more\n")
	w.commit("More")
	if _, err := Verify(e.ctx, n, gitDir); !errors.Is(err, ErrMismatch) {
		t.Fatal("verifying against other history:", err)
	}
}

// A file without a declared identity keeps its entry through every rename
// Git detects (--find-renames), edits included, as the Git backend's history
// follows it; a file whose content declares an identity keeps that one even
// where Git pairs it with a deleted file.
func TestImportGitDirFollowsGitRenames(t *testing.T) {
	e := newEnv(t)
	w := newWorkTree(t)
	plan := "# Plan\n\n" + strings.Repeat("A step of the plan.\n", 12)
	table := strings.Repeat("a,b,c\n", 20)
	w.write("plan.md", plan)
	w.write("assets/table.csv", table)
	w.write("draft.md", plan+"Drafted.\n")
	first := w.commit("Add the plan")
	os.Remove(filepath.Join(w.dir, "plan.md"))
	os.Remove(filepath.Join(w.dir, "assets", "table.csv"))
	w.write("Trash/plan.md", plan+"Trashed.\n")
	w.write("assets/sheet.csv", table+"d,e,f\n")
	moved := w.commit(`Move "plan.md" to "Trash/plan.md"`)
	os.Remove(filepath.Join(w.dir, "Trash", "plan.md"))
	w.write("plan.md", plan+"Restored.\n")
	restored := w.commit("Restore note from Trash")
	os.Remove(filepath.Join(w.dir, "draft.md"))
	w.write("final.md", note("fedcba9876543210fedcba9876543210", "Final", plan+"Drafted.\n"))
	finished := w.commit("Finish the draft")
	if renames := git(t, w.dir, "log", "--find-renames", "--diff-filter=R", "--format=%H", "HEAD"); len(strings.Fields(renames)) != 3 {
		t.Fatalf("Git detects renames in %q, not in each later commit", renames)
	}

	n := e.store.Notebook("renamed")
	if _, err := ImportGitDir(e.ctx, n, filepath.Join(w.dir, ".git")); err != nil {
		t.Fatal(err)
	}
	entry := func(rev gitobj.ID, path string) string {
		t.Helper()
		f, err := n.Stat(e.ctx, rev, path)
		if err != nil {
			t.Fatal(path, err)
		}
		return f.EntryID
	}
	note, asset := entry(first, "plan.md"), entry(first, "assets/table.csv")
	if entry(moved, "Trash/plan.md") != note || entry(restored, "plan.md") != note || entry(finished, "plan.md") != note {
		t.Fatal("the note's moves with edits gave it new entries")
	}
	if entry(moved, "assets/sheet.csv") != asset {
		t.Fatal("the attachment's move with an edit gave it a new entry")
	}
	if got := entry(finished, "final.md"); got != "fedcba9876543210fedcba9876543210" {
		t.Fatal("a note that declares its identity took", got)
	}
	infos, err := n.History(e.ctx, notestore.HistoryQuery{EntryID: note})
	if err != nil || len(infos) != 3 || infos[0].ID != restored || infos[1].ID != moved || infos[2].ID != first {
		t.Fatalf("the note's history: %d revisions, %v", len(infos), err)
	}
	for i, from := range []string{"Trash/plan.md", "plan.md"} {
		var renamed bool
		for _, c := range infos[i].Changes {
			renamed = renamed || c.EntryID == note && c.Op == notestore.OpRename && c.OldPath == from
		}
		if !renamed {
			t.Fatalf("revision %s does not record the rename from %s: %+v", infos[i].ID, from, infos[i].Changes)
		}
	}
}

// While history with branches is imported, the head passes through commits
// that are not each other's descendants. The export's branch never moves
// backwards or sideways; it waits for a descendant.
func TestExportBranchOnlyMovesToDescendants(t *testing.T) {
	e := newEnv(t)
	w := newWorkTree(t)
	w.write("a.md", "a\n")
	root := w.commit("Root")
	git(t, w.dir, "checkout", "--quiet", "-b", "side")
	w.write("s.md", "s\n")
	side := w.commit("Side")
	git(t, w.dir, "checkout", "--quiet", "main")
	w.write("a.md", "a2\n")
	main := w.commit("Main")
	git(t, w.dir, "merge", "--quiet", "--no-edit", "side")
	merge := mustID(t, git(t, w.dir, "rev-parse", "HEAD"))
	cat, err := openCatFile(e.ctx, filepath.Join(w.dir, ".git"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	n := e.store.Notebook("branchy")
	importOne := func(id gitobj.ID) {
		raw, _ := cat.read(id, "commit")
		trees := map[gitobj.ID][]byte{}
		for _, line := range strings.Split(git(t, w.dir, "ls-tree", "-r", "-t", id.String()), "\n") {
			if f := strings.Fields(line); len(f) >= 3 && f[1] == "tree" {
				trees[mustID(t, f[2])], _ = cat.read(mustID(t, f[2]), "tree")
			}
		}
		c, _ := gitobj.ParseCommit(raw)
		trees[c.Tree], _ = cat.read(c.Tree, "tree")
		if _, err := n.ImportCommit(e.ctx, raw, trees, func(b gitobj.ID) ([]byte, error) { return cat.read(b, "blob") }); err != nil {
			t.Fatal(err)
		}
	}
	repo := e.ex.Path("branchy")
	for _, step := range []struct {
		commit, exported gitobj.ID
	}{{root, root}, {side, side}, {main, side}, {merge, merge}} {
		importOne(step.commit)
		_, err := e.ex.Sync(e.ctx, "branchy")
		if step.exported != step.commit && !errors.Is(err, ErrDiverged) || step.exported == step.commit && err != nil {
			t.Fatalf("exporting %s: %v", step.commit, err)
		}
		if got := refOf(t, repo); got != step.exported {
			t.Fatalf("after importing %s the branch is %s, want %s", step.commit, got, step.exported)
		}
	}
	git(t, repo, "fsck", "--strict", "--no-dangling")
}

// An object whose content does not match its ID stops the import.
func TestImportGitDirRefusesCorruptObjects(t *testing.T) {
	e := newEnv(t)
	w := newWorkTree(t)
	w.write("a.md", "the real content\n")
	w.commit("Root")
	blob := git(t, w.dir, "rev-parse", "HEAD:a.md")
	loose := filepath.Join(w.dir, ".git", "objects", blob[:2], blob[2:])
	forged := t.TempDir()
	id, err := gitobj.WriteLoose(forged, "blob", []byte("forged content!!!\n"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(forged, "objects", id.String()[:2], id.String()[2:]))
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(loose)
	if err := os.WriteFile(loose, raw, 0o444); err != nil {
		t.Fatal(err)
	}
	n := e.store.Notebook("corrupt")
	if _, err := ImportGitDir(e.ctx, n, filepath.Join(w.dir, ".git")); !errors.Is(err, notestore.ErrInvalid) {
		t.Fatal("importing a corrupt blob:", err)
	}
	if _, err := n.Head(e.ctx); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a refused import stored its root commit:", err)
	}
}

// Bundle imports refuse, before storing anything, what the notebook import
// has always refused.
func TestImportBundleRefusals(t *testing.T) {
	e := newEnv(t)
	base := func() *workTree {
		w := newWorkTree(t)
		w.write("Inbox/a.md", note("0123456789abcdef0123456789abcdef", "A", "A note.\n"))
		w.write("assets/pic.png", "\x89PNG")
		w.write("comments/"+strings.Repeat("cd", 32)+".json", "{}\n")
		w.write(".gitnotes-locks.json", "{}\n")
		w.commit("First")
		w.write("Inbox/b.md", "No front matter.\n")
		w.commit("Second")
		return w
	}
	good := base()
	limits := testLimits(t)
	cases := []struct {
		name   string
		bundle func() []byte
		limits func(Limits) Limits
		want   string
	}{
		{name: "upload over the limit", bundle: func() []byte { return good.bundle("--all") },
			limits: func(l Limits) Limits { l.BundleBytes = 200; return l }, want: ErrBundleTooLarge.Error()},
		{name: "not a bundle", bundle: func() []byte { return []byte("# v2 git bundle\nnonsense\n") }, want: "choose a complete Git bundle"},
		{name: "truncated", bundle: func() []byte { b := good.bundle("--all"); return b[:len(b)-30] }, want: "damaged or incomplete Git history"},
		{name: "incremental", bundle: func() []byte { return good.bundle("HEAD~1..main", "HEAD") }, want: "choose a complete Git bundle"},
		{name: "without HEAD", bundle: func() []byte { return good.bundle("main") }, want: "the bundle must include HEAD"},
		{name: "unexpected reference", bundle: func() []byte {
			w := base()
			git(t, w.dir, "update-ref", "refs/notes/commits", "HEAD")
			return w.bundle("--all")
		}, want: "unsupported Git references"},
		{name: "invalid objects", bundle: func() []byte {
			w := base()
			bad := "tree " + git(t, w.dir, "rev-parse", "HEAD^{tree}") + "\nparent " + git(t, w.dir, "rev-parse", "HEAD") +
				"\nauthor No Email 1700000000 +0000\ncommitter Tester <tester@example.com> 1700000000 +0000\n\nBroken\n"
			cmd := filepath.Join(t.TempDir(), "commit")
			os.WriteFile(cmd, []byte(bad), 0o600)
			id := git(t, w.dir, "hash-object", "--literally", "-t", "commit", "-w", cmd)
			git(t, w.dir, "update-ref", "refs/heads/main", id)
			return w.bundle("--all")
		}, want: "cannot import notebook bundle"},
		{name: "history over the byte limit", bundle: func() []byte { return good.bundle("--all") },
			limits: func(l Limits) Limits { l.ObjectBytes = 100; return l }, want: "history exceeds"},
		{name: "history over the object limit", bundle: func() []byte { return good.bundle("--all") },
			limits: func(l Limits) Limits { l.Objects = 5; return l }, want: "history exceeds"},
		{name: "symlink", bundle: func() []byte {
			w := base()
			os.Symlink("Inbox/a.md", filepath.Join(w.dir, "link.md"))
			w.commit("Link")
			return w.bundle("--all")
		}, want: "symlinks and submodules are not supported"},
		{name: "submodule", bundle: func() []byte {
			w := base()
			git(t, w.dir, "update-index", "--add", "--cacheinfo", "160000,0123456789012345678901234567890123456789,vendor")
			w.commitIndex("Module")
			return w.bundle("--all")
		}, want: "symlinks and submodules are not supported"},
		{name: "file over its budget", bundle: func() []byte { return good.bundle("--all") },
			limits: func(l Limits) Limits { l.FileBytes = func(string) int64 { return 5 }; return l }, want: "exceeds its configured file"},
		{name: "repository file", bundle: func() []byte {
			w := base()
			w.write(".gitattributes", "*.md filter=evil\n")
			w.commit("Attributes")
			return w.bundle("--all")
		}, want: "unsupported notebook file: .gitattributes"},
		{name: "hidden directory", bundle: func() []byte {
			w := base()
			w.write("assets/.cache/x.png", "x")
			w.commit("Hidden")
			return w.bundle("--all")
		}, want: "unsupported notebook file: assets/.cache/x.png"},
		{name: "duplicate fmn-id", bundle: func() []byte {
			w := base()
			w.write("Inbox/copy.md", note("0123456789abcdef0123456789abcdef", "Copy", "Same identity.\n"))
			w.commit("Copy")
			return w.bundle("--all")
		}, want: "duplicate fmn-id in Inbox/copy.md"},
		{name: "malformed fmn-id", bundle: func() []byte {
			w := base()
			w.write("Inbox/bad.md", "---\nfmn-id: NOT-AN-ID\n---\n")
			w.commit("Bad")
			return w.bundle("--all")
		}, want: "invalid fmn-id in Inbox/bad.md"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := limits
			if c.limits != nil {
				l = c.limits(l)
			}
			n := e.store.Notebook("refused-" + strconv.Itoa(i))
			_, err := ImportBundle(e.ctx, n, bytes.NewReader(c.bundle()), l)
			if !errors.Is(err, ErrBundle) && !errors.Is(err, ErrBundleTooLarge) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error with %q", err, c.want)
			}
			if _, err := n.Head(e.ctx); !errors.Is(err, fs.ErrNotExist) {
				t.Fatal("a refused bundle stored history:", err)
			}
		})
	}
	// The bundle the refusals started from imports, but only once.
	n := e.store.Notebook("accepted")
	head, err := ImportBundle(e.ctx, n, bytes.NewReader(good.bundle("--all")), limits)
	if err != nil || head.Commit.String() != git(t, good.dir, "rev-parse", "HEAD") {
		t.Fatal(head, err)
	}
	if _, err := ImportBundle(e.ctx, n, bytes.NewReader(good.bundle("--all")), limits); !errors.Is(err, ErrNotEmpty) {
		t.Fatal("a second import into one notebook:", err)
	}
	if _, err := ImportBundle(e.ctx, e.store.Notebook("unchecked"), bytes.NewReader(good.bundle("--all")), Limits{}); err == nil {
		t.Fatal("an import without the notebook file checks")
	}
	if entries, _ := os.ReadDir(limits.Staging); len(entries) != 0 {
		t.Fatal("staging left behind:", len(entries))
	}
}

// The Git check of a bundle has time in proportion to the bundle, and
// running out of it, the check's own time or the caller's, is a timeout the
// error says is no sign of damage, never a refused bundle: a fetch that a
// slow disk kept from finishing once failed as "damaged or incomplete Git
// history".
func TestBundleCheckTimeIsATimeoutAndGrowsWithTheBundle(t *testing.T) {
	e := newEnv(t)
	w := newWorkTree(t)
	w.write("Inbox/a.md", note("0123456789abcdef0123456789abcdef", "A", "A note.\n"))
	w.commit("First")
	raw := w.bundle("--all")
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	defer func(timeout time.Duration, rate int64) { checkTimeout, checkRate = timeout, rate }(checkTimeout, checkRate)
	// Git whose fetch never finishes, as on a stalled disk.
	stalled := func(t *testing.T, ctx context.Context, notebook, want string) {
		shim, fetched := t.TempDir(), filepath.Join(t.TempDir(), "fetched")
		script := "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = fetch ]; then : >" + fetched + "; sleep 60; fi; done\nexec " + real + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
		limits := testLimits(t)
		n := e.store.Notebook(notebook)
		started := time.Now()
		_, err := ImportBundle(ctx, n, bytes.NewReader(raw), limits)
		if !errors.Is(err, ErrBundleTimeout) || errors.Is(err, ErrBundle) || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "does not show that the bundle is damaged") {
			t.Fatalf("a check that ran out of time: %v", err)
		}
		if _, err := os.Stat(fetched); err != nil || time.Since(started) > 30*time.Second {
			t.Fatal("the timeout did not stop the fetch:", err, time.Since(started))
		}
		if _, err := n.Head(e.ctx); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("a bundle whose check timed out stored history:", err)
		}
		if entries, _ := os.ReadDir(limits.Staging); len(entries) != 0 {
			t.Fatal("staging left behind:", len(entries))
		}
	}
	t.Run("own_time", func(t *testing.T) {
		checkTimeout, checkRate = 2*time.Second, 1<<40
		stalled(t, e.ctx, "slow", "timed out after 2s")
	})
	t.Run("callers_deadline", func(t *testing.T) {
		checkTimeout = time.Hour
		ctx, cancel := context.WithTimeout(e.ctx, 2*time.Second)
		defer cancel()
		stalled(t, ctx, "late", "the import ran out of time")
	})

	// With almost no time of its own, the check has what the bundle's size
	// earns it: a minute for this one.
	checkTimeout, checkRate = time.Nanosecond, int64(len(raw))/60
	if limit := checkLimit(int64(len(raw))); limit < time.Minute || limit > 70*time.Second {
		t.Fatal("the bundle's time:", limit)
	}
	head, err := ImportBundle(e.ctx, e.store.Notebook("timely"), bytes.NewReader(raw), testLimits(t))
	if err != nil || head.Commit.String() != git(t, w.dir, "rev-parse", "HEAD") {
		t.Fatal(head, err)
	}
}

// A notebook created between an import's check that it is empty and its
// first commit is not the import's: the import stops at that commit,
// leaving the rest of its history out, rather than writing all of it into
// someone else's notebook.
func TestImportGitDirStopsWhenTheNotebookWasCreatedMeanwhile(t *testing.T) {
	e := newEnv(t)
	w := newWorkTree(t)
	w.write("Inbox/a.md", note("0123456789abcdef0123456789abcdef", "A", "A note.\n"))
	w.commit("First")
	w.write("Inbox/b.md", note("0123456789abcdef0123456789abcdee", "B", "B note.\n"))
	w.commit("Second")
	w.write("Inbox/c.md", note("0123456789abcdef0123456789abcded", "C", "C note.\n"))
	w.commit("Third")
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// Git whose rev-parse, which the import runs once it found the notebook
	// empty, waits while the notebook is created.
	shim, signals := t.TempDir(), t.TempDir()
	began, released := filepath.Join(signals, "began"), filepath.Join(signals, "released")
	script := "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = rev-parse ]; then : >" + began + "; while [ ! -e " + released + " ]; do sleep 0.05; done; fi; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	nb := e.store.Notebook("taken")
	done := make(chan error, 1)
	go func() {
		_, err := ImportGitDir(e.ctx, nb, filepath.Join(w.dir, ".git"))
		done <- err
	}()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(began); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the import never looked up HEAD")
		}
	}
	e.create("taken")
	apply(t, nb, notestore.ChangeSet{Subject: "Plan", Author: ada, Files: []notestore.FileChange{write("plan.md", "# Plan\n")}})
	before, err := nb.Head(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(released, nil, 0o600)
	if err := <-done; !errors.Is(err, ErrNotEmpty) {
		t.Fatal("an import into a notebook created meanwhile:", err)
	}
	if head, err := nb.Head(e.ctx); err != nil || head.Seq > before.Seq+1 {
		t.Fatalf("the import went on past its first commit: %v, the notebook was at %v: %v", head, before, err)
	}
}
