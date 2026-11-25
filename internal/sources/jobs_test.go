package sources

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
)

// seedJob publishes a job as an earlier process left it: its original in
// blobs and its manifest row.
func seedJob(t *testing.T, db *pg.DB, blobs blob.Store, job sourceJob, original []byte) {
	t.Helper()
	if err := blob.Write(t.Context(), blobs, jobsDir+"/"+job.ID+"/original", original); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(t.Context(), `INSERT INTO source_jobs (org, id, version, reserved, manifest) VALUES ($1, $2, 1, 0, $3)`, db.Org, job.ID, raw); err != nil {
		t.Fatal(err)
	}
}

// dataStore is blob storage in the data directory, the file-based layout.
func dataStore(t *testing.T, data string) *blob.Dir {
	t.Helper()
	store, err := blob.OpenDir(data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// scratchGlob matches a file a conversion of any job left in the gateway's
// working directory under data.
func scratchGlob(data, name string) string {
	return filepath.Join(data, "cache", jobsDir, "*", "forgetmenote-pdf-*", name)
}

func TestSourceJobsRecoverInterruptedPublicationAndRetirement(t *testing.T) {
	data := t.TempDir()
	root := filepath.Join(data, jobsDir)
	for _, name := range []string{".staging-" + strings.Repeat("a", 32), ".retired-" + strings.Repeat("b", 32), strings.Repeat("c", 32)} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "original"), []byte("unacknowledged upload"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := OpenJobs(pgtest.Org(t), dataStore(t, data), data, func(ctx context.Context, _ JobScope) error { return ctx.Err() })
	if err != nil {
		t.Fatalf("interrupted admission/retirement prevented reopen: %v", err)
	}
	defer jobs.Close()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("intermediate private data retained: %v %v", entries, err)
	}
}

func TestSourceJobsRetainedStorageAdmission(t *testing.T) {
	t.Setenv("FMN_LIMIT_SOURCE_JOB_COUNT", "1")
	data := t.TempDir()
	db, store := pgtest.Org(t), dataStore(t, data)
	authorize := func(ctx context.Context, _ JobScope) error { return ctx.Err() }
	jobs, err := OpenJobs(db, store, data, authorize)
	if err != nil {
		t.Fatal(err)
	}
	scope := JobScope{Subject: "user", Notebook: "notes", Generation: 1}
	first, err := jobs.Submit(t.Context(), scope, JobInput{Filename: "one.txt"}, []byte("Amber"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Wait(t.Context(), scope, first.ID); err != nil {
		t.Fatal(err)
	}
	jobs.Close()
	jobs, err = OpenJobs(db, store, data, authorize)
	if err != nil {
		t.Fatal(err)
	}
	defer jobs.Close()
	if _, err := jobs.Submit(t.Context(), scope, JobInput{Filename: "two.txt"}, []byte("Cobalt")); !errors.Is(err, ErrJobCapacity) {
		t.Fatalf("retained job did not count after restart: %v", err)
	}
	t.Setenv("FMN_LIMIT_SOURCE_JOB_COUNT", "100")
	t.Setenv("FMN_LIMIT_SOURCE_JOB_BYTES", "1024")
	if _, err := jobs.Submit(t.Context(), scope, JobInput{Filename: "two.txt"}, []byte("Cobalt")); !errors.Is(err, ErrJobCapacity) {
		t.Fatalf("storage admitted work without scratch/checkpoint reservation: %v", err)
	}
}

func TestSourceJobsExpiryDrainsWorker(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"pdftotext": "printf '<html><body><doc><page width=\"612\" height=\"792\"/></doc></body></html>\\n'", "pdfimages": "printf '1 0 image\\n'", "pdftoppm": "touch started\nexec /bin/sleep 30"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	data := t.TempDir()
	marker := scratchGlob(data, "started")
	id := strings.Repeat("a", 32)
	db, store := pgtest.Org(t), dataStore(t, data)
	original := []byte("%PDF-fixture")
	scope := JobScope{Subject: "user", Notebook: "notes", Generation: 1}
	// Each extractor starts through the sandbox re-exec; leave a loaded race build
	// time to reach rendering before the job expires.
	seedJob(t, db, store, sourceJob{Version: 1, JobStatus: JobStatus{ID: id, State: "canceled", Expires: time.Now().Add(3 * time.Second)}, Scope: scope, Input: JobInput{Filename: "scan.pdf"}, Hash: workspace.ContentHash(original)}, original)
	jobs, err := OpenJobs(db, store, data, func(ctx context.Context, _ JobScope) error { return ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	defer jobs.Close()
	if _, err := jobs.Resume(t.Context(), scope, id); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2500 * time.Millisecond)
	for {
		if len(scratchFiles(t, marker)) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture never reached rendering")
		}
		time.Sleep(time.Millisecond * 5)
	}
	next, err := jobs.Submit(t.Context(), scope, JobInput{Filename: "next.txt"}, []byte("Cobalt"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := jobs.Wait(ctx, scope, next.ID); err != nil {
		t.Fatalf("expired execution retained the sole worker: %v", err)
	}
}

func TestSourceJobsExpiredActiveCanBeCanceled(t *testing.T) {
	// Model a job at its deadline while its child is still draining.
	data := t.TempDir()
	jobs := &Jobs{db: pgtest.Org(t), blobs: dataStore(t, data), scratch: filepath.Join(data, "cache", jobsDir), authorize: func(ctx context.Context, _ JobScope) error { return ctx.Err() }}
	id := strings.Repeat("d", 32)
	scope := JobScope{Subject: "user", Notebook: "notes", Generation: 1}
	seedJob(t, jobs.db, jobs.blobs, sourceJob{Version: 1, JobStatus: JobStatus{ID: id, State: "running", Expires: time.Now().Add(-time.Second)}, Scope: scope}, nil)
	jobs.active = id
	canceled := false
	jobs.cancel = func() { canceled = true }
	if _, err := jobs.Cancel(t.Context(), scope, id); err != nil || !canceled {
		t.Fatalf("expired active cancellation refused: %v", err)
	}
}

func TestSourceJobsAdmissionCancellationAndRevocation(t *testing.T) {
	dir := t.TempDir()
	bbox := `<html><body><doc><page width="612" height="792"><flow><block><line><word xMin="10" yMin="10" xMax="30" yMax="20">Amber checkpoint</word></line></block></flow></page><page width="612" height="792"/></doc></body></html>`
	for name, body := range map[string]string{"pdftotext": "cat <<'XML'\n" + bbox + "\nXML\n", "pdfimages": "printf '2 0 image\\n'\n", "pdftoppm": "touch started\nexec /bin/sleep 30\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var revoked atomic.Bool
	authorize := func(ctx context.Context, scope JobScope) error {
		if revoked.Load() {
			return workspace.ErrForbidden
		}
		return ctx.Err()
	}
	data := t.TempDir()
	marker := scratchGlob(data, "started")
	jobs, err := OpenJobs(pgtest.Org(t), dataStore(t, data), data, authorize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(jobs.Close)
	scope := JobScope{Subject: "user", Notebook: "notes", Generation: 1}
	active, err := jobs.Submit(t.Context(), scope, JobInput{Filename: "scan.pdf"}, []byte("%PDF-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if len(scratchFiles(t, marker)) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not start OCR")
		}
		time.Sleep(time.Millisecond * 5)
	}
	var queued []JobStatus
	for range 4 {
		job, err := jobs.Submit(t.Context(), scope, JobInput{Filename: "queued.txt"}, []byte("Cobalt queued"))
		if err != nil {
			t.Fatal(err)
		}
		queued = append(queued, job)
	}
	if _, err := jobs.Submit(t.Context(), scope, JobInput{Filename: "excess.txt"}, []byte("Too many")); !errors.Is(err, ErrJobCapacity) {
		t.Fatal("queue exceeded four waiting jobs", err)
	}
	if _, err := jobs.Cancel(t.Context(), scope, queued[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Submit(t.Context(), scope, JobInput{Filename: "replacement.txt"}, []byte("Replacement")); err != nil {
		t.Fatal("canceled queued job retained admission", err)
	}
	revoked.Store(true)
	if _, err := jobs.Status(t.Context(), scope, active.ID); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("revoked subject could read job", err)
	}
	revoked.Store(false)
	if _, err := jobs.Cancel(t.Context(), scope, active.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSourceJobsRestartResumeUsesCompletedPages(t *testing.T) {
	dir := t.TempDir()
	bbox := `<html><body><doc><page width="612" height="792"><flow><block><line><word xMin="10" yMin="10" xMax="30" yMax="20">Amber checkpoint</word></line></block></flow></page><page width="612" height="792"/></doc></body></html>`
	for name, body := range map[string]string{"pdftotext": "cat <<'XML'\n" + bbox + "\nXML\n", "pdfimages": "printf '2 0 image\\n'\n", "pdftoppm": "touch started\nexec /bin/sleep 30\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	authorize := func(ctx context.Context, scope JobScope) error { return ctx.Err() }
	scope := JobScope{Subject: "user", Notebook: "notes", Generation: 1}
	data := t.TempDir()
	db, store := pgtest.Org(t), dataStore(t, data)
	marker := scratchGlob(data, "started")
	jobs, err := OpenJobs(db, store, data, authorize)
	if err != nil {
		t.Fatal(err)
	}
	job, err := jobs.Submit(t.Context(), scope, JobInput{Filename: "scan.pdf"}, []byte("%PDF-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if len(scratchFiles(t, marker)) > 0 {
			break
		}
		if time.Now().After(deadline) {
			jobs.Close()
			t.Fatal("worker did not reach second physical page")
		}
		time.Sleep(time.Millisecond * 5)
	}
	started := time.Now()
	jobs.Close()
	if time.Since(started) > time.Second {
		t.Fatal("shutdown did not drain cancellation promptly")
	}
	// Change the first-page extractor fixture: resumed work must use its persisted
	// page, while the unfinished page renders now. No notebook store is involved.
	if err := os.WriteFile(filepath.Join(dir, "pdftotext"), []byte("#!/bin/sh\ncat <<'XML'\n"+strings.ReplaceAll(bbox, "Amber checkpoint", "Discarded new extraction")+"\nXML\n"), 0700); err != nil {
		t.Fatal(err)
	}
	white := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			white.Set(x, y, color.White)
		}
	}
	pngFile := filepath.Join(dir, "white.png")
	f, err := os.Create(pngFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, white); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pdftoppm"), []byte("#!/bin/sh\nfor last; do :; done\ncp '"+pngFile+"' \"$last.png\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	resumed, err := OpenJobs(db, store, data, authorize)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	status, err := resumed.Status(t.Context(), scope, job.ID)
	if err != nil || status.State != "interrupted" || status.Pages != 1 {
		t.Fatalf("restart lost checkpoint: %+v %v", status, err)
	}
	if _, err := resumed.Resume(t.Context(), scope, job.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	capture, err := resumed.Wait(ctx, scope, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(capture.Text, "Amber checkpoint") || strings.Contains(capture.Text, "Discarded new") || !strings.Contains(capture.Text, "## Page 2") {
		t.Fatal("completed page was repeated or lost", capture.Text)
	}
	scope.Notebook = "other"
	if _, err := resumed.Result(t.Context(), scope, job.ID); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("cross-notebook result exposed", err)
	}
	scope.Notebook = "notes"
	scope.Generation = 2
	if _, err := resumed.Status(t.Context(), scope, job.ID); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("old authority resumed", err)
	}
}

// The helper deliberately has no graceful Close: SIGKILL must bypass extractor
// defers, exactly as an instance crash does.
func TestSourceJobsRecoverKilledConversionScratch(t *testing.T) {
	if root := os.Getenv("FMN_TEST_CRASH_JOB_ROOT"); root != "" {
		marker := os.Getenv("FMN_TEST_CRASH_SCRATCH_MARKER")
		ready := marker + ".ready"
		db, err := pg.Open(t.Context(), os.Getenv("FMN_TEST_CRASH_JOB_DATABASE"), "")
		if err != nil {
			t.Fatal(err)
		}
		store, err := blob.OpenDir(root)
		if err != nil {
			t.Fatal(err)
		}
		jobs, err := OpenJobs(db, store, root, func(ctx context.Context, _ JobScope) error {
			// The sandboxed stub can only write in its scratch working directory;
			// this unsandboxed helper relays where that directory is.
			if found := scratchFiles(t, scratchGlob(root, "scratch-path")); len(found) > 0 {
				raw, err := os.ReadFile(found[0])
				if err == nil {
					err = os.WriteFile(marker, raw, 0600)
				}
				if err == nil {
					err = os.WriteFile(ready, []byte("conversion paused"), 0600)
				}
				if err != nil {
					return err
				}
				<-ctx.Done()
			}
			return ctx.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := jobs.Submit(t.Context(), JobScope{Subject: "user", Notebook: "notes", Generation: 1}, JobInput{Filename: "scan.pdf"}, []byte("%PDF-fixture")); err != nil {
			t.Fatal(err)
		}
		<-jobs.done
		return
	}
	root, tools := t.TempDir(), t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tools, "scratch")
	for name, body := range map[string]string{
		"pdftotext": "printf '%s' \"$PWD\" > scratch-path\nprintf '<html><body><doc><page width=\"612\" height=\"792\"/></doc></body></html>\n'",
		"pdfimages": "exit 0",
	} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSourceJobsRecoverKilledConversionScratch$", "-test.timeout=15s")
	database := pgtest.URL(t, true)
	cmd.Env = append(os.Environ(), "FMN_TEST_CRASH_JOB_ROOT="+root, "FMN_TEST_CRASH_JOB_DATABASE="+database, "FMN_TEST_CRASH_SCRATCH_MARKER="+marker, "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker + ".ready"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("conversion helper never reached checkpoint")
		}
		time.Sleep(5 * time.Millisecond)
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	scratch := string(raw)
	defer os.RemoveAll(scratch) // Also clean the old implementation's leaked temp directory on red.
	if _, err := os.Stat(filepath.Join(scratch, "source.pdf")); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	db, err := pg.Open(t.Context(), database, "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	jobs, err := OpenJobs(db, dataStore(t, root), root, func(ctx context.Context, _ JobScope) error { return ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	defer jobs.Close()
	if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("crashed conversion scratch escaped recovery: %s (%v)", scratch, err)
	}
	rel, err := filepath.Rel(root, scratch)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("conversion scratch was outside private job lifecycle: %s", scratch)
	}
	t.Setenv("FMN_LIMIT_SOURCE_JOB_BYTES", strconv.FormatInt(jobReservation(len("Cobalt"))+(1<<20), 10))
	scope := JobScope{Subject: "user", Notebook: "notes", Generation: 1}
	next, err := jobs.Submit(t.Context(), scope, JobInput{Filename: "next.txt"}, []byte("Cobalt"))
	if err != nil {
		t.Fatalf("crash recovery did not reclaim scratch reservation: %v", err)
	}
	if _, err := jobs.Wait(t.Context(), scope, next.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSourceJobsChargeRetainedScratch(t *testing.T) {
	data := t.TempDir()
	jobs, err := OpenJobs(pgtest.Org(t), dataStore(t, data), data, func(ctx context.Context, _ JobScope) error { return ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	defer jobs.Close()
	scope := JobScope{Subject: "user", Notebook: "notes", Generation: 1}
	job, err := jobs.Submit(t.Context(), scope, JobInput{Filename: "one.txt"}, []byte("Amber"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Wait(t.Context(), scope, job.ID); err != nil {
		t.Fatal(err)
	}
	// A failed recursive cleanup leaves ordinary conversion files. Admission
	// must charge them until removal succeeds rather than refunding the space.
	scratch := filepath.Join(data, "cache", jobsDir, job.ID, "conversion")
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "render.png"), make([]byte, 2<<20), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FMN_LIMIT_SOURCE_JOB_BYTES", strconv.FormatInt(jobReservation(len("Cobalt"))+(1<<20), 10))
	if _, err := jobs.Submit(t.Context(), scope, JobInput{Filename: "two.txt"}, []byte("Cobalt")); !errors.Is(err, ErrJobCapacity) {
		t.Fatalf("retained scratch did not consume storage capacity: %v", err)
	}
	if err := os.RemoveAll(filepath.Dir(scratch)); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Submit(t.Context(), scope, JobInput{Filename: "two.txt"}, []byte("Cobalt")); err != nil {
		t.Fatalf("removed scratch still consumed capacity: %v", err)
	}
}

func TestSourceJobsResumeClearsRetainedScratch(t *testing.T) {
	for _, refuseCleanup := range []bool{false, true} {
		t.Run(strconv.FormatBool(refuseCleanup), func(t *testing.T) {
			data := t.TempDir()
			db, store := pgtest.Org(t), dataStore(t, data)
			id := strings.Repeat("a", 32)
			original := []byte("Amber")
			scope := JobScope{Subject: "user", Notebook: "notes", Generation: 1}
			seedJob(t, db, store, sourceJob{Version: 1, JobStatus: JobStatus{ID: id, State: "failed", Expires: time.Now().Add(time.Hour)}, Scope: scope, Input: JobInput{Filename: "one.txt"}, Hash: workspace.ContentHash(original)}, original)
			var checks atomic.Int32
			jobs, err := OpenJobs(db, store, data, func(ctx context.Context, _ JobScope) error {
				if checks.Add(1) == 3 {
					<-ctx.Done()
				} // Hold processing after Resume returns.
				return ctx.Err()
			})
			if err != nil {
				t.Fatal(err)
			}
			defer jobs.Close()
			scratch := filepath.Join(data, "cache", jobsDir, id, "conversion")
			if err := os.MkdirAll(scratch, 0700); err != nil {
				t.Fatal(err)
			}
			old := filepath.Join(scratch, "render.png")
			if err := os.WriteFile(old, make([]byte, 2<<20), 0600); err != nil {
				t.Fatal(err)
			}
			if refuseCleanup {
				if os.Geteuid() == 0 {
					t.Skip("permission failure requires an unprivileged process")
				}
				if err := os.Chmod(scratch, 0500); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(scratch, 0700)
			}
			_, err = jobs.Resume(t.Context(), scope, id)
			if refuseCleanup {
				if err == nil {
					t.Fatal("resume released old accounting despite failed scratch cleanup")
				}
				job, err := jobs.load(id)
				if err != nil || job.State != "failed" || job.reserved != 0 {
					t.Fatalf("failed cleanup changed job admission: %+v %v", job, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("resume retained old scratch beside new reservation: %v", err)
				}
			}
		})
	}
}

// The job's row says the document was refused, so any gateway reports it alike.
func TestSourceJobRecordsRejection(t *testing.T) {
	db, store := pgtest.Org(t), dataStore(t, t.TempDir())
	authorize := func(ctx context.Context, _ JobScope) error { return ctx.Err() }
	first, err := OpenJobs(db, store, t.TempDir(), authorize)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenJobs(db, store, t.TempDir(), authorize)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	scope := JobScope{Subject: "user", Notebook: "notes", Generation: 1}
	settle := func(id string) JobStatus {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if _, err := second.Wait(ctx, scope, id); ctx.Err() != nil {
			t.Fatal(err)
		}
		status, err := second.Status(t.Context(), scope, id)
		if err != nil {
			t.Fatal(err)
		}
		return status
	}
	job, err := first.Submit(t.Context(), scope, JobInput{Filename: "page.html"}, []byte(strings.Repeat("<br>", 100001)))
	if err != nil {
		t.Fatal(err)
	}
	rejected := func() {
		t.Helper()
		if status := settle(job.ID); status.State != "failed" || status.Warning != "Rejected: HTML exceeds 100000 tokens" {
			t.Fatalf("rejected job: %+v", status)
		}
		if _, err := second.Wait(t.Context(), scope, job.ID); !errors.Is(err, ErrRejected) || err.Error() != "source processing failed: HTML exceeds 100000 tokens" {
			t.Fatalf("got %v", err)
		}
	}
	rejected()
	resumed, err := first.Resume(t.Context(), scope, job.ID)
	if err != nil || resumed.Warning != "" {
		t.Fatalf("resumed job kept its old failure: %+v %v", resumed, err)
	}
	rejected()
	// A failure this server caused is not the document's fault.
	if _, err := first.Submit(t.Context(), JobScope{}, JobInput{Filename: "page.html"}, []byte("<p>Amber</p>")); err == nil || errors.Is(err, ErrRejected) {
		t.Fatalf("job without a scope: %v", err)
	}
	broken, err := first.Submit(t.Context(), scope, JobInput{Filename: "page.html", Options: Options{ExternalOCR: true}}, []byte("<p>Amber</p>"))
	if err != nil {
		t.Fatal(err)
	}
	if status := settle(broken.ID); status.State != "failed" || strings.HasPrefix(status.Warning, rejectedWarning) {
		t.Fatalf("server failure: %+v", status)
	}
	if _, err := second.Wait(t.Context(), scope, broken.ID); err == nil || errors.Is(err, ErrRejected) {
		t.Fatalf("got %v", err)
	}
}

func TestGatewaysShareSourceJobsAndAdmission(t *testing.T) {
	t.Setenv("FMN_LIMIT_SOURCE_JOB_COUNT", "1")
	db, store := pgtest.Org(t), dataStore(t, t.TempDir())
	authorize := func(ctx context.Context, _ JobScope) error { return ctx.Err() }
	first, err := OpenJobs(db, store, t.TempDir(), authorize)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenJobs(db, store, t.TempDir(), authorize)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	scope := JobScope{Subject: "user", Notebook: "notes", Generation: 1}
	job, err := first.Submit(t.Context(), scope, JobInput{Filename: "one.txt"}, []byte("Amber"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	capture, err := second.Wait(ctx, scope, job.ID)
	if err != nil || !strings.Contains(capture.Text, "Amber") {
		t.Fatalf("another gateway could not read the job's result: %q %v", capture.Text, err)
	}
	if _, err := second.Submit(t.Context(), scope, JobInput{Filename: "two.txt"}, []byte("Cobalt")); !errors.Is(err, ErrJobCapacity) {
		t.Fatalf("a second gateway admitted past the instance's job count: %v", err)
	}
	if _, err := db.Pool.Exec(t.Context(), `UPDATE source_jobs SET manifest = $3 WHERE org = $1 AND id = $2`, db.Org, job.ID, []byte(`{"version":1`)); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Status(t.Context(), scope, job.ID); err == nil {
		t.Fatal("a damaged manifest was served")
	}
}

func TestOpeningLeavesAnotherLiveGatewaysJobRunning(t *testing.T) {
	db, store := pgtest.Org(t), dataStore(t, t.TempDir())
	other, leave := pgtest.Gateway(t, db)
	now := time.Now().UTC()
	id := strings.Repeat("d", 32)
	seedJob(t, db, store, sourceJob{Version: 1, JobStatus: JobStatus{ID: id, State: "running", Created: now, Updated: now, Expires: now.Add(time.Hour)}, Scope: JobScope{Subject: "user", Notebook: "notes", Generation: 1}, Input: JobInput{Filename: "one.txt"}, Holder: other.Holder}, []byte("Amber"))
	// The other gateway converts in its own working directory, on its disk.
	otherData := t.TempDir()
	scratch := filepath.Join(otherData, "cache", jobsDir, id)
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	jobs, err := OpenJobs(db, store, t.TempDir(), func(ctx context.Context, _ JobScope) error { return ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	defer jobs.Close()
	scope := JobScope{Subject: "user", Notebook: "notes", Generation: 1}
	if status, err := jobs.Status(t.Context(), scope, id); err != nil || status.State != "running" {
		t.Fatalf("opening interrupted another live gateway's job: %+v %v", status, err)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Fatal("opening removed a live job's scratch", err)
	}
	leave()
	jobs.mu.Lock()
	err = jobs.recoverGone()
	jobs.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if status, err := jobs.Status(t.Context(), scope, id); err != nil || status.State != "interrupted" {
		t.Fatalf("the job of a gateway that is gone was left running: %+v %v", status, err)
	}
	// The stopped gateway's scratch is removed when it opens again.
	restarted, err := OpenJobs(db, store, otherData, func(ctx context.Context, _ JobScope) error { return ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	restarted.Close()
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatal("recovery kept a stopped job's scratch", err)
	}
}
