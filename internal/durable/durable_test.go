package durable

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileReplacesAndSyncsDirectoryAfterRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	synced := []string{}
	err := WriteFile(path, ".state-", []byte("new"), func(d string) error {
		if raw, err := os.ReadFile(path); err != nil || string(raw) != "new" {
			t.Error("directory synced before the rename:", err)
		}
		synced = append(synced, d)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(synced) != 1 || synced[0] != dir {
		t.Fatalf("directory syncs = %q", synced)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("replacement mode:", info, err)
	}
	failure := errors.New("injected directory sync failure")
	if err := WriteFile(path, ".state-", []byte("next"), func(string) error { return failure }); !errors.Is(err, failure) {
		t.Fatal("directory sync failure was not reported:", err)
	}
	if err := WriteFile(path, ".state-", []byte("default"), nil); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "default" {
		t.Fatalf("content = %q", raw)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary files remain: %v", entries)
	}
}

func TestWriteFileLeavesTargetAndNoTemporaryOnFailedRename(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "occupied")
	if err := os.MkdirAll(filepath.Join(target, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	called := false
	err := WriteFile(target, ".prefix-", []byte("x"), func(string) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("rename over a non-empty directory succeeded or synced:", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".prefix-") {
			t.Fatal("temporary file remains:", entry.Name())
		}
	}
	if err := WriteFile(filepath.Join(dir, "missing", "file"), ".prefix-", nil, nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing directory:", err)
	}
}

func TestMkdirAllSyncsTheParentOfEveryDirectoryItCreates(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "a"), 0700); err != nil {
		t.Fatal(err)
	}
	var synced []string
	record := func(dir string) error { synced = append(synced, dir); return nil }
	target := filepath.Join(base, "a", "b", "c")
	if err := MkdirAll(target, 0750, record); err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(base, "a"), filepath.Join(base, "a", "b")}
	if strings.Join(synced, "|") != strings.Join(want, "|") {
		t.Fatalf("synced %q, want %q", synced, want)
	}
	// Like os.Mkdir, MkdirAll creates with perm less the process umask, which
	// is 077 under the release gate, so compare with what os.Mkdir creates.
	if err := os.Mkdir(filepath.Join(base, "reference"), 0750); err != nil {
		t.Fatal(err)
	}
	reference, err := os.Stat(filepath.Join(base, "reference"))
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() || info.Mode().Perm() != reference.Mode().Perm() {
		t.Fatal("created directory:", info, err, "want permissions", reference.Mode().Perm())
	}
	synced = nil
	if err := MkdirAll(target, 0750, record); err != nil || len(synced) != 0 {
		t.Fatal("an existing directory was created again:", synced, err)
	}
	failure := errors.New("injected directory sync failure")
	if err := MkdirAll(filepath.Join(base, "x", "y"), 0700, func(string) error { return failure }); !errors.Is(err, failure) {
		t.Fatal("directory sync failure was not reported:", err)
	}
	if err := os.WriteFile(filepath.Join(base, "file"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := MkdirAll(filepath.Join(base, "file", "child"), 0700, nil); err == nil {
		t.Fatal("a directory was created under a file")
	}
	if err := MkdirAll(filepath.Join(base, "default", "sync"), 0700, nil); err != nil {
		t.Fatal(err)
	}
}

func TestMkdirAllInStaysInsideItsRoot(t *testing.T) {
	base := t.TempDir()
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := MkdirAllIn(root, "blocks/ab/cd", 0700); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(base, "blocks", "ab", "cd")); err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatal("created directory:", info, err)
	}
	if err := MkdirAllIn(root, "blocks/ab/cd", 0700); err != nil {
		t.Fatal("existing directory:", err)
	}
	if err := MkdirAllIn(root, "../escape", 0700); err == nil {
		t.Fatal("a directory was created outside the root")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(base), "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("escape directory exists:", err)
	}
}
