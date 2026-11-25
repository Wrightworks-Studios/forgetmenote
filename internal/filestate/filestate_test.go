package filestate

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalJSONIgnoresFormattingButNotContent(t *testing.T) {
	a, err := JSON([]byte("{\n  \"b\": 1.50,\n  \"a\": [true, null]\n}\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := JSON([]byte(`{"a":[true,null],"b":1.50}`))
	if string(a) != string(b) || string(a) != `{"a":[true,null],"b":1.50}` {
		t.Fatalf("canonical forms differ: %s %s", a, b)
	}
	if _, err = JSON([]byte(`{} {}`)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	var first, second Tally
	first.Add("x/a.json", a)
	first.Add("b.json", []byte("b"))
	second.Add("b.json", []byte("b"))
	second.Add("x/a.json", b)
	if first.Sum() != second.Sum() || first.Sum().Files != 2 {
		t.Fatalf("equal files summed differently: %+v %+v", first.Sum(), second.Sum())
	}
	second.Add("b.json", []byte("c"))
	if first.Sum() == second.Sum() {
		t.Fatal("changed content kept its digest")
	}
}

func TestSourceStaysInsideItsDirectory(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("outside"), 0600)
	data := t.TempDir()
	os.Symlink(outside, filepath.Join(data, "linked.json"))
	src, err := OpenSource(data)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err = src.Read("linked.json"); err == nil {
		t.Fatal("a symbolic link was followed")
	}
	if _, err = src.Read("../secret"); err == nil {
		t.Fatal("a path left the directory")
	}
	if _, err = src.Read("missing.json"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing file: %v", err)
	}
	if _, err = OpenSource(data); err == nil {
		t.Fatal("two processes held one data directory")
	}
}

func TestTargetNeverReplaces(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "export")
	out, err := CreateTarget(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err = out.Write("a/b.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if err = out.Write("a/b.json", []byte("[]")); err == nil {
		t.Fatal("an export replaced a file")
	}
	if info, err := os.Stat(filepath.Join(dir, "a", "b.json")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("exported file is not private", info, err)
	}
	if _, err = CreateTarget(dir); err == nil {
		t.Fatal("an export reused a directory")
	}
}

// A cutover reads a stopped volume mounted read-only, so the release it came
// from can restart on it untouched; the lock still refuses a running process.
func TestSourceReadsADirectoryItCannotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through file modes")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "catalog.json"), []byte("{}"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "catalog.lock"), nil, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0700) })
	source, err := OpenSource(dir)
	if err != nil {
		t.Fatal("a read-only data directory was refused:", err)
	}
	raw, err := source.Read("catalog.json")
	if err != nil || string(raw) != "{}" {
		t.Fatalf("read %q, %v", raw, err)
	}
	if _, err = OpenSource(dir); err == nil {
		t.Fatal("a second reader took the lock the first holds")
	}
	source.Close()
}
