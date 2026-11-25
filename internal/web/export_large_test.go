package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/workspace"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in integration measurement; it uses ~400 MiB of disposable disk.
func TestLargeExportSnapshot(t *testing.T) {
	if os.Getenv("FMN_LARGE_EXPORT_TEST") != "1" {
		t.Skip("set FMN_LARGE_EXPORT_TEST=1 for 100 MiB export/restore")
	}
	app, store := fixture(t)
	ctx := context.Background()
	originals := map[string][]byte{}
	var files []notestore.FileChange
	for i := 0; i < 4; i++ {
		data := make([]byte, 25<<20)
		if _, err := rand.Read(data); err != nil {
			t.Fatal(err)
		}
		name := filepath.Join("assets", string(rune('a'+i))+".bin")
		originals[name] = data
		files = append(files, notestore.FileChange{Path: name, Content: data})
	}
	commitElsewhere(t, app, store, notestore.ChangeSet{Subject: "Large export fixture", Author: fixtureAuthor, Files: files})
	start := time.Now()
	first, err := app.backupChunk(ctx, store, "default", 0, 262144, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Total < 100<<20 {
		t.Fatal("fixture unexpectedly compressed", first.Total)
	}
	if _, err = store.Save(ctx, "later.md", []byte("after snapshot"), "", ""); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "export.bundle")
	out, err := os.Create(bundle)
	if err != nil {
		t.Fatal(err)
	}
	firstBytes, _ := base64.StdEncoding.DecodeString(first.Data)
	out.Write(firstBytes)
	for offset := first.NextOffset; offset < first.Total; {
		part, err := app.backupChunk(ctx, store, "default", offset, 262144, first.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := base64.StdEncoding.DecodeString(part.Data)
		out.Write(raw)
		offset = part.NextOffset
	}
	out.Close()
	raw, err := os.ReadFile(bundle)
	if err != nil || workspace.ContentHash(raw) != first.SHA256 {
		t.Fatal("hash mismatch", err)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	if output, err := exec.Command("git", "clone", bundle, clone).CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	if _, err = os.Stat(filepath.Join(clone, "later.md")); !os.IsNotExist(err) {
		t.Fatal("snapshot includes later save")
	}
	for name, original := range originals {
		restored, _ := os.ReadFile(filepath.Join(clone, name))
		if workspace.ContentHash(original) != workspace.ContentHash(restored) {
			t.Fatal("restored bytes differ", name)
		}
	}
	t.Logf("100 MiB snapshot export with concurrent later save, checksum and independent restore: %s", time.Since(start))
}
