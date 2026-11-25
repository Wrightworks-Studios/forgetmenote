package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// TestStorageImportChild runs the import command in a child process for
// TestStorageImportResumesAfterItWasKilled, which kills it.
func TestStorageImportChild(t *testing.T) {
	if os.Getenv("FMN_STORAGE_IMPORT_CHILD") == "" {
		t.Skip("run by TestStorageImportResumesAfterItWasKilled")
	}
	runAdmin([]string{"storage", "import", "--data", os.Getenv("FMN_STORAGE_IMPORT_DATA")}, nil, os.Stdout)
}

// manyCommits writes notebook id's repository with commits revisions, as
// the previous release would have after long use.
func manyCommits(t *testing.T, data, id string, commits int) string {
	t.Helper()
	work := filepath.Join(data, "projects", id, "workspace")
	os.MkdirAll(work, 0o700)
	cutoverGitRun(t, work, "init", "--quiet", "--initial-branch=main")
	var stream bytes.Buffer
	for i := range commits {
		message := fmt.Sprintf("Update note-%d.md", i%40)
		body := fmt.Sprintf("---\ntitle: Note %d\n---\n\nRevision %d of note %d.\n%s", i%40, i, i%40, strings.Repeat("A line of the note.\n", 20))
		fmt.Fprintf(&stream, "commit refs/heads/main\ncommitter Ada <ada@example.com> %d +0000\ndata %d\n%s\nM 100644 inline note-%d.md\ndata %d\n%s\n",
			1700000000+i, len(message), message, i%40, len(body), body)
	}
	cmd := exec.Command("git", "-C", work, "fast-import", "--quiet")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	cmd.Stdin = &stream
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	cutoverGitRun(t, work, "reset", "--quiet", "--hard", "main")
	return cutoverGitRun(t, work, "rev-parse", "HEAD")
}

// An import killed part way, as SIGKILL, running out of memory or a host
// crash do, is never served, and running the import again resumes it.
func TestStorageImportResumesAfterItWasKilled(t *testing.T) {
	data, file := cutoverInstance(t)
	want := manyCommits(t, data, "default", 1500)
	db := instanceDatabase(t, file)
	child := exec.Command(os.Args[0], "-test.run=^TestStorageImportChild$", "-test.count=1")
	child.Env = append(os.Environ(), "FMN_STORAGE_IMPORT_CHILD=1", "FMN_STORAGE_IMPORT_DATA="+data)
	var output bytes.Buffer
	child.Stdout, child.Stderr = &output, &output
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	stored := 0
	for deadline := time.Now().Add(2 * time.Minute); stored < 100; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			child.Process.Kill()
			t.Fatalf("the import stored %d revisions: %s", stored, output.String())
		}
		db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM revisions WHERE org = $1 AND notebook = 'default'`, db.Org).Scan(&stored)
	}
	child.Process.Signal(syscall.SIGKILL)
	if err := child.Wait(); err == nil || strings.Contains(output.String(), `"status":"imported"`) {
		t.Fatalf("the import finished before it was killed: %v %s", err, output.String())
	}
	refusesUnfinished(t, data, file, "a notebook whose import was killed")
	lines := importAll(t, data)
	if l := lines["default"]; l.Status != "resumed" || l.Head != want || l.Commits != 1500 {
		t.Fatalf("the import run again: %+v", l)
	}
	var out bytes.Buffer
	if err := runAdmin([]string{"storage", "verify", "--data", data}, nil, &out); err != nil {
		t.Fatal(err, out.String())
	}
	r, _, err := serve(t, data, file)
	if err != nil {
		t.Fatal("serving the resumed notebook:", err)
	}
	r.Close()
	// Finished, it is never imported over again.
	if l := importAll(t, data)["default"]; l.Status != "already imported" {
		t.Fatalf("a third import: %+v", l)
	}
}

// A killed import of a repository with merged branches can stop on a commit
// of a merged branch, off HEAD's first-parent line; it resumes from there.
// One the notebook took a revision on top of after it stopped never
// resumes, and the refusal names the recovery.
func TestStorageImportResumesOnlyItsOwnHistory(t *testing.T) {
	for _, scenario := range []string{"stopped on a merged branch", "a revision on top"} {
		t.Run(scenario, func(t *testing.T) {
			data, file := cutoverInstance(t)
			previousNotebook(t, data, "default")
			work := filepath.Join(data, "projects", "default", "workspace")
			cutoverGitRun(t, work, "checkout", "--quiet", "-b", "side", "HEAD~1")
			os.WriteFile(filepath.Join(work, "side.md"), []byte("# Side\n"), 0o600)
			cutoverGitRun(t, work, "add", "side.md")
			cutoverGitRun(t, work, "commit", "--quiet", "-m", "Side")
			cutoverGitRun(t, work, "checkout", "--quiet", "main")
			cutoverGitRun(t, work, "merge", "--quiet", "--no-ff", "-m", "Merge side", "side")
			want := cutoverGitRun(t, work, "rev-parse", "HEAD")
			// What a killed import of HEAD left: the history up to the merged
			// branch's commit, and its record that it began.
			stopped := filepath.Join(t.TempDir(), "stopped.git")
			cutoverGitRun(t, work, "clone", "--quiet", "--bare", "--no-local", work, stopped)
			cutoverGitRun(t, stopped, "update-ref", "refs/heads/main", "side")
			db := instanceDatabase(t, file)
			if err := catalog.BeginStorageImport(t.Context(), db, "default", want); err != nil {
				t.Fatal(err)
			}
			withNotes(t, data, db, func(notes *notestore.Store) {
				nb := notes.Notebook("default")
				if _, err := gitexport.ImportGitDir(t.Context(), nb, stopped); err != nil {
					t.Fatal(err)
				}
				if scenario == "a revision on top" {
					if _, err := nb.Apply(t.Context(), notestore.ChangeSet{Subject: "Update new.md", Author: gitobj.Signature{Name: "Ada", Email: "ada@example.com"}, Files: []notestore.FileChange{{Path: "new.md", Content: []byte("# New\n")}}}); err != nil {
						t.Fatal(err)
					}
				}
			})
			var out bytes.Buffer
			err := runAdmin([]string{"storage", "import", "--data", data}, nil, &out)
			l := storageLines(t, &out)["default"]
			if scenario == "a revision on top" {
				if err == nil || !strings.Contains(l.Error, "cannot resume") || !strings.Contains(l.Error, "restore the database from the dump taken at the freeze") {
					t.Fatalf("an import over a notebook with a revision on top: %v %+v", err, l)
				}
				return
			}
			if err != nil || l.Status != "resumed" || l.Head != want {
				t.Fatalf("the import run again: %v %+v", err, l)
			}
			if _, _, err := serve(t, data, file); err != nil {
				t.Fatal("serving the resumed notebook:", err)
			}
		})
	}
}
