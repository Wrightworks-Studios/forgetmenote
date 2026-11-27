package decision

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"

	"github.com/jackc/pgx/v5"
)

// The file-based release wrote testdata/release-a: a ledger with a settled, an
// unknown and a zero-output call, and one record of each kind, among them
// scores it wrote in exponent form, an integer past 2^53 and special characters.
const (
	fixtureJob    = "026ab639c21df8aa80e5789370a9db1b8b524b776fd8a0c2ba71efb377ecc7d9"
	fixtureResult = "results/" + fixtureJob + "-00000000.json"
	fixtureNotice = "notifications/81b637d8fcd2c6da6359e6963113a1170de795e4b725b84d1e0b4cfd9ec58ce9.json"
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

// The imported ledger keeps its order and refuses a second reservation of an
// imported attempt, records read back with their exact values, and the export
// is the fixture: the ledger byte for byte, each record in canonical form.
func TestJudgmentFilesRoundTrip(t *testing.T) {
	data := releaseA(t)
	// Interrupted writes leave temporary files the release never reads.
	os.WriteFile(filepath.Join(data, "jev", usageTemp+"123"), []byte(`{"version":`), 0600)
	os.WriteFile(filepath.Join(data, "jev", "jobs", recordTemp+"456"), []byte(`{"version":`), 0600)
	db := pgtest.Org(t)
	imported, err := importFiles(t, db, data)
	if err != nil {
		t.Fatal(err)
	}
	if imported.Files != 7 {
		t.Fatalf("import read %d files", imported.Files)
	}
	ledger, err := OpenLedger(db)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := ledger.Records()
	if err != nil || len(usage) != 3 || usage[0].Status != "settled" || usage[0].Tokens() != 942 || usage[1].Status != "unknown" || usage[1].Scope.Chat != "chat-7" || usage[2].Usage == nil {
		t.Fatalf("the ledger changed: %+v %v", usage, err)
	}
	if _, err = ledger.ReserveExternal(usage[0].Scope, "fixture-model", []byte("again"), 1); !errors.Is(err, ErrAlreadyAttempted) {
		t.Fatal("an imported attempt was reserved again:", err)
	}
	records, err := OpenRecords(db)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Version int    `json:"version"`
		Job     string `json:"job"`
		Index   int    `json:"index"`
		Hash    string `json:"hash"`
		Result  struct {
			Scores map[string]float64 `json:"scores"`
			Text   string             `json:"text"`
		} `json:"result"`
	}
	if err = records.Read(fixtureResult, &result); err != nil || result.Result.Scores["tiny"] != 1.2e-7 || result.Result.Scores["large"] != 3e21 || result.Result.Text != "line\nbreak and <tag> & \"quotes\"" {
		t.Fatalf("the result record changed: %+v %v", result, err)
	}
	var notice struct {
		Version int    `json:"version"`
		Items   []any  `json:"items"`
		Read    uint64 `json:"read"`
	}
	if err = records.Read(fixtureNotice, &notice); err != nil || notice.Read != 18446744073709551615 {
		t.Fatalf("the notification record lost digits: %+v %v", notice, err)
	}
	if jobs, err := records.List("jobs/", 2000); err != nil || len(jobs) != 1 || jobs[0] != "jobs/"+fixtureJob+".json" {
		t.Fatal("the job is not listed", jobs, err)
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
		if err != nil {
			return err
		}
		if rel != filepath.Join(jevDir, usageFile) {
			want, _ = canonicalRecord(want)
			got, _ = canonicalRecord(got)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s changed in the round trip:\n%s\n%s", rel, want, got)
		}
		return nil
	})
	if err != nil || written != 7 {
		t.Fatal("export wrote", written, err)
	}
}

