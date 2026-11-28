package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"forgetmenote/internal/capture"
	"forgetmenote/internal/catalog"
)

// admin capture exits 2 for a call it cannot run, before it touches the
// instance, and names the usage.
func TestAdminCaptureRefusesAWrongCall(t *testing.T) {
	t.Setenv("INSTANCE_CONFIG_FILE", "")
	t.Setenv("DATABASE_URL_FILE", "")
	t.Setenv("DATABASE_URL", "")
	existing := t.TempDir()
	for _, args := range [][]string{
		{},
		{"-data", t.TempDir()},
		{"-out", filepath.Join(t.TempDir(), "capture"), "extra"},
		{"-out", existing},
		{"-unknown"},
	} {
		var stdout, stderr bytes.Buffer
		if code := runCaptureAdmin(args, strings.NewReader("done\n"), &stdout, &stderr); code != 2 || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("admin capture %q: exit %d, %q, %q", args, code, stdout.String(), stderr.String())
		}
	}
	if entries, err := os.ReadDir(existing); err != nil || len(entries) != 0 {
		t.Fatal("admin capture wrote into an existing directory:", entries, err)
	}
}

// admin capture on a running instance prints its snapshot line, waits for
// done, prints ok and exits 0; when its input ends first, it exits 1 with a
// short reason and leaves the directory for the caller.
func TestAdminCaptureSpeaksTheBackupProtocol(t *testing.T) {
	t.Setenv("INSTANCE_CONFIG_FILE", "")
	db := instanceDatabase(t, databaseURLFile(t, true))
	data := t.TempDir()
	c, err := catalog.Open(data, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	store, err := c.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	book := "# Book\n\n" + strings.Repeat("A paragraph of a long manuscript, kept in blocks.\n\n", 400)
	if _, err := store.Save(t.Context(), "book.md", []byte(book), "", "test"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "custom.css"), []byte("body { color: teal; }\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "capture")
	var stdout, stderr bytes.Buffer
	if code := runCaptureAdmin([]string{"-data", data, "-out", out}, strings.NewReader("done\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("admin capture: exit %d: %s", code, stderr.String())
	}
	if !regexp.MustCompile(`^snapshot [0-9A-F]+-[0-9A-F]+-[0-9]+ purge -\nok\n$`).MatchString(stdout.String()) || stderr.Len() != 0 {
		t.Fatalf("admin capture printed %q and %q", stdout.String(), stderr.String())
	}
	raw, err := os.ReadFile(filepath.Join(out, "capture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m capture.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Format != 1 || m.Purge != "-" || m.Blocks == 0 || !strings.HasPrefix(m.Version, "forgetmenote ") || !strings.Contains(stdout.String(), "snapshot "+m.Snapshot+" ") {
		t.Fatalf("capture.json: %s", raw)
	}
	if got, err := os.ReadFile(filepath.Join(out, "data", "custom.css")); err != nil || string(got) != "body { color: teal; }\n" {
		t.Fatal("the capture's custom.css:", err)
	}
	if st, err := os.Stat(out); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatal("the capture directory is not private:", err)
	}

	abandoned := filepath.Join(t.TempDir(), "capture")
	stdout.Reset()
	stderr.Reset()
	if code := runCaptureAdmin([]string{"-data", data, "-out", abandoned}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Fatalf("admin capture without done: exit %d", code)
	}
	if !strings.HasPrefix(stdout.String(), "snapshot ") || strings.Contains(stdout.String(), "ok") || !strings.Contains(stderr.String(), "capture abandoned") || strings.Count(stderr.String(), "\n") != 1 {
		t.Fatalf("admin capture without done printed %q and %q", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(abandoned, "capture.json")); err != nil {
		t.Fatal("an abandoned capture removed its directory:", err)
	}
	// The abandoned capture released the blocks: the next one runs.
	stdout.Reset()
	if code := runCaptureAdmin([]string{"-data", data, "-out", filepath.Join(t.TempDir(), "capture")}, strings.NewReader("done\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("admin capture after an abandoned one: exit %d: %s", code, stderr.String())
	}
}
