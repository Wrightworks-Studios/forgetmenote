package backups

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/drivebackup"
	"forgetmenote/internal/drivebackup/drivetest"
	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"

	"github.com/jackc/pgx/v5"
)

// The file-based release wrote testdata/release-a: "notes" backed up and then
// handed off to its storage owner, "journal" paused after a failed upload, and
// "old-notes" disconnected. The handoff raised the file to version 2.

func releaseA(t *testing.T) string {
	t.Helper()
	data := t.TempDir()
	if err := os.CopyFS(data, os.DirFS("testdata/release-a")); err != nil {
		t.Fatal(err)
	}
	return data
}

func importFiles(t *testing.T, db *pg.DB, data string) (filestate.Sum, error) {
	t.Helper()
	src, err := filestate.OpenSource(data)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	ctx := context.Background()
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	sum, err := ImportFiles(ctx, db, tx, src)
	if err == nil {
		err = tx.Commit(ctx)
	}
	return sum, err
}

func exportFiles(t *testing.T, db *pg.DB) (filestate.Sum, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "export")
	out, err := filestate.CreateTarget(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	ctx := context.Background()
	tx, err := db.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	sum, err := ExportFiles(ctx, db, tx, out)
	if err != nil {
		t.Fatal(err)
	}
	return sum, dir
}

// Imported bindings serve as the file-based release served them, and the
// export is the fixture byte for byte.
func TestBackupFilesRoundTrip(t *testing.T) {
	db := pgtest.Org(t)
	imported, err := importFiles(t, db, releaseA(t))
	if err != nil {
		t.Fatal(err)
	}
	if imported.Files != 1 {
		t.Fatalf("import read %d files", imported.Files)
	}
	m, _, _ := fixtureOn(t, db)
	notes, err := m.Status("notes")
	if err != nil || notes.Handoff != strings.Repeat("c", 32) || !notes.AuthorityTarget || notes.LastRevision != strings.Repeat("a", 40) || notes.LastSuccess.IsZero() || notes.Epoch != 2 {
		t.Fatalf("the handed-off binding changed: %+v %v", notes, err)
	}
	if journal, err := m.Status("journal"); err != nil || !journal.Connected || journal.Enabled || journal.Error == "" || journal.Branch != "backups/journal" {
		t.Fatalf("the paused binding changed: %+v %v", journal, err)
	}
	if old, err := m.Status("old-notes"); err != nil || old.Connected || old.Generation != 2 {
		t.Fatalf("the disconnected binding changed: %+v %v", old, err)
	}
	if listed, err := m.List("local"); err != nil || len(listed) != 1 || listed[0].Project != "notes" {
		t.Fatal("the worker would not see the undrained handoff", listed, err)
	}
	m.Close()
	exported, dir := exportFiles(t, db)
	if exported != imported {
		t.Fatalf("export %+v differs from import %+v", exported, imported)
	}
	want, _ := os.ReadFile(filepath.Join("testdata/release-a", bindingsFile))
	if got, err := os.ReadFile(filepath.Join(dir, bindingsFile)); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("bindings.json changed in the round trip (%v):\n%s\n%s", err, want, got)
	}
}

// An active lease is left behind, as the file-based release's restart ended
// it, and the export imports again.
func TestBackupExportLeavesLeasesBehind(t *testing.T) {
	db := pgtest.Org(t)
	if _, err := importFiles(t, db, releaseA(t)); err != nil {
		t.Fatal(err)
	}
	m, _, _ := fixtureOn(t, db)
	journal, err := m.Status("journal")
	if err != nil {
		t.Fatal(err)
	}
	if journal, err = m.Control(t.Context(), "journal", journal.Generation, "resume"); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Lease(t.Context(), "local", "journal", journal.Generation, strings.Repeat("d", 40)); err != nil {
		t.Fatal(err)
	}
	m.Close()
	_, dir := exportFiles(t, db)
	raw, _ := os.ReadFile(filepath.Join(dir, bindingsFile))
	if bytes.Contains(raw, []byte("leases")) || bytes.Contains(raw, []byte("synthetic-secret")) || !bytes.Contains(raw, []byte(`"enabled": true`)) {
		t.Fatalf("export is not the file-based release's bindings:\n%s", raw)
	}
	if _, err = importFiles(t, pgtest.Org(t), dir); err != nil {
		t.Fatal(err)
	}
}

