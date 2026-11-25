package connectors

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"

	"github.com/jackc/pgx/v5"
)

// The file-based release sealed testdata/release-a under the synthetic key
// "ba" * 32: personalTestOwner's Google grant from an OAuth flow, another
// account's Slack grant with special characters, and a disconnected grant.
const (
	fixtureSlackOwner = "fedcba9876543210fedcba9876543210"
	fixtureDisconnect = "00000000000000000000000000000abc"
)

func personalConfig(t *testing.T, key string) PersonalConfig {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "key"), []byte(key), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret"), []byte("fixture-oauth-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	app := &OAuthApplication{ClientID: "fixture", SecretFile: filepath.Join(dir, "secret")}
	return PersonalConfig{Version: 1, KeyFile: filepath.Join(dir, "key"), Google: app, Slack: personalSlackApp(&OAuthApplication{ClientID: "fixture", SecretFile: app.SecretFile, Workspace: "T123"})}
}

func personalReleaseA(t *testing.T) string {
	t.Helper()
	data := t.TempDir()
	if err := os.CopyFS(data, os.DirFS("testdata/release-a")); err != nil {
		t.Fatal(err)
	}
	return data
}

func importPersonal(t *testing.T, db *pg.DB, data string, files PersonalFiles) (filestate.Sum, error) {
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
	sum, err := files.ImportFiles(ctx, db, tx, src)
	if err == nil {
		err = tx.Commit(ctx)
	}
	return sum, err
}

func exportPersonal(t *testing.T, db *pg.DB, files PersonalFiles) (filestate.Sum, string) {
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
	sum, err := files.ExportFiles(ctx, db, tx, out)
	if err != nil {
		t.Fatal(err)
	}
	return sum, dir
}

