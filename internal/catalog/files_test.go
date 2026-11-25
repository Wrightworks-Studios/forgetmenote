package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"

	"github.com/jackc/pgx/v5"
)

// releaseA copies the catalog files the file-based release wrote in
// testdata/release-a: two accounts, a group, a lowered role with its access
// epoch, a logo and their receipts.
func releaseA(t *testing.T) string {
	t.Helper()
	data := t.TempDir()
	entries, err := os.ReadDir("testdata/release-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join("testdata/release-a", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(data, e.Name()), raw, 0600); err != nil {
			t.Fatal(err)
		}
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

// storeNotebooks puts empty notebooks ids into the notes store of data,
// beside a catalog that lists them.
func storeNotebooks(t *testing.T, data string, db *pg.DB, ids ...string) {
	t.Helper()
	notes := testNotes(t, data, db)
	for _, id := range ids {
		store, err := workspace.CreateDB(t.Context(), notes, id, t.TempDir(), nil)
		if err != nil {
			t.Fatal(err)
		}
		store.Close()
	}
	notes.Close()
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

// An imported catalog keeps its permissions, epochs, receipts and logo, and
// exports back to the files the file-based release wrote, byte for byte.
func TestCatalogFilesRoundTrip(t *testing.T) {
	data := releaseA(t)
	db := pgtest.Dir(t, data)
	imported, err := importFiles(t, db, data)
	if err != nil {
		t.Fatal(err)
	}
	if imported.Files != 3 {
		t.Fatalf("import read %d files", imported.Files)
	}
	storeNotebooks(t, data, db, "default", "work")
	c, err := Open(data, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	admin := c.LocalUsername("")
	var bob string
	users, projects, err := c.Administration(Bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if c.LocalUsername(u.ID) == "bob" {
			bob = u.ID
		}
		if c.LocalUsername(u.ID) == "admin" {
			admin = u.ID
		}
	}
	if bob == "" || admin == "" || len(projects) != 2 {
		t.Fatalf("imported accounts or notebooks are missing: %v %v", users, projects)
	}
	if _, err = c.Access(bob, "work", "read"); err != nil {
		t.Fatal("bob lost read access", err)
	}
	if _, err = c.Access(bob, "work", "edit"); err == nil {
		t.Fatal("bob regained the role he lost")
	}
	if c.ProjectAccessEpoch(bob, "work") != 1 {
		t.Fatal("the lowered role's access epoch was lost")
	}
	if c.InstanceName() != "Fixture Instance" || len(c.InstanceLogo()) == 0 {
		t.Fatal("instance branding was lost")
	}
	org, err := c.Organization(admin)
	if err != nil || len(org.Activity) != 8 || org.Activity[0].Action != "fixture.note" {
		t.Fatalf("receipts were lost: %v %+v", err, org.Activity)
	}
	if _, _, err = c.AuthenticateLocal("bob", "fixture password for bob"); err != nil {
		t.Fatal("the local password was lost", err)
	}
	c.Close()
	exported, dir := exportFiles(t, db)
	if exported != imported {
		t.Fatalf("export %+v differs from import %+v", exported, imported)
	}
	entries, _ := os.ReadDir("testdata/release-a")
	for _, e := range entries {
		want, _ := os.ReadFile(filepath.Join("testdata/release-a", e.Name()))
		got, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s changed in the round trip (%v):\n%s\n%s", e.Name(), err, want, got)
		}
	}
}

// A cutover imports the stopped file-based volume through a read-only mount, so
// its release can restart on it untouched. Until its notebooks are imported
// into the notes store too, no gateway opens empty copies of them.
func TestCatalogImportFromAReadOnlyVolume(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through file modes")
	}
	data := releaseA(t)
	if err := os.WriteFile(filepath.Join(data, "catalog.lock"), nil, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(data, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(data, 0700) })
	db := pgtest.Dir(t, data)
	imported, err := importFiles(t, db, data)
	if err != nil {
		t.Fatal("a read-only volume was refused:", err)
	}
	entries, err := os.ReadDir(data)
	if err != nil || len(entries) != 4 {
		t.Fatalf("the import changed the source volume: %v %v", entries, err)
	}
	gateway := t.TempDir()
	if c, err := Open(gateway, nil, db); err == nil {
		c.Close()
		t.Fatal("a gateway opened notebooks the notes store does not hold yet")
	}
	if _, err = os.Stat(filepath.Join(gateway, "projects")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the gateway created empty copies of the notebooks", err)
	}
	if _, err = testNotes(t, gateway, db).Notebook("work").Head(t.Context()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the gateway stored an empty copy of a notebook", err)
	}
	if exported, _ := exportFiles(t, db); exported != imported {
		t.Fatalf("export %+v differs from import %+v", exported, imported)
	}
}

// Changes made after the import export as the file-based release would have
// written them, with the access epoch key it reads.
func TestCatalogExportCarriesLaterChanges(t *testing.T) {
	data := releaseA(t)
	db := pgtest.Dir(t, data)
	if _, err := importFiles(t, db, data); err != nil {
		t.Fatal(err)
	}
	storeNotebooks(t, data, db, "default", "work")
	c, err := Open(data, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Member(Bootstrap, "work", "carol", "edit"); err != nil {
		t.Fatal(err)
	}
	if err = c.Member(Bootstrap, "work", "carol", ""); err != nil {
		t.Fatal(err)
	}
	c.Close()
	sum, dir := exportFiles(t, db)
	if sum.Files != 3 {
		t.Fatalf("export wrote %d files", sum.Files)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "catalog.json"))
	if !strings.Contains(string(raw), `"work\u0000carol": 1`) || strings.Contains(string(raw), "localVolume") {
		t.Fatalf("export is not in the file-based release's format:\n%s", raw)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "activity.jsonl"))
	if lines := bytes.Count(log, []byte("\n")); lines != 10 {
		t.Fatalf("export holds %d receipts", lines)
	}
	// The export imports again, as the file-based release would read it.
	if _, err = importFiles(t, pgtest.Org(t), dir); err != nil {
		t.Fatal(err)
	}
}

