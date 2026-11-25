package reposource

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func upstream(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-b", "main")
	gitRun(t, dir, "config", "user.email", "fixture@example.invalid")
	gitRun(t, dir, "config", "user.name", "Fixture")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Sample\nA documented system.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "README.md")
	gitRun(t, dir, "commit", "-m", "Initial")
	return dir, gitRun(t, dir, "rev-parse", "HEAD")
}
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git fixture: %v %s", err, out)
	}
	return strings.TrimSpace(string(out))
}
func TestSourceCacheFetchReadsExactCommitWithoutChangingUpstream(t *testing.T) {
	dir, sha := upstream(t)
	c := &cache{dir: t.TempDir(), limits: Limits{History: 5, CacheBytes: 8 << 20, FileBytes: 1 << 20, Files: 100, Seconds: 10}, allowFile: true, remoteURL: func(Repository) string { return "file://" + dir }}
	if err := c.refresh(context.Background(), Repository{ID: 1, FullName: "example/source", DefaultBranch: "main"}, sha, ""); err != nil {
		t.Fatal(err)
	}
	b, err := c.git(context.Background(), filepath.Join(c.dir, "source.git"), "", 1024, "show", sha+":README.md")
	if err != nil || !strings.Contains(string(b), "A documented system.") {
		t.Fatalf("source file: %s %v", b, err)
	}
	if got := gitRun(t, dir, "rev-parse", "HEAD"); got != sha {
		t.Fatal("source HEAD changed")
	}
	if got := gitRun(t, dir, "status", "--porcelain"); got != "" {
		t.Fatalf("source dirty: %s", got)
	}
}

func TestCacheRotationLimitsUnsupportedFilesAndInterruptedRefresh(t *testing.T) {
	dir, first := upstream(t)
	ctx := context.Background()
	repo := Repository{ID: 1, FullName: "example/source", DefaultBranch: "main"}
	c := &cache{dir: t.TempDir(), limits: Limits{History: 2, CacheBytes: 8 << 20, EvidenceBytes: 1 << 20, FileBytes: 4096, Files: 100, Seconds: 10}, allowFile: true, remoteURL: func(Repository) string { return "file://" + dir }}
	if err := c.refresh(ctx, repo, first, ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if err := os.WriteFile(filepath.Join(dir, "change.go"), []byte(fmt.Sprintf("// revision %d\npackage sample\n", i)), 0600); err != nil {
			t.Fatal(err)
		}
		gitRun(t, dir, "add", "change.go")
		gitRun(t, dir, "commit", "-m", "Source change")
	}
	if err := os.Symlink("README.md", filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pointer.txt"), []byte("version https://git-lfs.github.com/spec/v1\noid sha256:"+strings.Repeat("a", 64)+"\nsize 5000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "Unsupported source types")
	latest := gitRun(t, dir, "rev-parse", "HEAD")
	if err := c.refresh(ctx, repo, latest, ""); err != nil {
		t.Fatal(err)
	}
	count, err := c.git(ctx, filepath.Join(c.dir, "source.git"), "", 1024, "rev-list", "--count", "HEAD")
	if err != nil || strings.TrimSpace(string(count)) != "2" {
		t.Fatalf("history unbounded: %s %v", count, err)
	}
	if _, err := c.file(ctx, repo, first, "README.md"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("evicted unretained history: %v", err)
	}
	for _, p := range []string{"link.md", "pointer.txt"} {
		if _, err := c.file(ctx, repo, latest, p); !errors.Is(err, ErrDenied) {
			t.Fatalf("unsupported %s: %v", p, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := c.refresh(canceled, repo, latest, ""); err == nil {
		t.Fatal("canceled fetch succeeded")
	}
	if _, err := c.file(ctx, repo, latest, "README.md"); err != nil {
		t.Fatal("interruption lost prior cache", err)
	}
	bytes := make([]byte, 1<<20)
	if _, err := rand.Read(bytes); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "large.bin"), bytes, 0600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "large.bin")
	gitRun(t, dir, "commit", "-m", "Large source")
	c.limits.CacheBytes = 128 << 10
	if err := c.refresh(ctx, repo, gitRun(t, dir, "rev-parse", "HEAD"), ""); !errors.Is(err, ErrLimit) {
		t.Fatalf("disk quota ignored: %v", err)
	}
	if _, err := c.file(ctx, repo, latest, "README.md"); err != nil {
		t.Fatal("quota failure lost prior cache", err)
	}
}
