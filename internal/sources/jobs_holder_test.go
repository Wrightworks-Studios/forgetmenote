package sources

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/blob/blobtest"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

// heldStore holds the first Get or Put of an object whose key ends in suffix
// until release, as a gateway that stalls in the middle of a call would; every
// other call goes on.
type heldStore struct {
	blob.Store
	method, suffix  string
	arrived, resume chan struct{}
	first, done     sync.Once
}

func holdObject(store blob.Store, method, suffix string) *heldStore {
	return &heldStore{Store: store, method: method, suffix: suffix, arrived: make(chan struct{}), resume: make(chan struct{})}
}

func (s *heldStore) hold(method, key string) {
	if method != s.method || !strings.HasSuffix(key, s.suffix) {
		return
	}
	held := false
	s.first.Do(func() {
		held = true
		close(s.arrived)
	})
	if held {
		<-s.resume
	}
}

func (s *heldStore) Put(ctx context.Context, key string, body io.ReadSeeker) error {
	s.hold(http.MethodPut, key)
	return s.Store.Put(ctx, key, body)
}

func (s *heldStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	s.hold(http.MethodGet, key)
	return s.Store.Get(ctx, key)
}

// await waits until the held call arrives.
func (s *heldStore) await(t *testing.T) {
	t.Helper()
	select {
	case <-s.arrived:
	case <-time.After(10 * time.Second):
		t.Fatalf("no %s of an object ending in %s arrived", s.method, s.suffix)
	}
}

func (s *heldStore) release() { s.done.Do(func() { close(s.resume) }) }

// textPDF installs extractor stubs for a two-page PDF of native text, whose
// pages convert without OCR, and returns a function that changes the text of
// the conversions that start after it.
func textPDF(t *testing.T) (change func(word string)) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	change = func(word string) {
		page := func(n string) string {
			return `<page width="612" height="792"><flow><block><line><word xMin="10" yMin="10" xMax="30" yMax="20">` + word + " page " + n + `</word></line></block></flow></page>`
		}
		write("pdftotext", "cat <<'XML'\n<html><body><doc>"+page("one")+page("two")+"</doc></body></html>\nXML\n")
	}
	change("Amber")
	write("pdfimages", "exit 0\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return change
}

// rowOf returns a job's row as stored: its version, reservation and manifest.
func rowOf(t *testing.T, db *pg.DB, id string) string {
	t.Helper()
	var version, reserved int64
	var raw []byte
	if err := db.Pool.QueryRow(t.Context(), `SELECT version, reserved, manifest FROM source_jobs WHERE org = $1 AND id = $2`, db.Org, id).Scan(&version, &reserved, &raw); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("version %d, reserved %d: %s", version, reserved, raw)
}

// objectsOf returns the content of every object stored under a job.
func objectsOf(t *testing.T, store blob.Store, id string) map[string]string {
	t.Helper()
	keys, err := store.List(t.Context(), jobsDir+"/"+id+"/")
	if err != nil {
		t.Fatal(err)
	}
	objects := map[string]string{}
	for _, key := range keys {
		raw, err := blob.Read(t.Context(), store, key, 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		objects[key] = string(raw)
	}
	return objects
}

// awaitIdle waits for a gateway's worker to stop the job it runs, on its own.
func awaitIdle(t *testing.T, jobs *Jobs) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		jobs.mu.Lock()
		idle := jobs.active == ""
		jobs.mu.Unlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the worker did not stop its job")
		}
	}
}