// The catalog keeps the receipts the file-based release kept, the newest
// 2*activityLimit, so its table does not grow with every change.
func TestCatalogKeepsTheNewestReceipts(t *testing.T) {
	data := releaseA(t)
	// A file-based log that was compacted holds only its newest receipts.
	sequence := 2*activityLimit + 8
	var log bytes.Buffer
	for id := sequence - 2*activityLimit + 1; id <= sequence; id++ {
		fmt.Fprintf(&log, `{"id":%d,"time":"2026-11-04T04:49:46Z","actor":"@local-admin","action":"fixture.note","target":"work","outcome":"ok"}`+"\n", id)
	}
	raw, _ := os.ReadFile(filepath.Join(data, catalogFile))
	os.WriteFile(filepath.Join(data, catalogFile), bytes.Replace(raw, []byte(`"activitySequence": 8,`), fmt.Appendf(nil, `"activitySequence": %d,`, sequence), 1), 0600)
	os.WriteFile(filepath.Join(data, activityFile), log.Bytes(), 0600)
	db := pgtest.Dir(t, data)
	if _, err := importFiles(t, db, data); err != nil {
		t.Fatal(err)
	}
	storeNotebooks(t, data, db, "default", "work")
	c, err := Open(data, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Member(Bootstrap, "work", "carol", "edit"); err != nil {
		t.Fatal(err)
	}
	c.Close()
	var count, newest int64
	if err = db.Pool.QueryRow(context.Background(), "SELECT count(*), max(id) FROM catalog_activity WHERE org = $1", db.Org).Scan(&count, &newest); err != nil {
		t.Fatal(err)
	}
	if count != 2*activityLimit || newest != int64(sequence)+1 {
		t.Fatalf("the catalog holds %d receipts up to %d", count, newest)
	}
}

// An import refuses what the file-based release would refuse or repair first.
func TestCatalogImportRefusesUnrepairedFiles(t *testing.T) {
	cases := map[string]func(data string){
		"interrupted change": func(data string) {
			os.WriteFile(filepath.Join(data, journalFile), []byte(`{"version":1}`), 0600)
		},
		"newer layout": func(data string) {
			raw, _ := os.ReadFile(filepath.Join(data, catalogFile))
			os.WriteFile(filepath.Join(data, catalogFile), bytes.Replace(raw, []byte(`"version": 10`), []byte(`"version": 11`), 1), 0600)
		},
		"lost receipt": func(data string) {
			raw, _ := os.ReadFile(filepath.Join(data, activityFile))
			lines := bytes.SplitAfter(raw, []byte("\n"))
			os.WriteFile(filepath.Join(data, activityFile), bytes.Join(lines[:len(lines)-2], nil), 0600)
		},
		"changed logo": func(data string) {
			files, _ := filepath.Glob(filepath.Join(data, "catalog-logo-*.bin"))
			os.WriteFile(files[0], []byte("not the committed logo"), 0600)
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
			if err := db.Pool.QueryRow(context.Background(), "SELECT count(*) FROM documents WHERE org = $1", db.Org).Scan(&n); err != nil || n != 0 {
				t.Fatal("a refused import wrote the catalog", n, err)
			}
		})
	}
	if _, err := importFiles(t, pgtest.Org(t), t.TempDir()); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an instance without a catalog failed to import", err)
	}
}

