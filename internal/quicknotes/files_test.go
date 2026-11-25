package quicknotes

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"

	"github.com/jackc/pgx/v5"
)

// The file-based release wrote testdata/release-a: alice's capture in Inbox,
// one filed to a folder with special characters, one left saving by a dirty
// Inbox and her manual settings, and bob's capture without settings.
const (
	fixtureAlice = "cdb6bec550cfc4564d4ddb8f08598636"
	fixtureBob   = "fb423e12fd5e1ddae6b70d58446a2441"
	fixtureSaved = "29b00c6e3890fdea713e94f09f633663"
)

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

// importedService opens the store over the imported rows without its scan.
func importedService(t *testing.T, db *pg.DB) *Service {
	t.Helper()
	c, err := catalog.Open(t.TempDir(), nil, pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(db, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); c.Close() })
	return s
}

// Imported captures and settings read as the store reads them, the saving
// capture is recovered by the scan, and the export is the fixture byte for byte.
func TestQuicknotesFilesRoundTrip(t *testing.T) {
	data := releaseA(t)
	// An interrupted write leaves a temporary file the release never reads.
	if err := os.WriteFile(filepath.Join(data, "quicknotes", fixtureAlice, tempPrefix+"123456"), []byte(`{"schema":`), 0600); err != nil {
		t.Fatal(err)
	}
	db := pgtest.Org(t)
	imported, err := importFiles(t, db, data)
	if err != nil {
		t.Fatal(err)
	}
	if imported.Files != 5 {
		t.Fatalf("import read %d files", imported.Files)
	}
	s := importedService(t, db)
	settings, err := s.settings(fixtureAlice)
	if err != nil || settings.Mode != "manual" || settings.NotifyDestination || settings.Generation != 2 || len(settings.ApprovedDestinations) != 2 || settings.ApprovedDestinations[1].Folder != "Ideas ✨ & more" {
		t.Fatalf("alice's settings were lost: %+v %v", settings, err)
	}
	if settings, err = s.settings(fixtureBob); err != nil || settings.Generation != defaultSettings().Generation {
		t.Fatal("bob gained settings", settings, err)
	}
	recent, err := s.recentRecords(fixtureAlice, 100)
	if err != nil || len(recent) != 3 || recent[0].Status != "saving" || recent[1].Status != "filed" || recent[2].Status != "inbox" {
		t.Fatalf("alice's captures were lost: %+v %v", recent, err)
	}
	if recent[1].Path != "Ideas ✨ & more/quicknote-"+recent[1].ID+".md" || recent[1].FiledVersion == "" || recent[1].version != 1 {
		t.Fatalf("the filed capture changed: %+v", recent[1])
	}
	unfinished, err := s.unfinished()
	if err != nil || len(unfinished) != 3 || !strings.Contains(strings.Join(unfinished, " "), fixtureAlice+"/"+fixtureSaved) {
		t.Fatal("the scan would not visit the unfinished captures", unfinished, err)
	}
	if s.busyElsewhere(context.Background(), recent[0]) {
		t.Fatal("an imported save is held by a gateway")
	}
	exported, dir := exportFiles(t, db)
	if exported != imported {
		t.Fatalf("export %+v differs from import %+v", exported, imported)
	}
	written := 0
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		written++
		rel, _ := filepath.Rel(dir, path)
		want, _ := os.ReadFile(filepath.Join("testdata/release-a", rel))
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s changed in the round trip (%v):\n%s\n%s", rel, err, want, got)
		}
		return nil
	})
	if err != nil || written != 5 {
		t.Fatal("export wrote", written, err)
	}
}

// A capture a gateway began filing exports without its holder, which the
// file-based release refuses, and the export imports again.
func TestQuicknotesExportDropsHolders(t *testing.T) {
	db := pgtest.Org(t)
	if _, err := importFiles(t, db, releaseA(t)); err != nil {
		t.Fatal(err)
	}
	s := importedService(t, db)
	r, err := s.readRecord(fixtureAlice, fixtureSaved)
	if err != nil {
		t.Fatal(err)
	}
	r.Holder = db.Holder
	if err = s.writeRecord(&r); err != nil || r.version != 2 {
		t.Fatal("the imported capture did not take a write over its version", r.version, err)
	}
	_, dir := exportFiles(t, db)
	raw, _ := os.ReadFile(filepath.Join(dir, "quicknotes", fixtureAlice, fixtureSaved+".json"))
	if bytes.Contains(raw, []byte("holder")) {
		t.Fatalf("export kept the gateway holder:\n%s", raw)
	}
	if _, err = importFiles(t, pgtest.Org(t), dir); err != nil {
		t.Fatal(err)
	}
}

// An import refuses what the file-based release would refuse to read, and a
// refused import writes no row.
func TestQuicknotesImportRefusesUnreadableFiles(t *testing.T) {
	record := filepath.Join("quicknotes", fixtureAlice, fixtureSaved+".json")
	cases := map[string]func(data string){
		"another capture's identity": func(data string) {
			raw, _ := os.ReadFile(filepath.Join(data, record))
			os.WriteFile(filepath.Join(data, record), bytes.Replace(raw, []byte(`"actor":"`+fixtureAlice), []byte(`"actor":"`+fixtureBob), 1), 0600)
		},
		"unknown field": func(data string) {
			raw, _ := os.ReadFile(filepath.Join(data, record))
			os.WriteFile(filepath.Join(data, record), bytes.Replace(raw, []byte(`{`), []byte(`{"holder":"gateway",`), 1), 0600)
		},
		"invalid settings": func(data string) {
			path := filepath.Join(data, "quicknotes", fixtureAlice, settingsFile)
			raw, _ := os.ReadFile(path)
			os.WriteFile(path, bytes.Replace(raw, []byte(`"manual"`), []byte(`"sometimes"`), 1), 0600)
		},
		"stray file": func(data string) {
			os.WriteFile(filepath.Join(data, "quicknotes", fixtureBob, "notes.txt"), []byte("stray"), 0600)
		},
		"stray directory": func(data string) {
			os.Mkdir(filepath.Join(data, "quicknotes", "lost+found"), 0700)
		},
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			data := releaseA(t)
			corrupt(data)
			db := pgtest.Org(t)
			if _, err := importFiles(t, db, data); err == nil {
				t.Fatal("the import accepted it")
			}
			var n int
			if err := db.Pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM captures WHERE org = $1) + (SELECT count(*) FROM capture_settings WHERE org = $1)", db.Org).Scan(&n); err != nil || n != 0 {
				t.Fatal("a refused import wrote rows", n, err)
			}
		})
	}
	if sum, err := importFiles(t, pgtest.Org(t), t.TempDir()); err != nil || sum.Files != 0 {
		t.Fatal("an instance without Quicknotes failed to import", sum, err)
	}
}
