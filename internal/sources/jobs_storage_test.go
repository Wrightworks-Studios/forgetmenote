package sources

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/blob/blobtest"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
)

// blobStores are the instance blob stores a job's files may live in: a
// directory gateways share, and S3.
var blobStores = map[string]func(t *testing.T) blob.Store{
	"directory": func(t *testing.T) blob.Store { return dataStore(t, t.TempDir()) },
	"s3":        func(t *testing.T) blob.Store { return blobtest.Start(t).Store(t, "instance") },
}

func storedKeys(t *testing.T, store blob.Store) []string {
	t.Helper()
	keys, err := store.List(context.Background(), jobsDir+"/")
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// awaitKeys waits for the janitor to leave exactly want in store.
func awaitKeys(t *testing.T, store blob.Store, want ...string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		keys := storedKeys(t, store)
		if slices.Equal(keys, want) || len(keys) == 0 && len(want) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stored objects %q, want %q", keys, want)
		}
	}
}

// expireJob moves a job's expiry into the past, as a day passing would.
func expireJob(t *testing.T, db *pg.DB, id string) {
	t.Helper()
	var raw []byte
	if err := db.Pool.QueryRow(t.Context(), `SELECT manifest FROM source_jobs WHERE org = $1 AND id = $2`, db.Org, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["expires"] = time.Now().Add(-time.Minute).UTC()
	raw, _ = json.Marshal(manifest)
	if _, err := db.Pool.Exec(t.Context(), `UPDATE source_jobs SET manifest = $3 WHERE org = $1 AND id = $2`, db.Org, id, raw); err != nil {
		t.Fatal(err)
	}
}

// A job's original and result are objects at the paths the file-based release
// kept them at, which another gateway with a disk of its own reads, and which
// expiry removes. In the data directory they are that release's files.
func TestSourceJobFilesAreBlobObjects(t *testing.T) {
	authorize := func(ctx context.Context, _ JobScope) error { return ctx.Err() }
	scope := JobScope{Subject: "user", Notebook: "notes", Generation: 1}
	t.Run("data directory", func(t *testing.T) {
		data := t.TempDir()
		jobs, err := OpenJobs(pgtest.Org(t), dataStore(t, data), data, authorize)
		if err != nil {
			t.Fatal(err)
		}
		defer jobs.Close()
		job, err := jobs.Submit(t.Context(), scope, JobInput{Filename: "one.txt"}, []byte("Amber"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = jobs.Wait(t.Context(), scope, job.ID); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(data, jobsDir, job.ID, "original"))
		if err != nil || string(raw) != "Amber" {
			t.Fatalf("the original is not the file-based release's file: %q %v", raw, err)
		}
		if _, err = os.Stat(filepath.Join(data, jobsDir, job.ID, "result.json")); err != nil {
			t.Fatal(err)
		}
		if entries, err := os.ReadDir(filepath.Join(data, "cache", jobsDir)); err != nil || len(entries) != 0 {
			t.Fatalf("conversion scratch is not the gateway's own, emptied working space: %v %v", entries, err)
		}
	})
	for name, open := range blobStores {
		t.Run(name, func(t *testing.T) {
			db, store := pgtest.Org(t), open(t)
			first, leave := pgtest.Gateway(t, db)
			firstData, secondData := t.TempDir(), t.TempDir()
			jobs, err := OpenJobs(first, store, firstData, authorize)
			if err != nil {
				t.Fatal(err)
			}
			defer jobs.Close()
			job, err := jobs.Submit(t.Context(), scope, JobInput{Filename: "one.txt"}, []byte("Amber"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = jobs.Wait(t.Context(), scope, job.ID); err != nil {
				t.Fatal(err)
			}
			want := []string{jobsDir + "/" + job.ID + "/original", jobsDir + "/" + job.ID + "/result.json"}
			if keys := storedKeys(t, store); !slices.Equal(keys, want) {
				t.Fatalf("stored objects %q, want %q", keys, want)
			}
			if original, err := blob.Read(t.Context(), store, want[0], 1<<20); err != nil || string(original) != "Amber" {
				t.Fatalf("stored original %q: %v", original, err)
			}
			jobs.Close()
			leave()

			second, err := OpenJobs(db, store, secondData, authorize)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			capture, err := second.Result(t.Context(), scope, job.ID)
			if err != nil || !strings.Contains(capture.Text, "Amber") || string(capture.Original) != "Amber" {
				t.Fatalf("another gateway could not read the result: %q %q %v", capture.Text, capture.Original, err)
			}
			for _, data := range []string{firstData, secondData} {
				if _, err := os.Stat(filepath.Join(data, jobsDir)); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("a gateway's own disk holds job files: %v", err)
				}
			}
			expireJob(t, db, job.ID)
			next, err := second.Submit(t.Context(), scope, JobInput{Filename: "two.txt"}, []byte("Cobalt"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = second.Wait(t.Context(), scope, next.ID); err != nil {
				t.Fatal(err)
			}
			awaitKeys(t, store, jobsDir+"/"+next.ID+"/original", jobsDir+"/"+next.ID+"/result.json")
			if _, err = second.Status(t.Context(), scope, job.ID); !errors.Is(err, workspace.ErrForbidden) {
				t.Fatalf("an expired job is still served: %v", err)
			}
		})
	}
}

// A job's row commits before its original is stored, so opening removes only
// objects whose row is gone, and leaves a job another live gateway is still
// creating, which nobody serves until it is published.
func TestSourceJobObjectsWithoutRowsAreRemoved(t *testing.T) {
	for name, open := range blobStores {
		t.Run(name, func(t *testing.T) {
			db, store := pgtest.Org(t), open(t)
			creating, leave := pgtest.Gateway(t, db)
			now := time.Now().UTC()
			scope := JobScope{Subject: "user", Notebook: "notes", Generation: 1}
			orphan, live, abandoned, stray := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32), jobsDir+"/notes.txt"
			blob.Write(t.Context(), store, jobsDir+"/"+orphan+"/original", []byte("retired"))
			blob.Write(t.Context(), store, jobsDir+"/"+orphan+"/page-00001.json", []byte(`{"number":1}`))
			blob.Write(t.Context(), store, stray, []byte("not a job's"))
			seedJob(t, db, store, sourceJob{Version: 1, JobStatus: JobStatus{ID: live, State: "staging", Created: now, Updated: now, Expires: now.Add(time.Hour)}, Scope: scope, Input: JobInput{Filename: "one.txt"}, Holder: creating.Holder}, []byte("uploading"))
			seedJob(t, db, store, sourceJob{Version: 1, JobStatus: JobStatus{ID: abandoned, State: "staging", Created: now, Updated: now, Expires: now.Add(time.Hour)}, Scope: scope, Input: JobInput{Filename: "one.txt"}, Holder: pg.NewHolder()}, []byte("crashed upload"))
			jobs, err := OpenJobs(db, store, t.TempDir(), func(ctx context.Context, _ JobScope) error { return ctx.Err() })
			if err != nil {
				t.Fatal(err)
			}
			defer jobs.Close()
			want := []string{jobsDir + "/" + live + "/original", stray}
			if keys := storedKeys(t, store); !slices.Equal(keys, want) {
				t.Fatalf("after opening: %q, want %q", keys, want)
			}
			if job, err := jobs.load(abandoned); err != nil || job != nil {
				t.Fatalf("the upload of a gateway that is gone was kept: %+v %v", job, err)
			}
			if _, err = jobs.Status(t.Context(), scope, live); !errors.Is(err, workspace.ErrForbidden) {
				t.Fatalf("a job still being created was served: %v", err)
			}

			// Its creator stops, and the periodic recovery retires it.
			leave()
			jobs.mu.Lock()
			err = jobs.recoverGone()
			jobs.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			awaitKeys(t, store, stray)

			// A retirement interrupted after its row went leaves objects the
			// sweep removes.
			ready := strings.Repeat("d", 32)
			seedJob(t, db, store, sourceJob{Version: 1, JobStatus: JobStatus{ID: ready, State: "ready", Created: now, Updated: now, Expires: now.Add(time.Hour)}, Scope: scope, Input: JobInput{Filename: "one.txt"}}, []byte("done"))
			if _, err = db.Pool.Exec(t.Context(), `DELETE FROM source_jobs WHERE org = $1 AND id = $2`, db.Org, ready); err != nil {
				t.Fatal(err)
			}
			if err = jobs.sweep(t.Context()); err != nil {
				t.Fatal(err)
			}
			if keys := storedKeys(t, store); !slices.Equal(keys, []string{stray}) {
				t.Fatalf("after the sweep: %q", keys)
			}
		})
	}
}

// An upload blob storage refuses leaves neither a row nor an object.
func TestSourceJobUploadBlobStorageRefusesLeavesNothing(t *testing.T) {
	db, s3 := pgtest.Org(t), blobtest.Start(t)
	store := s3.Store(t, "instance")
	jobs, err := OpenJobs(db, store, t.TempDir(), func(ctx context.Context, _ JobScope) error { return ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	defer jobs.Close()
	s3.SetFail(func(r *http.Request) int {
		if r.Method == http.MethodPut {
			return http.StatusServiceUnavailable
		}
		return 0
	})
	scope := JobScope{Subject: "user", Notebook: "notes", Generation: 1}
	if _, err = jobs.Submit(t.Context(), scope, JobInput{Filename: "one.txt"}, []byte("Amber")); err == nil {
		t.Fatal("a job was admitted without its original")
	}
	var rows int
	if err = db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM source_jobs WHERE org = $1`, db.Org).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("a refused upload left %d rows: %v", rows, err)
	}
	if objects := s3.Objects(); len(objects) != 0 {
		t.Fatalf("a refused upload left objects: %q", objects)
	}
	s3.SetFail(nil)
	job, err := jobs.Submit(t.Context(), scope, JobInput{Filename: "one.txt"}, []byte("Amber"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = jobs.Wait(t.Context(), scope, job.ID); err != nil {
		t.Fatal(err)
	}
}

// pdfTools installs extractor stubs for a two-page scan whose second page
// renders until the conversion is canceled, and returns a function that
// changes them: the first page's text changes, and the second renders.
func pdfTools(t *testing.T) (finish func()) {
	t.Helper()
	dir := t.TempDir()
	bbox := `<html><body><doc><page width="612" height="792"><flow><block><line><word xMin="10" yMin="10" xMax="30" yMax="20">Amber checkpoint</word></line></block></flow></page><page width="612" height="792"/></doc></body></html>`
	for name, body := range map[string]string{"pdftotext": "cat <<'XML'\n" + bbox + "\nXML\n", "pdfimages": "printf '2 0 image\\n'\n", "pdftoppm": "touch started\nexec /bin/sleep 30\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() {
		if err := os.WriteFile(filepath.Join(dir, "pdftotext"), []byte("#!/bin/sh\ncat <<'XML'\n"+strings.ReplaceAll(bbox, "Amber checkpoint", "Discarded new extraction")+"\nXML\n"), 0700); err != nil {
			t.Fatal(err)
		}
		white := image.NewRGBA(image.Rect(0, 0, 32, 32))
		for y := 0; y < 32; y++ {
			for x := 0; x < 32; x++ {
				white.Set(x, y, color.White)
			}
		}
		f, err := os.Create(filepath.Join(dir, "white.png"))
		if err == nil {
			err = png.Encode(f, white)
		}
		if err == nil {
			err = f.Close()
		}
		if err == nil {
			err = os.WriteFile(filepath.Join(dir, "pdftoppm"), []byte("#!/bin/sh\nfor last; do :; done\ncp '"+filepath.Join(dir, "white.png")+"' \"$last.png\"\n"), 0700)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// A gateway that is gone leaves a running job whose checkpoints another
// gateway, with a disk of its own, resumes from and then expires.
func TestAnotherGatewayResumesAJobFromItsStoredCheckpoints(t *testing.T) {
	requireOCR(t)
	for name, open := range blobStores {
		t.Run(name, func(t *testing.T) {
			finish := pdfTools(t)
			authorize := func(ctx context.Context, _ JobScope) error { return ctx.Err() }
			scope := JobScope{Subject: "user", Notebook: "notes", Generation: 1}
			db, store := pgtest.Org(t), open(t)
			gone, leave := pgtest.Gateway(t, db)
			goneData, data := t.TempDir(), t.TempDir()
			first, err := OpenJobs(gone, store, goneData, authorize)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Close()
			job, err := first.Submit(t.Context(), scope, JobInput{Filename: "scan.pdf"}, []byte("%PDF-fixture"))
			if err != nil {
				t.Fatal(err)
			}
			second, err := OpenJobs(db, store, data, authorize)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
				status, err := second.Status(t.Context(), scope, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				if status.Pages == 1 && len(scratchFiles(t, scratchGlob(goneData, "started"))) > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the first gateway did not checkpoint its first page: %+v", status)
				}
			}
			leave()
			second.mu.Lock()
			err = second.recoverGone()
			second.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if status, err := second.Status(t.Context(), scope, job.ID); err != nil || status.State != "interrupted" || status.Pages != 1 {
				t.Fatalf("the gone gateway's job: %+v %v", status, err)
			}
			first.Close()

			finish()
			if _, err = second.Resume(t.Context(), scope, job.ID); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			capture, err := second.Wait(ctx, scope, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(capture.Text, "Amber checkpoint") || strings.Contains(capture.Text, "Discarded new") || !strings.Contains(capture.Text, "## Page 2") {
				t.Fatal("the stored checkpoint was repeated or lost:", capture.Text)
			}
			if _, err := os.Stat(filepath.Join(data, jobsDir)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the resuming gateway's disk holds job files: %v", err)
			}
			pages := 0
			for _, key := range storedKeys(t, store) {
				if strings.Contains(key, "/page-") {
					pages++
				}
			}
			if pages != 2 {
				t.Fatalf("stored %d page checkpoints, want 2", pages)
			}
			expireJob(t, db, job.ID)
			second.mu.Lock()
			err = second.expire()
			second.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			awaitKeys(t, store)
		})
	}
}
