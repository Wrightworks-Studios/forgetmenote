package gitexport

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// stoppedPurge exports a notebook with a leaked key, then rewrites it as a
// purge does, without rebuilding the export, as a purge whose process
// stopped in between leaves it: it returns the export's old head and the
// purge's rewritten head.
func stoppedPurge(t *testing.T, e *env) (old gitobj.ID, rewritten notestore.Head) {
	t.Helper()
	n := e.create("notes")
	apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("Inbox/keys.md", "The key is "+secret+".\n")}})
	apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("Inbox/after.md", "Written after the leak.\n")}})
	head, err := e.ex.Sync(e.ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	redact := func(f notestore.RewriteFile, content func() ([]byte, error)) ([]byte, bool, error) {
		data, err := content()
		return bytes.ReplaceAll(data, []byte(secret), []byte("[purged]")), true, err
	}
	result, err := n.Rewrite(e.ctx, 1, "purge-1", redact, notestore.RewriteOptions{Actor: "owner", Reason: "a leaked key"})
	if err != nil {
		t.Fatal(err)
	}
	return head.Commit, result.Head
}

func checkFinishedPurge(t *testing.T, e *env, old gitobj.ID, rewritten notestore.Head) {
	t.Helper()
	repo := e.ex.Path("notes")
	if refOf(t, repo) != rewritten.Commit {
		t.Fatal("the branch is not the rewritten head")
	}
	flag, ok, err := ReadPurge(repo)
	if err != nil || !ok || flag.Purge != "purge-1" || flag.Head != rewritten.Commit || !slices.Contains(flag.Replaces, old) {
		t.Fatalf("purge flag %+v %v %v, want it to replace %s", flag, ok, err, old)
	}
	if all := git(t, repo, "cat-file", "--batch-all-objects", "--unordered", "--batch"); strings.Contains(all, secret) {
		t.Fatal("the export still holds purged content")
	}
}

// A purge that rewrote a notebook and stopped before it rebuilt the export
// left the export holding the old history, which no export moves past. The
// next export of the notebook, on the exporter's catch-up or for a backup,
// finishes it: it rebuilds the export for that purge and flags it, so the
// backup worker replaces the remote's old history once. While this process
// runs a purge of the notebook, the purge's own rebuild does that.
func TestExportFinishesAPurgeThatStoppedBeforeItsRebuild(t *testing.T) {
	e := newEnv(t)
	end := e.ex.Purging("notes")
	old, rewritten := stoppedPurge(t, e)
	if _, err := e.ex.Sync(e.ctx, "notes"); !errors.Is(err, ErrDiverged) {
		t.Fatal("an export of a notebook this process is purging:", err)
	}
	if _, ok, _ := ReadPurge(e.ex.Path("notes")); ok || refOf(t, e.ex.Path("notes")) != old {
		t.Fatal("an export rebuilt a notebook this process is purging")
	}
	end()
	head, err := e.ex.Sync(e.ctx, "notes")
	if err != nil || head.Commit != rewritten.Commit {
		t.Fatal(head, err)
	}
	checkFinishedPurge(t, e, old, rewritten)
}

// An operator who recreates such an export gets it rebuilt for the purge,
// with its flag, rather than a new export without one, which would leave
// the remote's old history blocking every push as diverged.
func TestRecreateFinishesAPurgeThatStoppedBeforeItsRebuild(t *testing.T) {
	e := newEnv(t)
	old, rewritten := stoppedPurge(t, e)
	head, err := e.ex.Recreate(e.ctx, "notes")
	if err != nil || head.Commit != rewritten.Commit {
		t.Fatal(head, err)
	}
	checkFinishedPurge(t, e, old, rewritten)
}

