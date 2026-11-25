package web

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/workspace"
)

// A process that stopped mid-request leaves what the request was writing in
// its data directory: a browser backup's bundle, a bundle export's before it
// was stored, an import's upload. The next process removes them before
// serving, and leaves the data directory's other files alone, such as the
// bundles an operator wrote there with the backup or notebook export
// command, whatever their names.
func TestRestartRemovesWhatAStoppedProcessWasWriting(t *testing.T) {
	app, _ := fixture(t)
	leftovers := []string{
		filepath.Join("cache", "backups", "backup-123.bundle"),
		filepath.Join("cache", "exports", "bundle-456"),
		filepath.Join(".notebook-import-012", "fmn-bundle-345", "upload.bundle"),
	}
	kept := []string{"backup-notes.bundle", "backup-20261123.bundle", "backup-1.bundle", filepath.Join("notebook-import-012", "upload.bundle")}
	for _, name := range append(leftovers, kept...) {
		path := filepath.Join(app.data, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("# v2 git bundle\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	app.Close()
	again, err := New(app.catalog, app.db, app.data, app.origin, app.token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(again.Close)
	for _, name := range leftovers {
		if _, err := os.Stat(filepath.Join(app.data, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s remains: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(app.data, ".notebook-import-012")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the import's staging directory remains:", err)
	}
	for _, name := range kept {
		if _, err := os.Stat(filepath.Join(app.data, name)); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}
}

// A write a crash stopped leaves its temporary file beside what it was
// replacing: a content block or an export bundle in the blob directory, a
// notebook's knowledge caches. Each may hold notebook content, purged
// content too, so a starting gateway removes them, and so does a purge; a
// temporary a write may still be writing, changed within the last minute,
// stays.
func TestStartupAndPurgeRemoveWhatInterruptedWritesLeft(t *testing.T) {
	app, _ := fixture(t)
	interrupted := []string{
		filepath.Join("blocks", ".blob-0123456789abcdef"),
		filepath.Join("exports", ".blob-fedcba9876543210"),
		filepath.Join("projects", "default", ".knowledge-123"),
		filepath.Join("projects", "default", "cache", ".vectors-456"),
	}
	place := func(suffix string, age time.Duration) []string {
		var paths []string
		for _, name := range interrupted {
			path := filepath.Join(app.data, name+suffix)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("The staging key is sk-live-0000.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, time.Now().Add(-age), time.Now().Add(-age)); err != nil {
				t.Fatal(err)
			}
			paths = append(paths, path)
		}
		return paths
	}
	check := func(when string, removed, kept []string) {
		t.Helper()
		for _, path := range removed {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s: %s remains: %v", when, path, err)
			}
		}
		for _, path := range kept {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("%s: %s, written just now, was removed: %v", when, path, err)
			}
		}
	}
	stale, fresh := place("0", time.Hour), place("1", 0)
	ordinary := filepath.Join(app.data, "projects", "default", "note-map.json")
	if err := os.WriteFile(ordinary, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(ordinary, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	app.Close()
	again, err := New(app.catalog, app.db, app.data, app.origin, app.token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(again.Close)
	check("a starting gateway", stale, append(fresh, ordinary))
	stale = place("2", time.Hour)
	if err := again.removeTemporaryBundles(); err != nil {
		t.Fatal(err)
	}
	check("a purge", stale, append(fresh, ordinary))
}

// holdGit puts a Git first on PATH whose command, such as "bundle create",
// waits once it has begun until release runs; began reports that one has.
func holdGit(t *testing.T, command string) (began func() bool, release func()) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim, signals := t.TempDir(), t.TempDir()
	marker, released := filepath.Join(signals, "began"), filepath.Join(signals, "released")
	script := "#!/bin/sh\ncase \" $* \" in *\" " + command + " \"*) : >" + marker + "; while [ ! -e " + released + " ]; do sleep 0.05; done;; esac\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	release = func() { os.WriteFile(released, nil, 0o600) }
	t.Cleanup(release)
	return func() bool { _, err := os.Stat(marker); return err == nil }, release
}

// A purge removes the bundles this gateway holds of notebooks as they were:
// what a request failed to remove, and what a browser backup or a bundle
// export is still preparing, which is then refused and asked to start again
// rather than sent with the content the purge removed.
func TestPurgeRemovesTheBundlesBeingPrepared(t *testing.T) {
	for _, kind := range []string{"browser backup", "bundle export"} {
		t.Run(kind, func(t *testing.T) {
			app, store := fixture(t)
			ctx := context.Background()
			secret := webPurgeSecret()
			savePurgeNote(t, store, "notes/keys.md", "# Keys\n\nThe staging key is "+secret+".\n", "")
			bundle := filepath.Join(t.TempDir(), "notes.bundle")
			if err := store.Backup(ctx, bundle); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(bundle)
			if err != nil {
				t.Fatal(err)
			}
			// Bundles that requests failed to remove.
			left := []string{filepath.Join(app.data, "cache", "backups", "backup-1.bundle"), filepath.Join(app.data, "cache", "exports", "bundle-2")}
			for _, path := range left {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret})

			// The request makes its bundle of the notebook as it is, and is
			// held while the purge rewrites the notebook.
			began, release := holdGit(t, "bundle create")
			refused := make(chan error, 1)
			go func() {
				if kind == "browser backup" {
					w := call(app, "GET", "/api/backup", "", testToken)
					if w.Code != 409 || !strings.Contains(w.Body.String(), "A purge ran while this backup was prepared") {
						refused <- errors.New(w.Result().Status + " " + w.Body.String())
						return
					}
					refused <- nil
					return
				}
				chunk, err := app.backupChunk(ctx, store, "default", 0, exportPiece, "")
				if !errors.Is(err, errExportPurged) && !errors.Is(err, errExportGone) || chunk.Data != "" {
					refused <- errors.Join(errors.New("an export overtaken by a purge"), err)
					return
				}
				refused <- nil
			}()
			for deadline := time.Now().Add(30 * time.Second); !began(); time.Sleep(10 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the bundle was never begun")
				}
			}
			type outcome struct {
				code    int
				receipt purgeReceipt
				body    string
			}
			purged := make(chan outcome, 1)
			go func() {
				code, receipt, body := executePurgeHTTP(app, plan, "Leaked staging key")
				purged <- outcome{code, receipt, body}
			}()
			for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				if status, err := store.Status(ctx); err == nil && status.Revision != plan.Revision {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the purge never rewrote the notebook")
				}
			}
			release()
			if err := <-refused; err != nil {
				t.Fatalf("a %s the purge overtook: %v", kind, err)
			}
			done := <-purged
			if done.code != 200 || len(done.receipt.Warnings) > 0 || done.receipt.Rewritten == 0 {
				t.Fatalf("execute: HTTP %d %s", done.code, done.body)
			}
			for _, dir := range temporaryBundles {
				if entries, _ := os.ReadDir(filepath.Join(app.data, dir)); len(entries) != 0 {
					t.Errorf("%s holds %d bundles after the purge", dir, len(entries))
				}
			}
			if n := storedExports(t, app); n != 0 {
				t.Error("an export the purge overtook remains stored:", n)
			}
		})
	}
}
