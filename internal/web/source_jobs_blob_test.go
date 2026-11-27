package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/blob/blobtest"
	"forgetmenote/internal/pg/pgtest"
)

// A source job submitted on one gateway is read on another, which resumes it
// from its stored original after the first is gone, saves it and expires it.
// The gateways share only the database and blob storage: neither disk holds
// the job's files.
func TestSourceJobMovesBetweenGatewaysThroughBlobStorage(t *testing.T) {
	stores := map[string]func(t *testing.T) blob.Store{
		"directory": func(t *testing.T) blob.Store {
			store, err := blob.OpenDir(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			return store
		},
		"s3": func(t *testing.T) blob.Store { return blobtest.Start(t).Store(t, "instance") },
	}
	for name, open := range stores {
		t.Run(name, func(t *testing.T) {
			app, _ := fixture(t)
			tools := t.TempDir()
			// The first gateway's conversion runs until it stops.
			if err := os.WriteFile(filepath.Join(tools, "pdftotext"), []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))

			store := open(t)
			db, leave := pgtest.Gateway(t, app.db)
			firstData := t.TempDir()
			first, err := NewWithBlobs(app.catalog, db, store, firstData, app.origin, app.token)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(first.Close)
			second := gatewayWith(t, app, store)

			input, _ := json.Marshal(map[string]any{"filename": "scan.pdf", "data": base64.StdEncoding.EncodeToString([]byte("%PDF-fixture"))})
			created := call(first, "POST", "/api/sources/jobs", string(input), testToken)
			if created.Code != 200 {
				t.Fatalf("source job creation: %d %s", created.Code, created.Body.String())
			}
			var out struct {
				Job struct {
					ID    string `json:"id"`
					State string `json:"state"`
					Pages int    `json:"pages"`
				} `json:"job"`
				Prepared *preparedSource `json:"prepared"`
			}
			if err := json.Unmarshal(created.Body.Bytes(), &out); err != nil || out.Job.ID == "" {
				t.Fatal("missing job id", err)
			}
			id := out.Job.ID
			poll := func(until func() bool) {
				t.Helper()
				for deadline := time.Now().Add(15 * time.Second); !until(); time.Sleep(10 * time.Millisecond) {
					if time.Now().After(deadline) {
						t.Fatalf("job %s stalled: %+v", id, out.Job)
					}
					response := call(second, "GET", "/api/sources/jobs/"+id, "", testToken)
					if response.Code != 200 {
						t.Fatal(response.Code, response.Body.String())
					}
					if err := json.Unmarshal(response.Body.Bytes(), &out); err != nil {
						t.Fatal(err)
					}
				}
			}
			poll(func() bool { return out.Job.State == "running" })

			// The first gateway stops mid-conversion, and the second resumes
			// from the stored original.
			first.Close()
			leave()
			bbox := `<html><body><doc><page width="612" height="792"><flow><block><line><word xMin="10" yMin="10" xMax="30" yMax="20">Amber resumed</word></line></block></flow></page></doc></body></html>`
			if err := os.WriteFile(filepath.Join(tools, "pdftotext"), []byte("#!/bin/sh\ncat <<'XML'\n"+bbox+"\nXML\n"), 0700); err != nil {
				t.Fatal(err)
			}
			if resumed := call(second, "POST", "/api/sources/jobs/"+id+"/resume", "{}", testToken); resumed.Code != 200 {
				t.Fatalf("resume on the other gateway: %d %s", resumed.Code, resumed.Body.String())
			}
			out.Prepared = nil
			poll(func() bool { return out.Prepared != nil })
			if !strings.Contains(out.Prepared.Preview, "Amber resumed") {
				t.Fatalf("the resumed conversion was lost: %s", out.Prepared.Preview)
			}
			body, _ := json.Marshal(map[string]any{"job": id, "title": "Resumed scan"})
			saved := call(second, "POST", "/api/sources/import", string(body), testToken)
			if saved.Code != 200 || !strings.Contains(saved.Body.String(), "Amber resumed") {
				t.Fatalf("saving the resumed job: %d %s", saved.Code, saved.Body.String())
			}

			// The job expires, and the other gateway removes its files.
			var raw []byte
			if err := app.db.Pool.QueryRow(context.Background(), `SELECT manifest FROM source_jobs WHERE org = $1 AND id = $2`, app.db.Org, id).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var manifest map[string]any
			json.Unmarshal(raw, &manifest)
			manifest["expires"] = time.Now().Add(-time.Minute).UTC()
			raw, _ = json.Marshal(manifest)
			if _, err := app.db.Pool.Exec(context.Background(), `UPDATE source_jobs SET manifest = $3 WHERE org = $1 AND id = $2`, app.db.Org, id, raw); err != nil {
				t.Fatal(err)
			}
			next, _ := json.Marshal(map[string]any{"filename": "next.txt", "data": base64.StdEncoding.EncodeToString([]byte("Cobalt"))})
			if response := call(second, "POST", "/api/sources/jobs", string(next), testToken); response.Code != 200 {
				t.Fatalf("next job: %d %s", response.Code, response.Body.String())
			}
			if gone := call(second, "GET", "/api/sources/jobs/"+id, "", testToken); gone.Code != 403 {
				t.Fatalf("the expired job is still served: %d", gone.Code)
			}
			// The other gateway's janitor removes the expired job's objects.
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				keys, err := store.List(context.Background(), "source-jobs/"+id+"/")
				if err != nil {
					t.Fatal(err)
				}
				if len(keys) == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("expiry left %q", keys)
				}
			}
			for _, data := range []string{firstData, second.data, app.data} {
				if _, err := os.Stat(filepath.Join(data, "source-jobs")); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("a gateway's own disk holds source job files: %v", err)
				}
			}
		})
	}
}
