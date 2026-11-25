package automation

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"

	"github.com/jackc/pgx/v5"
)

// releaseA copies the automation files the file-based release wrote in
// testdata/release-a: two configured jobs, a completed, a failed, a running and
// three queued runs (one batching an event, one learning), two effects, a job
// state and incident, repair, event and Drive checkpoints.
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

func fixtureFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir("testdata/release-a", func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			rel, _ := filepath.Rel("testdata/release-a", path)
			files = append(files, rel)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// Imported automation state reaches the queue and definitions, and exports
// back to the files the file-based release wrote, byte for byte.
func TestAutomationFilesRoundTrip(t *testing.T) {
	data := releaseA(t)
	// Neither prepared environments nor an interrupted write's temporary file
	// are automation state.
	os.MkdirAll(filepath.Join(data, "automations", "python", "env"), 0700)
	os.WriteFile(filepath.Join(data, "automations", "python", "env", "ready.json"), []byte(`{}`), 0600)
	os.WriteFile(filepath.Join(data, "automations", ".pending-123"), []byte(`{"partial`), 0600)
	db := pgtest.Org(t)
	imported, err := importFiles(t, db, data)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(fixtureFiles(t)); imported.Files != want {
		t.Fatalf("import read %d files, want %d", imported.Files, want)
	}
	exported, dir := exportFiles(t, db)
	if exported != imported {
		t.Fatalf("export %+v differs from import %+v", exported, imported)
	}
	for _, rel := range fixtureFiles(t) {
		want, _ := os.ReadFile(filepath.Join("testdata/release-a", rel))
		got, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s changed in the round trip (%v):\n%s\n%s", rel, err, want, got)
		}
	}

	runtime, err := OpenRuntime(db)
	if err != nil {
		t.Fatal(err)
	}
	definitions := runtime.Definitions.List()
	if len(definitions) != 2 || definitions[1].Spec.ID != "summary" || !strings.Contains(definitions[1].Spec.Instruction, "“the week” — naïvely & <briefly>") {
		t.Fatalf("configured jobs were lost: %+v", definitions)
	}
	runs, err := runtime.Queue.List()
	if err != nil || len(runs) != 6 {
		t.Fatalf("runs were lost: %v %d", err, len(runs))
	}
	states := map[string]int{}
	var sweep Run
	for _, r := range runs {
		states[r.State]++
		if r.Spec.ID == "sweep" {
			sweep = r
		}
		if r.State == "completed" && r.Result != "Digest: “quotes” & <html> ünïcode\nline two" {
			t.Fatalf("a run's result changed: %q", r.Result)
		}
	}
	// Opening queues the interrupted run again, as the file-based release did.
	if states["completed"] != 1 || states["failed"] != 1 || states["queued"] != 4 || sweep.State != "queued" {
		t.Fatalf("run states after opening: %v, sweep %s", states, sweep.State)
	}
	// The fixture's checkpoints belong to this definition, as written.
	daily := Spec{ID: "daily", Kind: "digest", Actor: "robot", Project: "notes", Output: "report.md", Calls: []Call{{Connection: "metrics", Operation: "read", Arguments: map[string]any{"days": 1, "label": "Café “weekly” <b>&</b>"}}}}
	if state, err := runtime.Queue.JobState(daily); err != nil || string(state.Data) != `{"cursor":"2026-10-02","seen":["a","b"]}` {
		t.Fatalf("job state was lost: %s %v", state.Data, err)
	}
	if memo, err := runtime.Queue.RepairMemo(daily); err != nil || memo.Report.Summary != "All checks passed" {
		t.Fatalf("repair checkpoint was lost: %+v %v", memo, err)
	}
	if drive, err := runtime.Queue.ReadDrive("drive-sync"); err != nil || drive.Files["f1"].Path != "Notes/Ré sumé.md" {
		t.Fatalf("Drive checkpoint was lost: %+v %v", drive, err)
	}
	if effects, err := runtime.Queue.Effects("daily-scope"); err != nil || len(effects) != 1 || effects[0].State != "accepted" {
		t.Fatalf("effects were lost: %+v %v", effects, err)
	}
	if _, _, err := runtime.Queue.BeginEffect(Effect{Scope: "ask-scope", Action: "reply/2", InputHash: Hash("other"), Actor: "alice"}); err == nil {
		t.Fatal("an unresolved imported effect no longer blocks its scope")
	}

	// A run this gateway is running exports without its claimant, as running
	// work the file-based release queues again when it opens.
	ran, err := runtime.Queue.RunMatching(t.Context(), func(s Spec) bool { return s.ID == "sweep" }, func(ctx context.Context, r *Run, save func() error) error {
		_, dir := exportFiles(t, db)
		raw, err := os.ReadFile(filepath.Join(dir, "automations", r.ID+".json"))
		if err != nil {
			return err
		}
		if bytes.Contains(raw, []byte("claimant")) {
			t.Errorf("the export kept the claimant:\n%s", raw)
		}
		if r, err := decodeRun(r.ID, raw); err != nil || r.State != "running" {
			t.Errorf("the export of a running run: %+v %v", r.State, err)
		}
		// The export passes the file-based release's checks.
		if _, err := importFiles(t, pgtest.Org(t), dir); err != nil {
			t.Error("the export does not import again:", err)
		}
		return nil
	})
	if err != nil || !ran {
		t.Fatal("the requeued run did not run", err)
	}
}

// An import refuses a file the file-based release would refuse, and writes
// nothing.
func TestAutomationImportRefusesInvalidFiles(t *testing.T) {
	run := func(data string) string {
		files, _ := filepath.Glob(filepath.Join(data, "automations", "*.json"))
		return files[0]
	}
	cases := map[string]func(data string){
		"truncated run": func(data string) {
			raw, _ := os.ReadFile(run(data))
			os.WriteFile(run(data), raw[:len(raw)/2], 0600)
		},
		"newer run version": func(data string) {
			raw, _ := os.ReadFile(run(data))
			os.WriteFile(run(data), bytes.Replace(raw, []byte(`"version": 5`), []byte(`"version": 9`), 1), 0600)
		},
		"run under another name": func(data string) {
			os.Rename(run(data), filepath.Join(data, "automations", strings.Repeat("0", 64)+".json"))
		},
		"corrupt effect": func(data string) {
			files, _ := filepath.Glob(filepath.Join(data, "automations", "*.effect"))
			os.WriteFile(files[0], []byte(`{"id":`), 0600)
		},
		"unknown file": func(data string) {
			os.WriteFile(filepath.Join(data, "automations", "notes.txt"), []byte("stray"), 0600)
		},
		"invalid definitions": func(data string) {
			os.WriteFile(filepath.Join(data, definitionsFile), []byte(`{"version":1,"jobs":null}`), 0600)
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
			if err := db.Pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM automation_records WHERE org = $1) + (SELECT count(*) FROM documents WHERE org = $1)", db.Org).Scan(&n); err != nil || n != 0 {
				t.Fatal("a refused import wrote state", n, err)
			}
		})
	}
	if sum, err := importFiles(t, pgtest.Org(t), t.TempDir()); err != nil && !errors.Is(err, os.ErrNotExist) || sum.Files != 0 {
		t.Fatal("an instance without automations failed to import", sum, err)
	}
}
