package reposource

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRetainedEvidenceSurvivesCacheEvictionAndRestore(t *testing.T) {
	upstreamDir, sha := upstream(t)
	c := &cache{dir: t.TempDir(), limits: Limits{History: 1, CacheBytes: 8 << 20, EvidenceBytes: 1 << 20, FileBytes: 1 << 20, Files: 100, Seconds: 10}, allowFile: true, remoteURL: func(Repository) string { return "file://" + upstreamDir }}
	repo := Repository{ID: 1, FullName: "example/source", DefaultBranch: "main"}
	ctx := context.Background()
	if err := c.refresh(ctx, repo, sha, ""); err != nil {
		t.Fatal(err)
	}
	f, err := c.file(ctx, repo, sha, "README.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.retain(ctx, repo, f, 0); err != nil {
		t.Fatal(err)
	}
	before, err := diskBytes(filepath.Join(c.dir, "evidence"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.retain(ctx, repo, f, 0); err != nil {
		t.Fatal(err)
	}
	after, _ := diskBytes(filepath.Join(c.dir, "evidence"))
	if before != after {
		t.Fatal("duplicate evidence grew storage")
	}
	restored := t.TempDir()
	if err = os.CopyFS(filepath.Join(restored, "evidence"), os.DirFS(filepath.Join(c.dir, "evidence"))); err != nil {
		t.Fatal(err)
	}
	c.dir = restored
	got, err := c.file(ctx, repo, sha, "README.md")
	if err != nil || !got.Retained || got.Content != f.Content || got.Hash != f.Hash {
		t.Fatalf("restored evidence: %+v %v", got, err)
	}
	repo.FullName = "example/renamed-source"
	got, err = c.file(ctx, repo, sha, "README.md")
	if err != nil || got.URL != f.URL {
		t.Fatalf("renaming the repository rewrote the original evidence URL: %s %v", got.URL, err)
	}
	if _, err = c.retain(ctx, repo, got, 0); err != nil {
		t.Fatalf("renaming the repository prevented evidence reuse: %v", err)
	}
	if revision, path := c.citation(ctx, repo, f.URL+"#L1-L2"); revision != sha || path != f.Path {
		t.Fatal("old citation no longer resolves after a repository rename")
	}
	if revision, _ := c.citation(ctx, repo, "https://github.com/unrelated/source/blob/"+sha+"/README.md"); revision != "" {
		t.Fatal("a citation to an unrelated repository resolved to private evidence")
	}
	if _, err = c.file(ctx, repo, sha, "uncaptured.go"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("uncaptured: %v", err)
	}
	if _, err = c.file(ctx, repo, sha, "../README.md"); !errors.Is(err, ErrDenied) {
		t.Fatalf("traversal: %v", err)
	}
}