// A gateway whose presence lapsed while it still runs a job, as in a network
// partition, is stalled storing a checkpoint or the result. Another gateway
// resumes the job and finishes it. Once the first goes on, it stores nothing
// over the objects the finished job names, writes nothing to its row, and its
// late object is charged and then removed with the job.
func TestALostSourceRunNeverWritesOverTheRunThatResumedIt(t *testing.T) {
	cases := []struct {
		name, suffix, page string
		// converting is a gateway stalled while it holds its conversion slot.
		converting bool
	}{
		{name: "checkpoint", suffix: "/page-00002.json", page: "Cobalt page two", converting: true},
		{name: "result", suffix: "/result.json", page: "Amber page two"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			change := textPDF(t)
			authorize := func(ctx context.Context, _ JobScope) error { return ctx.Err() }
			scope := JobScope{Subject: "user", Notebook: "notes", Generation: 1}
			db, store := pgtest.Org(t), blobtest.Start(t).Store(t, "instance")
			lost, leave := pgtest.Gateway(t, db)
			stalled := holdObject(store, http.MethodPut, c.suffix)
			first, err := OpenJobs(lost, stalled, t.TempDir(), authorize)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Close()
			defer stalled.release()
			second, err := OpenJobs(db, store, t.TempDir(), authorize)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			job, err := first.Submit(t.Context(), scope, JobInput{Filename: "scan.pdf"}, []byte("%PDF-fixture"))
			if err != nil {
				t.Fatal(err)
			}
			stalled.await(t)
			// Gateways are processes of their own, each with a conversion slot;
			// these two share one, so a gateway stalled while converting lends
			// its slot until it goes on.
			var lent sync.Once
			giveBack := func() {}
			if c.converting {
				<-documentWork
				giveBack = func() { lent.Do(func() { documentWork <- struct{}{} }) }
				defer stalled.release()
				defer giveBack()
			}

			leave()
			second.mu.Lock()
			err = second.recoverGone()
			second.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			change("Cobalt")
			if _, err = second.Resume(t.Context(), scope, job.ID); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			capture, err := second.Wait(ctx, scope, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(capture.Text, "Amber page one") || !strings.Contains(capture.Text, c.page) {
				t.Fatal("the resumed job lost a checkpoint:", capture.Text)
			}
			row, objects := rowOf(t, db, job.ID), objectsOf(t, store, job.ID)

			giveBack()
			stalled.release()
			awaitIdle(t, first)
			if now := rowOf(t, db, job.ID); now != row {
				t.Errorf("the lost run wrote the job's row\n%s\nover\n%s", now, row)
			}
			after := objectsOf(t, store, job.ID)
			for key, raw := range objects {
				if after[key] != raw {
					t.Errorf("the lost run stored %s over the finished job's\n%s\nover\n%s", key, after[key], raw)
				}
			}
			if len(after) != len(objects)+1 {
				t.Fatalf("the lost run's late object is not beside the finished job's: %q", slices.Sorted(maps.Keys(after)))
			}
			again, err := second.Result(t.Context(), scope, job.ID)
			if err != nil || again.Text != capture.Text || again.Record != capture.Record || string(again.Original) != string(capture.Original) {
				t.Fatalf("the finished job's result changed: %+v %v", again.Record, err)
			}
			stored := int64(0)
			for _, raw := range after {
				stored += int64(len(raw))
			}
			live, err := second.load(job.ID)
			if err != nil || live.Stored < stored {
				t.Fatalf("the row charges %d bytes of the %d stored: %v", live.Stored, stored, err)
			}
			// An export writes the finished job's objects at the paths the
			// file-based release reads, and that release reads it.
			_, dir := exportFiles(t, db, store)
			for _, name := range []string{"result.json", "page-00002.json"} {
				var want []string
				for key, raw := range objects {
					if strings.HasSuffix(key, "/"+name) {
						want = append(want, raw)
					}
				}
				if raw, err := os.ReadFile(filepath.Join(dir, jobsDir, job.ID, name)); err != nil || len(want) != 1 || string(raw) != want[0] {
					t.Fatalf("the export's %s is not the finished job's: %q %v", name, raw, err)
				}
			}
			if _, err = importFiles(t, pgtest.Org(t), dir, nil); err != nil {
				t.Fatal("the export does not import again:", err)
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

// A gateway whose presence lapsed while it was stalled before converting goes
// on only after another gateway resumed the job and runs it. It stops at its
// first check of the row, with nothing written, and the job finishes there.
func TestALostSourceRunStopsAtItsNextCheck(t *testing.T) {
	textPDF(t)
	authorize := func(ctx context.Context, _ JobScope) error { return ctx.Err() }
	scope := JobScope{Subject: "user", Notebook: "notes", Generation: 1}
	db, store := pgtest.Org(t), blobtest.Start(t).Store(t, "instance")
	lost, leave := pgtest.Gateway(t, db)
	stalled := holdObject(store, http.MethodGet, "/original")
	first, err := OpenJobs(lost, stalled, t.TempDir(), authorize)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	defer stalled.release()
	resumed := holdObject(store, http.MethodGet, "/original")
	second, err := OpenJobs(db, resumed, t.TempDir(), authorize)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	defer resumed.release()
	job, err := first.Submit(t.Context(), scope, JobInput{Filename: "scan.pdf"}, []byte("%PDF-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	stalled.await(t)

	leave()
	second.mu.Lock()
	err = second.recoverGone()
	second.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = second.Resume(t.Context(), scope, job.ID); err != nil {
		t.Fatal(err)
	}
	resumed.await(t)
	row, objects := rowOf(t, db, job.ID), objectsOf(t, store, job.ID)

	stalled.release()
	awaitIdle(t, first)
	if now := rowOf(t, db, job.ID); now != row {
		t.Fatalf("the lost run wrote the job's row\n%s\nover\n%s", now, row)
	}
	if after := objectsOf(t, store, job.ID); !maps.Equal(after, objects) {
		t.Fatalf("the lost run stored objects: %q, was %q", after, objects)
	}
	resumed.release()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	capture, err := second.Wait(ctx, scope, job.ID)
	if err != nil || !strings.Contains(capture.Text, "Amber page one") || !strings.Contains(capture.Text, "Amber page two") {
		t.Fatalf("the resumed job did not finish: %q %v", capture.Text, err)
	}
}