// openPersonalFile returns the document a credentials.enc seals.
func openPersonalFile(t *testing.T, config PersonalConfig, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	key, err := personalCipher(config.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := key.Open(nil, raw[:key.NonceSize()], raw[key.NonceSize():], []byte(personalSeal))
	if err != nil {
		t.Fatal("cannot open", path, err)
	}
	return plain
}

// Each imported grant is a row of its own that the store opens, and the export
// seals the document the file-based release sealed, under a new nonce.
func TestPersonalFilesRoundTrip(t *testing.T) {
	requireSlack(t) // the release holds a Slack grant
	config := personalConfig(t, strings.Repeat("ba", 32))
	files := PersonalFiles{Config: &config}
	db := pgtest.Org(t)
	imported, err := importPersonal(t, db, personalReleaseA(t), files)
	if err != nil {
		t.Fatal(err)
	}
	if imported.Files != 1 || len(sealedCredentials(t, db)) != 3 {
		t.Fatalf("import read %d files into %d rows", imported.Files, len(sealedCredentials(t, db)))
	}
	p, err := OpenPersonalConnections(db, config)
	if err != nil {
		t.Fatal(err)
	}
	if got := listConnections(t, p, personalTestOwner); got[0].State != "connected" || got[0].Email != "alice@example.com" || got[1].State != "disconnected" {
		t.Fatalf("the Google grant changed: %+v", got)
	}
	if got := listConnections(t, p, fixtureSlackOwner); got[1].State != "connected" || got[1].Email != "zoë+<test>@example.com" || got[1].Generation != 4 || len(got[1].Scopes) != 2 {
		t.Fatalf("the Slack grant changed: %+v", got)
	}
	if got := listConnections(t, p, fixtureDisconnect); got[0].State != "disconnected" || got[0].Generation != 1 {
		t.Fatalf("the disconnected grant changed: %+v", got)
	}
	slack, _, err := p.load(t.Context(), db.Pool, fixtureSlackOwner, "slack")
	if err != nil || slack.AccessToken != "fixture-access-slack" || slack.RefreshToken != "fixture-refresh-slack" {
		t.Fatal("the Slack tokens were lost", err)
	}
	if p.LinkedOwner("slack", "T123", "U0FIXTURE") != fixtureSlackOwner {
		t.Fatal("the Slack identity is not linked")
	}
	exported, dir := exportPersonal(t, db, files)
	if exported != imported {
		t.Fatalf("export %+v differs from import %+v", exported, imported)
	}
	want := openPersonalFile(t, config, filepath.Join("testdata/release-a", personalFile))
	if got := openPersonalFile(t, config, filepath.Join(dir, personalFile)); !bytes.Equal(got, want) {
		t.Fatalf("the sealed grants changed in the round trip:\n%s\n%s", want, got)
	}
}

// The report an operator keeps reveals nothing about the tokens: its digest is
// keyed with the personal key, so nobody without it can test a guessed grant
// against the report.
func TestPersonalFilesReportIsKeyed(t *testing.T) {
	config := personalConfig(t, strings.Repeat("ba", 32))
	imported, err := importPersonal(t, pgtest.Org(t), personalReleaseA(t), PersonalFiles{Config: &config})
	if err != nil {
		t.Fatal(err)
	}
	plain := openPersonalFile(t, config, filepath.Join("testdata/release-a", personalFile))
	canonical, err := filestate.JSON(plain)
	if err != nil {
		t.Fatal(err)
	}
	var unkeyed filestate.Tally
	unkeyed.Add(personalFile, canonical)
	if imported == unkeyed.Sum() {
		t.Fatal("the report's digest is computed from the grants alone")
	}
}

// A refresh in progress when the gateways stopped exports without its gateway,
// and reads after the export as needing reauthorization.
func TestPersonalExportDropsRefreshHolder(t *testing.T) {
	requireSlack(t) // the release holds a Slack grant
	config := personalConfig(t, strings.Repeat("ba", 32))
	files := PersonalFiles{Config: &config}
	db := pgtest.Org(t)
	if _, err := importPersonal(t, db, personalReleaseA(t), files); err != nil {
		t.Fatal(err)
	}
	p, err := OpenPersonalConnections(db, config)
	if err != nil {
		t.Fatal(err)
	}
	slack, version, err := p.load(t.Context(), db.Pool, fixtureSlackOwner, "slack")
	if err != nil {
		t.Fatal(err)
	}
	slack.Refreshing, slack.RefreshHolder = true, "stopped-gateway"
	if err = p.save(t.Context(), db.Pool, slack, version); err != nil {
		t.Fatal(err)
	}
	_, dir := exportPersonal(t, db, files)
	plain := openPersonalFile(t, config, filepath.Join(dir, personalFile))
	if !bytes.Contains(plain, []byte(`"refreshing":true`)) || bytes.Contains(plain, []byte("refreshHolder")) || bytes.Contains(plain, []byte("stopped-gateway")) {
		t.Fatalf("the interrupted refresh was not exported as the file-based release keeps it:\n%s", plain)
	}
	again := pgtest.Org(t)
	if _, err = importPersonal(t, again, dir, files); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenPersonalConnections(again, config)
	if err != nil {
		t.Fatal(err)
	}
	if got := listConnections(t, reopened, fixtureSlackOwner); got[1].State != "reauthorize" {
		t.Fatalf("the interrupted refresh was reused: %+v", got)
	}
}

// An import refuses grants it cannot open as the file-based release would, and
// a refused import writes no row.
func TestPersonalImportRefusesUnopenedGrants(t *testing.T) {
	right := personalConfig(t, strings.Repeat("ba", 32))
	other := personalConfig(t, strings.Repeat("cd", 32))
	unchanged := func(string) {}
	tamper := func(path string) {
		raw, _ := os.ReadFile(path)
		raw[len(raw)-1] ^= 1
		os.WriteFile(path, raw, 0600)
	}
	truncate := func(path string) { os.WriteFile(path, []byte("short"), 0600) }
	cases := map[string]struct {
		files   PersonalFiles
		corrupt func(path string)
	}{
		"another key":    {PersonalFiles{Config: &other}, unchanged},
		"no key":         {PersonalFiles{}, unchanged},
		"tampered file":  {PersonalFiles{Config: &right}, tamper},
		"truncated file": {PersonalFiles{Config: &right}, truncate},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			data := personalReleaseA(t)
			tc.corrupt(filepath.Join(data, personalFile))
			db := pgtest.Org(t)
			if _, err := importPersonal(t, db, data, tc.files); err == nil {
				t.Fatal("the import accepted it")
			}
			if n := len(sealedCredentials(t, db)); n != 0 {
				t.Fatal("a refused import wrote rows", n)
			}
		})
	}
	if sum, err := importPersonal(t, pgtest.Org(t), t.TempDir(), PersonalFiles{}); err != nil || sum.Files != 0 {
		t.Fatal("an instance without personal connections failed to import", sum, err)
	}
}
