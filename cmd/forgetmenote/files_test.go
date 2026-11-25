package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"forgetmenote/internal/auth"
	"forgetmenote/internal/filestate"
	"forgetmenote/internal/instanceconfig"
	"forgetmenote/internal/pg"
)

// releaseAFiles is a stopped file-based instance's state: every owner
// package's testdata/release-a, which that release's own code wrote, and a
// sign-in state. Personal connection grants open with the fixtures' key.
func releaseAFiles(t *testing.T, data string) {
	t.Helper()
	later := time.Now().Add(time.Hour).Unix()
	files := map[string][]byte{
		"oauth.json": []byte(`{"clients":{"client-1":{"client_id":"client-1","client_name":"Editor","redirect_uris":["http://127.0.0.1:9/callback"],"created":1}},` +
			`"access":{"` + auth.Digest("access-1") + `":{"family":"family-1","familyExpires":` + jsonNumber(later) + `,"Client":"client-1","Scope":"notes:read","Resource":"","Subject":"alice","Expires":` + jsonNumber(later) + `,"Epoch":0}},` +
			`"refresh":{},"sessions":{"` + auth.Digest("session-1") + `":{"Client":"browser","Scope":"","Resource":"","Subject":"alice","Expires":` + jsonNumber(later) + `,"Epoch":0}}}`),
	}
	fixtures, err := filepath.Glob("../../internal/*/testdata/release-a")
	if err != nil || len(fixtures) == 0 {
		t.Fatal("no release A fixtures", err)
	}
	for _, root := range fixtures {
		err = filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			// Other packages' fixtures carry a catalog of their own.
			if (rel == "catalog.json" || rel == "activity.jsonl") && !strings.Contains(root, "/catalog/") {
				return nil
			}
			files[filepath.ToSlash(rel)], err = os.ReadFile(path)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for rel, content := range files {
		path := filepath.Join(data, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	keys := t.TempDir()
	os.WriteFile(filepath.Join(keys, "key"), []byte(strings.Repeat("ba", 32)), 0600)
	os.WriteFile(filepath.Join(keys, "secret"), []byte("fixture-oauth-secret"), 0600)
	app := map[string]string{"clientID": "fixture", "secretFile": filepath.Join(keys, "secret")}
	config, _ := json.Marshal(map[string]any{"version": 1, "keyFile": filepath.Join(keys, "key"), "google": app, "slack": map[string]string{"clientID": "fixture", "secretFile": app["secretFile"], "workspace": "T123"}})
	os.WriteFile(filepath.Join(keys, "personal.json"), config, 0600)
	t.Setenv("PERSONAL_CONNECTIONS_CONFIG_FILE", filepath.Join(keys, "personal.json"))
}

func jsonNumber(n int64) string { raw, _ := json.Marshal(n); return string(raw) }

type stateReport struct {
	Imported []movedArea `json:"imported"`
	Exported []movedArea `json:"exported"`
}

func readReport(t *testing.T, out *bytes.Buffer) stateReport {
	t.Helper()
	var report stateReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("report %q: %v", out.String(), err)
	}
	return report
}

// An import reaches the stores, refuses to run twice, and an export of it
// reports the same files and digests as the originals.
func TestImportFilesRoundTripsAStoppedInstance(t *testing.T) {
	db := instanceDatabase(t, databaseURLFile(t, true))
	data := t.TempDir()
	t.Setenv("DATA_DIR", data)
	releaseAFiles(t, data)
	var out bytes.Buffer
	if err := runImportFiles(nil, &out); err != nil {
		t.Fatal(err)
	}
	imported := readReport(t, &out).Imported
	if len(imported) != len(stateAreas(instanceconfig.Config{})) || imported[0].Area != "catalog" || imported[0].Files != 3 || imported[2].Area != "sign-in" || imported[2].Files != 1 {
		t.Fatalf("import report %+v", imported)
	}
	for _, area := range imported {
		if area.Files == 0 {
			t.Fatalf("the %s fixture was not imported: %+v", area.Area, imported)
		}
	}
	state, err := auth.OpenState(db)
	if err != nil {
		t.Fatal(err)
	}
	if g, ok := state.Access[auth.Digest("access-1")]; !ok || g.Subject != "alice" || g.Family != "family-1" {
		t.Fatalf("the imported grant is missing: %+v", state.Access)
	}
	if err = runImportFiles(nil, &bytes.Buffer{}); !errors.Is(err, pg.ErrNotEmpty) {
		t.Fatalf("a second import was not refused: %v", err)
	}
	exported := filepath.Join(t.TempDir(), "export")
	out.Reset()
	if err = runExportFiles([]string{"-output", exported}, &out); err != nil {
		t.Fatal(err)
	}
	if report := readReport(t, &out).Exported; !equalAreas(report, imported) {
		t.Fatalf("export %+v differs from import %+v", report, imported)
	}
	for _, rel := range []string{"oauth.json", "catalog.json", "activity.jsonl"} {
		before, _ := os.ReadFile(filepath.Join(data, rel))
		after, err := os.ReadFile(filepath.Join(exported, rel))
		if err != nil {
			t.Fatal(err)
		}
		a, _ := filestate.JSON(before)
		b, _ := filestate.JSON(after)
		if !bytes.Equal(a, b) {
			t.Fatalf("%s changed in the round trip:\n%s\n%s", rel, a, b)
		}
	}
	if err = runExportFiles([]string{"-output", exported}, &bytes.Buffer{}); err == nil {
		t.Fatal("an export replaced an existing directory")
	}
}

func equalAreas(a, b []movedArea) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// An export waits for every gateway to stop, so no write follows it.
func TestExportFilesRefusesWhileAGatewayServes(t *testing.T) {
	db := instanceDatabase(t, databaseURLFile(t, true))
	t.Setenv("DATA_DIR", t.TempDir())
	leave, err := db.KeepPresence()
	if err != nil {
		t.Fatal(err)
	}
	exported := filepath.Join(t.TempDir(), "export")
	if err = runExportFiles([]string{"-output", exported}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "gateways are serving") {
		t.Fatalf("an export ran beside a serving gateway: %v", err)
	}
	if _, err = os.Stat(exported); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused export created its directory")
	}
	leave()
	if err = runExportFiles([]string{"-output", exported}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

// An import into a database a gateway already used is refused before reading.
func TestImportFilesRefusesADatabaseAGatewayUsed(t *testing.T) {
	db := instanceDatabase(t, databaseURLFile(t, true))
	data := t.TempDir()
	t.Setenv("DATA_DIR", data)
	releaseAFiles(t, data)
	leave, err := db.KeepPresence()
	if err != nil {
		t.Fatal(err)
	}
	defer leave()
	if err = runImportFiles(nil, &bytes.Buffer{}); !errors.Is(err, pg.ErrNotEmpty) {
		t.Fatalf("an import merged into a used database: %v", err)
	}
}

// An import refuses a data directory a file-based process still serves.
func TestImportFilesRefusesARunningFileBasedInstance(t *testing.T) {
	instanceDatabase(t, databaseURLFile(t, true))
	data := t.TempDir()
	t.Setenv("DATA_DIR", data)
	releaseAFiles(t, data)
	lock, err := os.OpenFile(filepath.Join(data, "catalog.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err = runImportFiles(nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "owns this data directory") {
		t.Fatalf("an import read a running instance: %v", err)
	}
}

// One area's invalid file fails the whole import, so the database stays empty
// and the import can run again once the file is repaired.
func TestImportFilesIsAllOrNothing(t *testing.T) {
	db := instanceDatabase(t, databaseURLFile(t, true))
	data := t.TempDir()
	t.Setenv("DATA_DIR", data)
	releaseAFiles(t, data)
	if err := os.WriteFile(filepath.Join(data, "oauth.json"), []byte(`{"clients":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runImportFiles(nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "import sign-in") {
		t.Fatalf("an invalid sign-in state was imported: %v", err)
	}
	if err := db.RequireEmpty(context.Background(), db.Pool); err != nil {
		t.Fatal("a failed import left state behind:", err)
	}
	releaseAFiles(t, data)
	if err := runImportFiles(nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}