// An export that a purge rewrote and that is too damaged for the purge's
// rebuild in place, as one an operator recreates may be, is recreated
// anew for that purge: the new export holds none of the purged content, and
// its flag still lets the backup worker replace the old branch once.
func TestRecreateRebuildsADamagedExportForAPurge(t *testing.T) {
	e := newEnv(t)
	old, rewritten := stoppedPurge(t, e)
	// A directory stands where the export's purge flag goes, so no rebuild
	// in place can flag it.
	if err := os.Mkdir(filepath.Join(e.ex.Path("notes"), PurgeFlag), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ex.Sync(e.ctx, "notes"); err == nil {
		t.Fatal("an export the purge's rebuild cannot repair synced")
	}
	head, err := e.ex.Recreate(e.ctx, "notes")
	if err != nil || head.Commit != rewritten.Commit {
		t.Fatal(head, err)
	}
	checkFinishedPurge(t, e, old, rewritten)
}

// A damaged export recreated anew for a purge flags every commit the
// purge rewrote, not only the old export's branch: the backup worker
// pushes on its interval, so the GitHub copy may be at a commit before the
// branch, here A while the export reached B, and it is replaced all the
// same, as the rebuild in place would.
func TestRecreateAnewFlagsEveryCommitThePurgeRewrote(t *testing.T) {
	e := newEnv(t)
	n := e.create("notes")
	apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("Inbox/keys.md", "The key is "+secret+".\n")}})
	pushed, err := e.ex.Sync(e.ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	apply(t, n, notestore.ChangeSet{Files: []notestore.FileChange{write("Inbox/after.md", "Written after the leak.\n")}})
	exported, err := e.ex.Sync(e.ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	redact := func(f notestore.RewriteFile, content func() ([]byte, error)) ([]byte, bool, error) {
		data, err := content()
		return bytes.ReplaceAll(data, []byte(secret), []byte("[purged]")), true, err
	}
	result, err := n.Rewrite(e.ctx, 1, "purge-1", redact, notestore.RewriteOptions{Actor: "owner", Reason: "a leaked key"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(e.ex.Path("notes"), PurgeFlag), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ex.Recreate(e.ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	checkFinishedPurge(t, e, exported.Commit, result.Head)
	if flag, _, _ := ReadPurge(e.ex.Path("notes")); !slices.Contains(flag.Replaces, pushed.Commit) {
		t.Fatalf("the flag %+v does not replace a GitHub copy at %s, before the export's branch", flag, pushed.Commit)
	}
}

// A recreation anew whose rebuild fails, as on a full disk, leaves the old
// export and its flag as they were, so a later recreation still flags what
// it replaces; it leaves no partial repository behind.
func TestRecreateAnewThatFailsKeepsTheOldExport(t *testing.T) {
	e := newEnv(t)
	old, rewritten := stoppedPurge(t, e)
	repo := e.ex.Path("notes")
	earlier := Purge{Purge: "purge-0", Head: old, Replaces: []gitobj.ID{mustID(t, strings.Repeat("ab", 20))}}
	if err := writePurge(repo, earlier); err != nil {
		t.Fatal(err)
	}
	before := objectFiles(t, repo)
	// Git indexes no pack, neither in place nor anew.
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = index-pack ]; then echo 'No space left on device' >&2; exit 1; fi; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("PATH")
	t.Setenv("PATH", shim+string(os.PathListSeparator)+path)
	if _, err := e.ex.Recreate(e.ctx, "notes"); err == nil {
		t.Fatal("a recreation whose rebuild failed succeeded")
	}
	if refOf(t, repo) != old {
		t.Fatal("the failed recreation moved the old export's branch")
	}
	if flag, ok, err := ReadPurge(repo); err != nil || !ok || !slices.Equal(flag.Replaces, earlier.Replaces) {
		t.Fatalf("the old export's flag after a failed recreation: %+v %v %v", flag, ok, err)
	}
	for name := range before {
		if _, err := os.Stat(filepath.Join(repo, "objects", name)); err != nil {
			t.Fatalf("the failed recreation removed the old export's %s: %v", name, err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(repo))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".notes") {
			t.Fatalf("the failed recreation left %s", entry.Name())
		}
	}
	t.Setenv("PATH", path)
	if _, err := e.ex.Recreate(e.ctx, "notes"); err != nil {
		t.Fatal(err)
	}
	checkFinishedPurge(t, e, old, rewritten)
	if flag, _, _ := ReadPurge(repo); !slices.Contains(flag.Replaces, earlier.Replaces[0]) {
		t.Fatalf("the flag %+v lost what the old flag replaced", flag)
	}
}
