package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/benchfixture"
)

func runFixture(t *testing.T, args ...string) summary {
	t.Helper()
	var out bytes.Buffer
	if err := run(t.Context(), args, &out); err != nil {
		t.Fatal(err)
	}
	var s summary
	if err := json.Unmarshal(out.Bytes(), &s); err != nil {
		t.Fatal(err, out.String())
	}
	return s
}

func TestMaterializesNotebookAsGitAndFiles(t *testing.T) {
	small := []string{"-fixture", "f2", "-notes", "30", "-assets", "2", "-revisions", "200", "-min-asset-bytes", "2048", "-max-asset-bytes", "8192"}
	repo := filepath.Join(t.TempDir(), "repo")
	s := runFixture(t, append(small, "-out", repo)...)
	head, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil || s.Revisions != 200 || strings.TrimSpace(string(head)) != s.Head {
		t.Fatalf("summary %+v, git HEAD %s %v", s, head, err)
	}
	files := filepath.Join(t.TempDir(), "files")
	if s = runFixture(t, append(small, "-format", "files", "-out", files)...); s.Revisions != 200 {
		t.Fatalf("summary %+v", s)
	}
	tracked, err := exec.Command("git", "-C", repo, "ls-files", "-z").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range strings.Split(strings.TrimSuffix(string(tracked), "\x00"), "\x00") {
		a, errA := os.ReadFile(filepath.Join(repo, p))
		b, errB := os.ReadFile(filepath.Join(files, p))
		if errA != nil || errB != nil || !bytes.Equal(a, b) {
			t.Fatalf("%s differs between the repository and the plain files: %v %v", p, errA, errB)
		}
	}
	if err = run(t.Context(), append(small, "-out", repo), &bytes.Buffer{}); err == nil {
		t.Fatal("overwrote an existing fixture")
	}
	if err = run(t.Context(), []string{"-fixture", "f9", "-out", t.TempDir()}, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted an unknown fixture")
	}
}

func TestMaterializesNovelFixtures(t *testing.T) {
	if _, err := benchfixture.Novel(t.Context()); errors.Is(err, benchfixture.ErrUnavailable) {
		t.Skip(err)
	}
	dir := t.TempDir()
	if s := runFixture(t, "-fixture", "f1", "-out", filepath.Join(dir, "f1")); s.Files != 1 {
		t.Fatalf("f1 summary %+v", s)
	}
	if s := runFixture(t, "-fixture", "f1h", "-edits", "20", "-format", "bare", "-out", filepath.Join(dir, "f1h.git")); s.Revisions != 22 || s.Head == "" {
		t.Fatalf("f1h summary %+v", s)
	}
	if s := runFixture(t, "-fixture", "t1", "-out", filepath.Join(dir, "t1")); s.Files != 7 {
		t.Fatalf("t1 summary %+v", s)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "t1", "t1-typing-start.json"))
	var trace benchfixture.Trace
	if err != nil || json.Unmarshal(raw, &trace) != nil || trace.FixtureSHA256 != benchfixture.NovelSHA256 || len(trace.Events) < 1000 {
		t.Fatalf("typing trace: %v, %d events", err, len(trace.Events))
	}
}
