package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/notestore"
)

// backupTestStore is a test store whose notebooks are exported, as a
// gateway's are, so it can be backed up.
func backupTestStore(t testing.TB) *Store {
	t.Helper()
	s := newTestStore(t)
	useTestExporter(t, s.noteStore)
	return s
}

// useTestExporter exports notes' notebooks into a directory of the test's
// and makes that export the source of their backups. It runs no background
// export: each backup brings the export up to the head itself.
func useTestExporter(t testing.TB, notes *notestore.Store) {
	t.Helper()
	exporter, err := gitexport.New(notes, filepath.Join(t.TempDir(), "exports"), gitexport.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(exporter.Close)
	t.Cleanup(UseExporter(notes, exporter))
}

// backupFile backs s up into a new file and returns its path.
func backupFile(t testing.TB, s *Store) string {
	t.Helper()
	bundle := filepath.Join(t.TempDir(), "notebook.bundle")
	if err := s.Backup(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}
	return bundle
}

// bundleHead is the commit HEAD names in the header of the bundle at path.
func bundleHead(t testing.TB, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	header, _, _ := strings.Cut(string(raw), "\n\n")
	for _, line := range strings.Split(header, "\n") {
		if id, ref, _ := strings.Cut(line, " "); ref == "HEAD" {
			return id
		}
	}
	t.Fatalf("no HEAD in the bundle header %q", header)
	return ""
}

// Each backup brings the notebook's export up to the head first, so it holds
// every revision committed before it, here or by another gateway, though no
// background export ran.
func TestBackupHoldsEveryRevisionCommittedBeforeIt(t *testing.T) {
	s := backupTestStore(t)
	ctx := context.Background()
	first, err := s.Save(ctx, "a.md", []byte("first"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if head := bundleHead(t, backupFile(t, s)); head != first.Revision {
		t.Fatalf("the backup is at %s, the notebook at %s", head, first.Revision)
	}
	if _, err = s.Save(ctx, "a.md", []byte("second"), first.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	head := commitFixture(t, s, fixture("b.md", "from another gateway"))
	if got := bundleHead(t, backupFile(t, s)); got != head {
		t.Fatalf("the backup is at %s, the notebook at %s", got, head)
	}
	limited := filepath.Join(t.TempDir(), "limited.bundle")
	if err = s.BackupLimited(ctx, limited, 1<<20); err != nil {
		t.Fatal(err)
	}
	if got := bundleHead(t, limited); got != head {
		t.Fatalf("the bounded backup is at %s, the notebook at %s", got, head)
	}
}

// A backup refuses, as the Git backend's did, while accepted live edits wait
// for their checkpoint, and writes nothing.
func TestBackupRefusesPendingLiveEdits(t *testing.T) {
	s := backupTestStore(t)
	ctx := context.Background()
	if _, err := s.Save(ctx, "a.md", []byte("a"), "", "test"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.live["a.md"] = &liveRecord{Path: "a.md", Uncommitted: []string{"op"}}
	s.mu.Unlock()
	target := filepath.Join(t.TempDir(), "notebook.bundle")
	if err := s.Backup(ctx, target); !errors.Is(err, ErrLivePending) {
		t.Fatal("a backup past pending live edits:", err)
	}
	if err := s.BackupLimited(ctx, target, 1<<20); !errors.Is(err, ErrLivePending) {
		t.Fatal("a bounded backup past pending live edits:", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused backup wrote its target:", err)
	}
	s.mu.Lock()
	s.live["a.md"].Uncommitted = nil
	s.mu.Unlock()
	if err := s.Backup(ctx, target); err != nil {
		t.Fatal(err)
	}
}

// A process that runs no export of the notebook cannot back it up, says so
// and writes nothing.
func TestBackupNeedsTheGitExport(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	target := filepath.Join(t.TempDir(), "notebook.bundle")
	if err := s.Backup(ctx, target); !errors.Is(err, errNoExport) {
		t.Fatal("a backup without the export:", err)
	}
	if err := s.BackupLimited(ctx, target, 1<<20); !errors.Is(err, errNoExport) {
		t.Fatal("a bounded backup without the export:", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused backup wrote its target:", err)
	}
}

// A deleted notebook's export goes with RemoveExport, so a notebook created
// later under its ID backs up instead of diverging from the old export.
func TestRemoveExportLetsANotebookIDBeReused(t *testing.T) {
	s := backupTestStore(t)
	ctx := context.Background()
	if _, err := s.Save(ctx, "a.md", []byte("old"), "", "test"); err != nil {
		t.Fatal(err)
	}
	backupFile(t, s)
	if err := DeleteDB(ctx, s.noteStore, s.notebook); err != nil {
		t.Fatal(err)
	}
	if err := RemoveExport(ctx, s.noteStore, s.notebook); err != nil {
		t.Fatal(err)
	}
	again, err := CreateDB(ctx, s.noteStore, s.notebook, filepath.Join(t.TempDir(), s.notebook), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { again.Close() })
	doc, err := again.Save(ctx, "b.md", []byte("new"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if head := bundleHead(t, backupFile(t, again)); head != doc.Revision {
		t.Fatalf("the backup is at %s, the new notebook at %s", head, doc.Revision)
	}
	if err := RemoveExport(ctx, newTestStore(t).noteStore, "notes"); err != nil {
		t.Fatal("without an exporter:", err)
	}
}

// A backup file is made as git bundle create made it: written as target.lock,
// with the mode a new file takes under the umask, and renamed into place; a
// lock file that exists refuses the backup and stays.
func TestBackupWritesItsFileAsGitDid(t *testing.T) {
	s := backupTestStore(t)
	ctx := context.Background()
	if _, err := s.Save(ctx, "a.md", []byte("a"), "", "test"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(probe, nil, 0666); err != nil {
		t.Fatal(err)
	}
	want, err := os.Stat(probe)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "notebook.bundle")
	if err = s.Backup(ctx, target); err != nil {
		t.Fatal(err)
	}
	got, err := os.Stat(target)
	if err != nil || got.Mode().Perm() != want.Mode().Perm() {
		t.Fatalf("the backup's mode is %v, a new file's %v: %v", got.Mode().Perm(), want.Mode().Perm(), err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("the backup left %v", entries)
	}
	if err = os.WriteFile(target+".lock", []byte("another backup"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.Backup(ctx, target); !errors.Is(err, os.ErrExist) {
		t.Fatal("a backup past its target's lock file:", err)
	}
	if raw, err := os.ReadFile(target + ".lock"); err != nil || string(raw) != "another backup" {
		t.Fatalf("the refused backup changed the lock file: %q %v", raw, err)
	}
}
