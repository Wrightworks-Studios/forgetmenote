package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

// repositoryHead reads HEAD as Git resolves it: a branch's loose ref, the
// same branch once its refs are packed, a detached HEAD, and nothing for a
// branch with no commit yet; it refuses what is not a .git directory.
func TestRepositoryHeadResolvesHEADAsGitDoes(t *testing.T) {
	work := t.TempDir()
	testGit(t, work, "init", "--quiet", "--initial-branch=main")
	testGit(t, work, "commit", "--quiet", "--allow-empty", "-m", "First")
	first := testGit(t, work, "rev-parse", "HEAD")
	testGit(t, work, "commit", "--quiet", "--allow-empty", "-m", "Second")
	second := testGit(t, work, "rev-parse", "HEAD")
	gitDir := filepath.Join(work, ".git")
	want := func(state, commit string) {
		t.Helper()
		head, ok, err := repositoryHead(gitDir)
		if err != nil || !ok || head.String() != commit {
			t.Fatalf("%s: HEAD %v %v %v, want %s", state, head, ok, err, commit)
		}
	}
	want("a loose branch", second)
	testGit(t, work, "tag", "-a", "-m", "Release", "v1", first)
	testGit(t, work, "pack-refs", "--all")
	if _, err := os.Stat(filepath.Join(gitDir, "refs", "heads", "main")); !os.IsNotExist(err) {
		t.Fatal("the branch is still a loose ref:", err)
	}
	want("packed refs", second)
	testGit(t, work, "checkout", "--quiet", "--detach", first)
	want("a detached HEAD", first)
	testGit(t, work, "checkout", "--quiet", "--orphan", "empty")
	if head, ok, err := repositoryHead(gitDir); err != nil || ok {
		t.Fatalf("a branch with no commit: %v %v %v", head, ok, err)
	}
	linked := filepath.Join(t.TempDir(), ".git")
	if err := os.WriteFile(linked, []byte("gitdir: "+gitDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repositoryHead(linked); err == nil {
		t.Fatal("read the HEAD of a .git file")
	}
}
