package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func databaseURLFile(t *testing.T, migrated bool) string {
	t.Helper()
	file := writeDatabaseURL(t, migrated)
	t.Setenv("DATABASE_URL_FILE", file)
	return file
}

// writeDatabaseURL stores a fresh database's URL in a private file.
func writeDatabaseURL(t *testing.T, migrated bool) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "database-url")
	if err := os.WriteFile(file, []byte(pgtest.URL(t, migrated)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return file
}

// instanceDatabase opens the database a URL file names, so a test shares the
// control plane of the commands it runs, as one instance's processes do.
func instanceDatabase(t *testing.T, file string) *pg.DB {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	db, err := pg.Open(context.Background(), string(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func TestServeRefusesAnUnmigratedDatabaseUntilMigrateRuns(t *testing.T) {
	databaseURLFile(t, false)
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("LISTEN_ADDR", "127.0.0.1:0")
	if err := runArguments(nil); err == nil || !strings.Contains(err.Error(), "forgetmenote migrate") {
		t.Fatalf("serve accepted a database without the schema: %v", err)
	}
	var out bytes.Buffer
	migrations, err := filepath.Glob("../../internal/pg/migrations/*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatal(migrations, err)
	}
	if err := runMigrate(nil, &out); err != nil || out.String() != fmt.Sprintf("applied %d migrations\n", len(migrations)) {
		t.Fatalf("first migrate: %q %v", out.String(), err)
	}
	out.Reset()
	if err := runMigrate([]string{"-finish-rollout"}, &out); err != nil {
		t.Fatal(err)
	}
	if want := "applied 0 migrations\ndatabase requires reader version 2\n"; out.String() != want || pg.ReaderVersion != 2 {
		t.Fatalf("second migrate printed %q", out.String())
	}
}

// The previous release kept notebooks in Git repositories and reads version 1
// of the database. A migrated database still admits it, as the storage v2
// cutover's refusals need; once the rollout is finished, it requires the
// reader version of this release, which keeps notebooks in the notes store,
// so a container left on the previous image refuses to serve the instance
// rather than serving and changing the old repositories.
func TestFinishingTheRolloutShutsOutTheGitRelease(t *testing.T) {
	file := databaseURLFile(t, false)
	if err := runMigrate(nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	db := instanceDatabase(t, file)
	const previousRelease = 1
	required := func() int {
		t.Helper()
		var version int
		if err := db.Pool.QueryRow(t.Context(), "SELECT min_version FROM instance_reader").Scan(&version); err != nil {
			t.Fatal(err)
		}
		return version
	}
	if v := required(); v > previousRelease {
		t.Fatalf("a migrated database requires reader version %d; the previous release reads %d", v, previousRelease)
	}
	if err := runMigrate([]string{"-finish-rollout"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if v := required(); v <= previousRelease || v != pg.ReaderVersion {
		t.Fatalf("a finished rollout requires reader version %d, which the previous release (%d) still serves", v, previousRelease)
	}
	if err := db.Check(t.Context()); err != nil {
		t.Fatal("this release refuses the database it finished:", err)
	}
}

func TestMigrateRequiresAPrivateAbsoluteURLFile(t *testing.T) {
	t.Setenv("DATABASE_URL_FILE", "")
	if err := runMigrate(nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "no database is configured") {
		t.Fatalf("migrate without a database: %v", err)
	}
	t.Setenv("DATABASE_URL_FILE", "relative/database-url")
	if err := runMigrate(nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("migrate accepted a relative URL file: %v", err)
	}
	file := filepath.Join(t.TempDir(), "database-url")
	if err := os.WriteFile(file, []byte("postgres://fmn:hunter2-secret@[::1"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL_FILE", file)
	if err := runMigrate(nil, &bytes.Buffer{}); err == nil || strings.Contains(err.Error(), "hunter2-secret") {
		t.Fatalf("migrate leaked or accepted a bad URL: %v", err)
	}
}
