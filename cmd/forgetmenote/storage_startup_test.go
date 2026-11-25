package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runtimeapp "forgetmenote/internal/app"
	"forgetmenote/internal/blob"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"
)

// cutoverInstance is a stopped instance at the cutover: its catalog lists
// notebook default, which the notes store lacks. It returns the data
// directory and the database's URL file.
func cutoverInstance(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("INSTANCE_CONFIG_FILE", "")
	data := t.TempDir()
	file := databaseURLFile(t, true)
	db := instanceDatabase(t, file)
	c, err := catalog.Open(data, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	withNotes(t, data, db, func(notes *notestore.Store) {
		if err := notes.Delete(t.Context(), "default"); err != nil {
			t.Fatal(err)
		}
	})
	return data, file
}

// withNotes runs fn with the instance's notes store, as a command does.
func withNotes(t *testing.T, data string, db *pg.DB, fn func(*notestore.Store)) {
	t.Helper()
	blobs, err := blob.OpenDir(data)
	if err != nil {
		t.Fatal(err)
	}
	defer blobs.Close()
	notes, err := notestore.Open(t.Context(), db, blobs, notestore.Options{ImportEntryID: workspace.ImportEntryID})
	if err != nil {
		t.Fatal(err)
	}
	defer notes.Close()
	fn(notes)
}

// serve opens the instance as serve does and returns its notebook default,
// or the error that refused it.
func serve(t *testing.T, data, file string) (*runtimeapp.Runtime, *workspace.Store, error) {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	db, err := pg.Open(context.Background(), strings.TrimSpace(string(raw)), "")
	if err != nil {
		t.Fatal(err)
	}
	r, err := runtimeapp.Open(runtimeapp.Options{Data: data, Origin: "http://localhost", Token: strings.Repeat("x", 32), Database: db})
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	t.Cleanup(func() { r.Close() })
	store, err := r.Catalog.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	return r, store, nil
}

func refusesStale(t *testing.T, data, file, why string) {
	t.Helper()
	if r, _, err := serve(t, data, file); err == nil || !strings.Contains(err.Error(), "notebook default is at") || !strings.Contains(err.Error(), "forgetmenote admin storage verify") {
		if r != nil {
			r.Close()
		}
		t.Fatalf("serving %s: %v", why, err)
	}
}

func refusesUnfinished(t *testing.T, data, file, why string) {
	t.Helper()
	if r, _, err := serve(t, data, file); err == nil || !strings.Contains(err.Error(), "notebook default: its storage import did not finish") || !strings.Contains(err.Error(), "forgetmenote admin storage import") {
		if r != nil {
			r.Close()
		}
		t.Fatalf("serving %s: %v", why, err)
	}
}

func importAll(t *testing.T, data string) map[string]storageLine {
	t.Helper()
	var out bytes.Buffer
	err := runAdmin([]string{"storage", "import", "--data", data}, nil, &out)
	lines := storageLines(t, &out)
	if err != nil {
		t.Fatalf("import: %v %+v", err, lines)
	}
	return lines
}

// A notebook whose repository the previous release committed to after the
// import is never served without those commits, here with the repository's
// references packed, and nor is one whose import stopped part way.
func TestStartupRefusesANotebookItsRepositoryIsAheadOf(t *testing.T) {
	t.Run("committed after the import", func(t *testing.T) {
		data, file := cutoverInstance(t)
		previousNotebook(t, data, "default")
		importAll(t, data)
		work := filepath.Join(data, "projects", "default", "workspace")
		os.WriteFile(filepath.Join(work, "idea.md"), []byte("---\nfmn-id: 0123456789abcdef0123456789abcdef\ntitle: Idea\n---\n\nEdited on the previous release.\n"), 0o600)
		cutoverGitRun(t, work, "commit", "--quiet", "-am", "Update idea.md")
		cutoverGitRun(t, work, "pack-refs", "--all")
		refusesStale(t, data, file, "a notebook whose repository the previous release committed to")
	})
	t.Run("an import that stopped", func(t *testing.T) {
		data, file := cutoverInstance(t)
		previousNotebook(t, data, "default")
		work := filepath.Join(data, "projects", "default", "workspace")
		truncated := filepath.Join(t.TempDir(), "truncated.git")
		cutoverGitRun(t, work, "clone", "--quiet", "--bare", "--no-local", work, truncated)
		cutoverGitRun(t, truncated, "update-ref", "refs/heads/main", "main~1")
		withNotes(t, data, instanceDatabase(t, file), func(notes *notestore.Store) {
			if _, err := gitexport.ImportGitDir(t.Context(), notes.Notebook("default"), truncated); err != nil {
				t.Fatal(err)
			}
		})
		refusesStale(t, data, file, "a notebook with part of its history")
	})
}

// The old repository stays after the cutover: a notebook that took new
// revisions since, here with its repository's HEAD detached, and one a purge
// rewrote are served.
func TestStartupServesNotebooksThatMovedOnFromTheirRepository(t *testing.T) {
	t.Run("saved after the cutover", func(t *testing.T) {
		data, file := cutoverInstance(t)
		previousNotebook(t, data, "default")
		importAll(t, data)
		cutoverGitRun(t, filepath.Join(data, "projects", "default", "workspace"), "checkout", "--quiet", "--detach")
		r, store, err := serve(t, data, file)
		if err != nil {
			t.Fatal("serving an imported notebook:", err)
		}
		if _, err := store.Save(t.Context(), "after.md", []byte("# After the cutover\n"), "", "writer"); err != nil {
			t.Fatal(err)
		}
		r.Close()
		if _, _, err := serve(t, data, file); err != nil {
			t.Fatal("serving a notebook saved after the cutover:", err)
		}
	})
	t.Run("purged after the cutover", func(t *testing.T) {
		data, file := cutoverInstance(t)
		head := previousNotebook(t, data, "default")
		importAll(t, data)
		withNotes(t, data, instanceDatabase(t, file), func(notes *notestore.Store) {
			nb := notes.Notebook("default")
			redact := func(f notestore.RewriteFile, content func() ([]byte, error)) ([]byte, bool, error) {
				raw, err := content()
				return bytes.ReplaceAll(raw, []byte("An idea"), []byte("A purged idea")), true, err
			}
			result, err := nb.Rewrite(t.Context(), 2, "purge-after-cutover", redact, notestore.RewriteOptions{Actor: "owner", Reason: "test", Scope: map[string]any{"kind": "text"}})
			if err != nil || result.Head.Commit.String() == head {
				t.Fatalf("purge: %v %v", result.Head, err)
			}
		})
		_, store, err := serve(t, data, file)
		if err != nil {
			t.Fatal("serving a notebook purged after the cutover:", err)
		}
		if doc, err := store.Read(t.Context(), "idea.md", ""); err != nil || !strings.Contains(string(doc.Content), "A purged idea") {
			t.Fatalf("idea.md: %q %v", doc.Content, err)
		}
	})
}

// An import that stopped before it verified the notebook and cleared its
// record is never served, even when it stored the whole history, or when
// the repository it imported is gone; running the import again verifies it.
func TestStartupRefusesANotebookWhoseImportDidNotFinish(t *testing.T) {
	for _, scenario := range []string{"stopped before it verified", "stopped part way, repository moved away"} {
		t.Run(scenario, func(t *testing.T) {
			data, file := cutoverInstance(t)
			head := previousNotebook(t, data, "default")
			work := filepath.Join(data, "projects", "default", "workspace")
			imported := filepath.Join(work, ".git")
			if scenario != "stopped before it verified" {
				imported = filepath.Join(t.TempDir(), "truncated.git")
				cutoverGitRun(t, work, "clone", "--quiet", "--bare", "--no-local", work, imported)
				cutoverGitRun(t, imported, "update-ref", "refs/heads/main", "main~1")
			}
			db := instanceDatabase(t, file)
			if err := catalog.BeginStorageImport(t.Context(), db, "default", head); err != nil {
				t.Fatal(err)
			}
			withNotes(t, data, db, func(notes *notestore.Store) {
				if _, err := gitexport.ImportGitDir(t.Context(), notes.Notebook("default"), imported); err != nil {
					t.Fatal(err)
				}
			})
			moved := filepath.Join(t.TempDir(), "workspace")
			if scenario != "stopped before it verified" {
				if err := os.Rename(work, moved); err != nil {
					t.Fatal(err)
				}
			}
			refusesUnfinished(t, data, file, "a notebook whose import did not finish")
			if scenario != "stopped before it verified" {
				if err := os.Rename(moved, work); err != nil {
					t.Fatal(err)
				}
			}
			importAll(t, data)
			if _, _, err := serve(t, data, file); err != nil {
				t.Fatal("serving the notebook once its import finished:", err)
			}
		})
	}
}