// A call still reserved when the gateways stopped exports as unknown, as the
// file-based release would record it, so the export imports again.
func TestJudgmentExportRecordsReservedCallsAsUnknown(t *testing.T) {
	db := pgtest.Org(t)
	if _, err := importFiles(t, db, releaseA(t)); err != nil {
		t.Fatal(err)
	}
	ledger, err := OpenLedger(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ledger.ReserveExternal(Scope{Actor: "alice", Notebook: "work", Feature: "rerank", Run: "run-4", Operation: "rank-1"}, "fixture-model", []byte("in flight"), 5); err != nil {
		t.Fatal(err)
	}
	_, dir := exportFiles(t, db)
	raw, _ := os.ReadFile(filepath.Join(dir, jevDir, usageFile))
	if bytes.Contains(raw, []byte(`"reserved"`)) || bytes.Count(raw, []byte(`"status":"unknown"`)) != 2 {
		t.Fatalf("the reserved call was not recorded as unknown:\n%s", raw)
	}
	if _, err = importFiles(t, pgtest.Org(t), dir); err != nil {
		t.Fatal(err)
	}
}

// An import refuses what the file-based release would refuse or repair first,
// and a refused import writes no row.
func TestJudgmentImportRefusesUnreadableFiles(t *testing.T) {
	edit := func(rel, old, new string) func(string) {
		return func(data string) {
			path := filepath.Join(data, rel)
			raw, _ := os.ReadFile(path)
			os.WriteFile(path, bytes.Replace(raw, []byte(old), []byte(new), 1), 0600)
		}
	}
	cases := map[string]func(data string){
		"reserved call":       edit("jev/usage.json", `"status":"unknown"`, `"status":"reserved"`),
		"ledger version":      edit("jev/usage.json", `{"version":1`, `{"version":2`),
		"changed attempt":     edit("jev/usage.json", `"attempt":1`, `"attempt":2`),
		"unknown field":       edit("jev/usage.json", `"estimate":1200`, `"estimate":1200,"cost":3`),
		"duplicate key":       edit("jev/"+fixtureNotice, `{"items"`, `{"version":1,"items"`),
		"trailing document":   edit("jev/"+fixtureResult, `"version":1}`, `"version":1}{}`),
		"unknown record kind": func(data string) { os.Mkdir(filepath.Join(data, "jev", "drafts"), 0700) },
		"stray record file":   func(data string) { os.WriteFile(filepath.Join(data, "jev", "users", "x.txt"), nil, 0600) },
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
			if err := db.Pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM jev_usage WHERE org = $1) + (SELECT count(*) FROM jev_records WHERE org = $1)", db.Org).Scan(&n); err != nil || n != 0 {
				t.Fatal("a refused import wrote rows", n, err)
			}
		})
	}
	if sum, err := importFiles(t, pgtest.Org(t), t.TempDir()); err != nil || sum.Files != 0 {
		t.Fatal("an instance without judgments failed to import", sum, err)
	}
}

// The report compares each number by its exact value: jsonb's spelling of a
// number matches the file's, and no two different numbers match.
func TestJudgmentReportComparesNumbersExactly(t *testing.T) {
	canonical := func(raw string) string {
		t.Helper()
		out, err := canonicalRecord([]byte(raw))
		if err != nil {
			t.Fatal(raw, err)
		}
		return string(out)
	}
	for _, same := range [][2]string{
		{`{"score":1.2e-7,"count":9007199254740993}`, `{"count":9007199254740993,"score":0.00000012}`},
		{`[1.50, 1e3, -0, 2E+2, 0.0]`, `[1.5, 1000, 0, 200, 0]`},
	} {
		if canonical(same[0]) != canonical(same[1]) {
			t.Errorf("equal values differ: %s %s", canonical(same[0]), canonical(same[1]))
		}
	}
	for _, different := range [][2]string{
		{`{"count":9007199254740993}`, `{"count":9007199254740992}`},
		{`{"score":0.1000000000000000055511151231257827}`, `{"score":0.1}`},
		{`{"value":1}`, `{"value":"1"}`},
	} {
		if canonical(different[0]) == canonical(different[1]) {
			t.Errorf("different values match: %s", canonical(different[0]))
		}
	}
	if _, err := canonicalRecord([]byte(`{"huge":1e100000}`)); err == nil {
		t.Error("a number no record holds was compared")
	}
}