// embeddedCatalog rewrites the fixture in a layout older than version 9, which
// keeps receipts and the logo inside catalog.json: an envelope of version, or
// the bare catalog when version is 0.
func embeddedCatalog(t *testing.T, data string, version int) {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(data, catalogFile))
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	state := envelope["data"].(map[string]any)
	logo, _ := os.ReadFile(filepath.Join(data, logoFile(state["instanceLogoHash"].(string))))
	log, _ := os.ReadFile(filepath.Join(data, activityFile))
	var activity []any
	for line := range bytes.Lines(log) {
		var entry any
		json.Unmarshal(line, &entry)
		activity = append(activity, entry)
	}
	state["activity"], state["instanceLogo"] = activity, logo
	delete(state, "instanceLogoHash")
	var file any = map[string]any{"version": version, "data": state}
	if version == 0 {
		file = state
	}
	raw, _ = json.Marshal(file)
	files, _ := filepath.Glob(filepath.Join(data, "catalog-logo-*.bin"))
	for _, f := range append(files, filepath.Join(data, activityFile)) {
		os.Remove(f)
	}
	os.WriteFile(filepath.Join(data, catalogFile), raw, 0600)
}

// A catalog older than version 9 imports as the file-based release reads it,
// and exports as the separated files that release writes on its next change.
func TestCatalogImportReadsEmbeddedLayouts(t *testing.T) {
	want, _ := importFiles(t, pgtest.Org(t), releaseA(t))
	for _, version := range []int{0, 8} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			data := releaseA(t)
			embeddedCatalog(t, data, version)
			db := pgtest.Dir(t, data)
			imported, err := importFiles(t, db, data)
			if err != nil {
				t.Fatal(err)
			}
			if imported != want {
				t.Fatalf("the import describes %+v, not the separated files %+v", imported, want)
			}
			storeNotebooks(t, data, db, "default", "work")
			c, err := Open(data, nil, db)
			if err != nil {
				t.Fatal(err)
			}
			org, err := c.Organization(Bootstrap)
			if err != nil || len(org.Activity) != 8 || len(c.InstanceLogo()) == 0 {
				t.Fatalf("receipts or logo were lost: %v %d", err, len(org.Activity))
			}
			c.Close()
			exported, dir := exportFiles(t, db)
			if exported != want {
				t.Fatalf("export %+v differs from %+v", exported, want)
			}
			entries, _ := os.ReadDir("testdata/release-a")
			for _, e := range entries {
				original, _ := os.ReadFile(filepath.Join("testdata/release-a", e.Name()))
				if got, err := os.ReadFile(filepath.Join(dir, e.Name())); err != nil || !bytes.Equal(got, original) {
					t.Fatalf("%s is not what the file-based release writes (%v)", e.Name(), err)
				}
			}
		})
	}
	data := releaseA(t)
	embeddedCatalog(t, data, 5)
	raw, _ := os.ReadFile(filepath.Join(data, catalogFile))
	os.WriteFile(filepath.Join(data, catalogFile), bytes.Replace(raw, []byte(`"data":{`), []byte(`"data":{"ai":{"revision":1},`), 1), 0600)
	if _, err := importFiles(t, pgtest.Org(t), data); err == nil || !strings.Contains(err.Error(), "AI policy") {
		t.Fatalf("a version 5 catalog with an AI policy was accepted: %v", err)
	}
}
