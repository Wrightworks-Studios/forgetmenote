package sources

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/blob/blobtest"
	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"

	"github.com/jackc/pgx/v5"
)

// The fixture's jobs, written by the file-based release's own worker: a ready
// job with its original and result, a failed, a canceled, a running and a
// queued one. Their expiry was moved a century ahead so they stay live.
const (
	readyJob    = "baa54751c2854df4831842bdf89a96d6"
	failedJob   = "3e71da49f40e9276a01ac2e2733d2271"
	canceledJob = "f4aa7e763cfd9189e0f1a909c2a72df4"
	runningJob  = "90dae69d42748368bf4b1780c37eb0c1"
	queuedJob   = "268ae50d94c8a020cdb3344c8fae5b13"
)

var fixtureJobs = []string{readyJob, failedJob, canceledJob, runningJob, queuedJob}

func releaseA(t *testing.T) string {
	t.Helper()
	data := t.TempDir()
	if err := os.CopyFS(data, os.DirFS("testdata/release-a")); err != nil {
		t.Fatal(err)
	}
	return data
}

// importFiles imports data's source jobs, storing their blob files in blobs,
// or leaving them in data when blobs is nil.
func importFiles(t *testing.T, db *pg.DB, data string, blobs blob.Store) (filestate.Sum, error) {
	t.Helper()
	src, err := filestate.OpenSource(data)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	src.Blobs = blobs
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

func exportFiles(t *testing.T, db *pg.DB, blobs blob.Store) (filestate.Sum, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "export")
	out, err := filestate.CreateTarget(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	out.Blobs = blobs
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

// Imported manifests publish their jobs, whose files stay in place, and export
// back to the files the file-based release wrote, byte for byte.
func TestSourceJobFilesRoundTrip(t *testing.T) {
	data := releaseA(t)
	root := filepath.Join(data, jobsDir)
	// An interrupted admission and an unacknowledged upload hold no manifest;
	// both releases remove them when they open.
	for _, dir := range []string{".staging-" + strings.Repeat("a", 32), strings.Repeat("c", 32)} {
		os.Mkdir(filepath.Join(root, dir), 0700)
		os.WriteFile(filepath.Join(root, dir, "original"), []byte("unacknowledged"), 0600)
	}
	db := pgtest.Org(t)
	store := dataStore(t, data)
	imported, err := importFiles(t, db, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Five manifests, and the ready job's original and result.
	if imported.Files != len(fixtureJobs)+2 {
		t.Fatalf("import read %d files", imported.Files)
	}
	exported, dir := exportFiles(t, db, store)
	if exported != imported {
		t.Fatalf("export %+v differs from import %+v", exported, imported)
	}
	sameFiles(t, "testdata/release-a", dir)

	started, release := make(chan struct{}, 1), make(chan struct{})
	defer close(release)
	jobs, err := OpenJobs(db, store, data, func(ctx context.Context, scope JobScope) error {
		if _, worker := ctx.Deadline(); worker && scope.Notebook == "slow" {
			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer jobs.Close()
	notes := JobScope{Subject: "alice", Notebook: "notes", Generation: 3}
	capture, err := jobs.Result(t.Context(), notes, readyJob)
	if err != nil || !strings.Contains(capture.Text, `Amber & \<cobalt\> — “quoted”`) || len(capture.Original) == 0 {
		t.Fatalf("the ready job's result was lost: %q %v", capture.Text, err)
	}
	want := map[string]string{failedJob: "failed", queuedJob: "interrupted"}
	for id, state := range want {
		if status, err := jobs.Status(t.Context(), notes, id); err != nil || status.State != state {
			t.Fatalf("job %s is %+v, want %s: %v", id, status, state, err)
		}
	}
	// Opening interrupts the jobs the stopped release was running or had
	// queued, as that release did.
	slow := JobScope{Subject: "bob", Notebook: "slow", Generation: 1}
	if status, err := jobs.Status(t.Context(), slow, runningJob); err != nil || status.State != "interrupted" {
		t.Fatalf("the running job is %+v: %v", status, err)
	}
	held := JobScope{Subject: "bob", Notebook: "held", Generation: 1}
	if status, err := jobs.Status(t.Context(), held, canceledJob); err != nil || status.State != "canceled" || status.Warning == "" {
		t.Fatalf("the canceled job is %+v: %v", status, err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != len(fixtureJobs) {
		t.Fatalf("unpublished directories were kept: %v", entries)
	}

	// A job this gateway runs exports without its holder, as running work the
	// file-based release interrupts when it opens.
	if _, err = jobs.Resume(t.Context(), slow, runningJob); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the resumed job did not start")
	}
	_, dir = exportFiles(t, db, store)
	raw, err := os.ReadFile(filepath.Join(dir, manifestFile(runningJob)))
	if err != nil {
		t.Fatal(err)
	}
	var job sourceJob
	if err = strictRecord(raw, &job); err != nil || job.State != "running" || job.Holder != "" || bytes.Contains(raw, []byte("holder")) {
		t.Fatalf("the running job exported as %s: %v", raw, err)
	}
	// The export passes the file-based release's checks.
	if _, err = importFiles(t, pgtest.Org(t), dir, nil); err != nil {
		t.Fatal("the export does not import again:", err)
	}
}

// An import refuses what the file-based release refuses when it opens, and
// writes nothing.
func TestSourceJobImportRefusesInvalidFiles(t *testing.T) {
	cases := map[string]func(root string){
		"unknown entry": func(root string) {
			os.WriteFile(filepath.Join(root, "notes.txt"), []byte("stray"), 0600)
		},
		"truncated manifest": func(root string) {
			os.WriteFile(filepath.Join(root, readyJob, "job.json"), []byte(`{"version":1,`), 0600)
		},
		"unknown field": func(root string) {
			raw, _ := os.ReadFile(filepath.Join(root, readyJob, "job.json"))
			os.WriteFile(filepath.Join(root, readyJob, "job.json"), bytes.Replace(raw, []byte(`{"version":1,`), []byte(`{"version":1,"holder":"gateway",`), 1), 0600)
		},
		"another job's manifest": func(root string) {
			raw, _ := os.ReadFile(filepath.Join(root, readyJob, "job.json"))
			os.WriteFile(filepath.Join(root, queuedJob, "job.json"), raw, 0600)
		},
		"unknown state": func(root string) {
			raw, _ := os.ReadFile(filepath.Join(root, readyJob, "job.json"))
			os.WriteFile(filepath.Join(root, readyJob, "job.json"), bytes.Replace(raw, []byte(`"state":"ready"`), []byte(`"state":"paused"`), 1), 0600)
		},
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			data := releaseA(t)
			corrupt(filepath.Join(data, "source-jobs"))
			db := pgtest.Org(t)
			if _, err := importFiles(t, db, data, nil); err == nil {
				t.Fatal("the import accepted it")
			}
			var n int
			if err := db.Pool.QueryRow(context.Background(), "SELECT count(*) FROM source_jobs WHERE org = $1", db.Org).Scan(&n); err != nil || n != 0 {
				t.Fatal("a refused import wrote jobs", n, err)
			}
		})
	}
	if sum, err := importFiles(t, pgtest.Org(t), t.TempDir(), nil); err != nil || sum.Files != 0 {
		t.Fatal("an instance without source jobs failed to import", sum, err)
	}
}

// sameFiles fails unless every file under dir is the file at the same path
// under want, byte for byte, and dir has them all.
func sameFiles(t *testing.T, want, dir string) {
	t.Helper()
	files := map[string][]byte{}
	for _, root := range []string{want, dir} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			raw, err := os.ReadFile(path)
			if root == want {
				files[rel] = raw
			} else if expected, ok := files[rel]; !ok || !bytes.Equal(raw, expected) {
				t.Fatalf("%s changed in the round trip:\n%s\n%s", rel, expected, raw)
			} else {
				delete(files, rel)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(files) != 0 {
		t.Fatalf("the round trip lost %d files", len(files))
	}
}

// Jobs imported into S3 are served from it by a gateway with a disk of its
// own, and export to the file-based release's files. The import and export
// report what an import that leaves the files in the data directory reports.
func TestSourceJobFilesRoundTripThroughS3(t *testing.T) {
	data := releaseA(t)
	// Opening the source creates its catalog.lock; nothing else is written.
	entries := func() (n int) {
		filepath.WalkDir(filepath.Join(data, jobsDir), func(string, fs.DirEntry, error) error {
			n++
			return nil
		})
		return n
	}
	before := entries()
	inPlace, err := importFiles(t, pgtest.Org(t), data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if after := entries(); after != before {
		t.Fatalf("an import into the data directory wrote %d entries", after-before)
	}

	db, s3 := pgtest.Org(t), blobtest.Start(t)
	store := s3.Store(t, "instance")
	imported, err := importFiles(t, db, data, store)
	if err != nil {
		t.Fatal(err)
	}
	if imported != inPlace {
		t.Fatalf("the report depends on where the files went: %+v %+v", imported, inPlace)
	}
	objects := s3.Objects()
	for _, name := range []string{"original", "result.json"} {
		want, _ := os.ReadFile(filepath.Join(data, jobsDir, readyJob, name))
		if got, ok := objects["instance/"+jobsDir+"/"+readyJob+"/"+name]; !ok || !bytes.Equal(got, want) {
			t.Fatalf("%s was not stored as it was: %q", name, got)
		}
	}
	if len(objects) != 2 {
		t.Fatalf("stored %d objects, want the ready job's original and result", len(objects))
	}
	exported, dir := exportFiles(t, db, store)
	if exported != imported {
		t.Fatalf("export %+v differs from import %+v", exported, imported)
	}
	sameFiles(t, "testdata/release-a", dir)

	jobs, err := OpenJobs(db, store, t.TempDir(), func(ctx context.Context, _ JobScope) error { return ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	defer jobs.Close()
	capture, err := jobs.Result(t.Context(), JobScope{Subject: "alice", Notebook: "notes", Generation: 3}, readyJob)
	if want, _ := os.ReadFile(filepath.Join(data, jobsDir, readyJob, "original")); err != nil || !bytes.Equal(capture.Original, want) {
		t.Fatalf("the imported job's result is not served from S3: %v", err)
	}
	// The imported jobs are charged their stored files, as that release did.
	var reserved int64
	var raw []byte
	if err = db.Pool.QueryRow(t.Context(), `SELECT reserved, manifest FROM source_jobs WHERE org = $1 AND id = $2`, db.Org, readyJob).Scan(&reserved, &raw); err != nil {
		t.Fatal(err)
	}
	job, err := decodeJob(readyJob, 1, reserved, raw)
	if err != nil || job.Stored != int64(len(objects["instance/"+jobsDir+"/"+readyJob+"/original"])+len(objects["instance/"+jobsDir+"/"+readyJob+"/result.json"])) {
		t.Fatalf("the ready job stores %d bytes: %v", job.Stored, err)
	}
}