// An import refuses what the file-based release refuses to open, and a refused
// import writes no document.
func TestBackupImportRefusesDamagedBindings(t *testing.T) {
	edit := func(old, new string) func([]byte) []byte {
		return func(raw []byte) []byte { return bytes.Replace(raw, []byte(old), []byte(new), 1) }
	}
	cases := map[string]func([]byte) []byte{
		"damaged JSON":      func(raw []byte) []byte { return raw[:len(raw)/2] },
		"newer version":     edit(`"version": 2`, `"version": 3`),
		"missing bindings":  func([]byte) []byte { return []byte(`{"version": 1}`) },
		"renamed binding":   edit(`"project": "journal"`, `"project": "diary"`),
		"destination twice": edit(`"branch": "backups/journal"`, `"branch": "main"`),
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			data := releaseA(t)
			path := filepath.Join(data, bindingsFile)
			raw, _ := os.ReadFile(path)
			os.WriteFile(path, corrupt(raw), 0600)
			db := pgtest.Org(t)
			if _, err := importFiles(t, db, data); err == nil {
				t.Fatal("the import accepted it")
			}
			if version, _, err := db.ReadDocument(context.Background(), db.Pool, stateKind, stateID, -1); err != nil || version != 0 {
				t.Fatal("a refused import wrote the bindings", version, err)
			}
		})
	}
	if sum, err := importFiles(t, pgtest.Org(t), t.TempDir()); err != nil || sum.Files != 0 {
		t.Fatal("an instance without managed backups failed to import", sum, err)
	}
}

// Drive destinations travel in drives.json beside bindings.json, with their
// credentials still sealed, and an import serves them to a gateway that holds
// the seal key. The file-based release reads bindings.json as before.
func TestDriveDestinationsRoundTripSealed(t *testing.T) {
	db := pgtest.Org(t)
	m, f, _ := driveManager(t, db, false)
	d, password, refresh := connectDrive(t, m, f, drivebackup.Google, "notes", "alice@example.com", true, 0)
	m.Close()
	exported, dir := exportFiles(t, db)
	if exported.Files != 2 {
		t.Fatalf("export wrote %d files", exported.Files)
	}
	bindings, _ := os.ReadFile(filepath.Join(dir, bindingsFile))
	drives, _ := os.ReadFile(filepath.Join(dir, drivesFile))
	if !bytes.Contains(bindings, []byte(`"version": 2`)) || !bytes.Contains(bindings, []byte(`"bindings": {}`)) {
		t.Fatalf("bindings.json is not the file-based release's:\n%s", bindings)
	}
	if !bytes.Contains(drives, []byte(d.Path)) || !bytes.Contains(drives, []byte("sealedRefresh")) || bytes.Contains(drives, []byte(refresh)) || bytes.Contains(drives, []byte(password)) {
		t.Fatalf("drives.json does not keep the destination sealed:\n%s", drives)
	}
	restored := pgtest.Org(t)
	imported, err := importFiles(t, restored, dir)
	if err != nil || imported != exported {
		t.Fatal(imported, exported, err)
	}
	again, err := Open(restored, nil, f.Client, func(Binding) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	l, err := again.DriveLease(t.Context(), "local", drivebackup.Google, "notes", d.Generation, strings.Repeat("a", 40))
	if err != nil || l.Path != d.Path || l.Encryption == nil || l.Encryption.Password != password {
		t.Fatalf("restored destination: %+v %v", l, err)
	}
	elsewhere := pgtest.Org(t)
	if _, err = importFiles(t, elsewhere, dir); err != nil {
		t.Fatal(err)
	}
	wrong, err := Open(elsewhere, nil, drivetest.New(t).Client, func(Binding) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	if _, err = wrong.DriveLease(t.Context(), "local", drivebackup.Google, "notes", d.Generation, strings.Repeat("a", 40)); err == nil {
		t.Fatal("a gateway with another seal key leased the destination")
	}
}
