package workspace

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

// newTestNotes is an empty notes store in an org of its own, such as another
// instance restoring a backup has.
func newTestNotes(t testing.TB) *notestore.Store {
	t.Helper()
	blobs, err := blob.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	return openTestNotes(t, &testNotebook{db: pgtest.Org(t), blobs: blobs})
}

// importFile imports the bundle at path as notebook of notes, as the
// catalog's import does.
func importFile(t testing.TB, notes *notestore.Store, notebook, path string) error {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return ImportBundleDB(context.Background(), notes, notebook, t.TempDir(), f)
}

// openImported opens notebook of notes after an import, as the catalog does.
func openImported(t testing.TB, notes *notestore.Store, notebook string) *Store {
	t.Helper()
	s, err := OpenDB(context.Background(), notes, notebook, filepath.Join(t.TempDir(), notebook), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// restore imports a backup of s as notebook "restored" of a new instance's
// notes store and opens it.
func restore(t testing.TB, s *Store) *Store {
	t.Helper()
	notes := newTestNotes(t)
	if err := importFile(t, notes, "restored", backupFile(t, s)); err != nil {
		t.Fatal(err)
	}
	return openImported(t, notes, "restored")
}

// absent fails unless notes holds no notebook named notebook.
func absent(t testing.TB, notes *notestore.Store, notebook string) {
	t.Helper()
	if head, err := notes.Notebook(notebook).Head(context.Background()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("notebook %s exists at %v: %v", notebook, head, err)
	}
}

// gitBundle commits files with Git in a new repository, as a bundle made
// outside ForgetMeNote's write boundary could be, and bundles its history.
func gitBundle(t testing.TB, files map[string]string) string {
	t.Helper()
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=Outside", "-c", "user.email=outside@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	git("init", "--quiet", "--initial-branch=main")
	git("commit", "--quiet", "--allow-empty", "-m", "Initialize notes notebook")
	for name, content := range files {
		full := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "--all")
	git("commit", "--quiet", "-m", "Outside the notebook write boundary")
	bundle := filepath.Join(t.TempDir(), "notebook.bundle")
	git("bundle", "create", "--quiet", bundle, "--all")
	return bundle
}

// commitIDs lists notebook's commits, oldest first.
func commitIDs(t testing.TB, nb *notestore.Notebook) []string {
	t.Helper()
	revisions, err := nb.Revisions(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range revisions {
		ids = append(ids, r.ID.String())
	}
	return ids
}

// Backup, then import into another instance, gives the same commits, files,
// versions and note identities, and a notebook that takes new revisions.
func TestBackupRoundTripKeepsEveryRevision(t *testing.T) {
	s := backupTestStore(t)
	ctx := context.Background()
	note, _ := WithNoteID([]byte("# Idea\n"), strings.Repeat("b", 32))
	first, err := s.Save(ctx, "notes/idea.md", note, "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(ctx, "notes/idea.md", append(note, "More.\n"...), first.Revision, "writer"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(ctx, "assets/image.bin", []byte{0, 255, 10, 13, 0}, "", "writer"); err != nil {
		t.Fatal(err)
	}
	last, err := s.Save(ctx, "plain.md", []byte("# Plain\n"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	restored := restore(t, s)
	if want, got := commitIDs(t, s.notes), commitIDs(t, restored.notes); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("the import has commits %v, the notebook %v", got, want)
	}
	for _, read := range []struct{ path, revision string }{{"notes/idea.md", ""}, {"notes/idea.md", first.Revision}, {"assets/image.bin", ""}, {"plain.md", ""}} {
		want, err := s.Read(ctx, read.path, read.revision)
		if err != nil {
			t.Fatal(err)
		}
		got, err := restored.Read(ctx, read.path, read.revision)
		if err != nil || string(got.Content) != string(want.Content) || got.Revision != want.Revision || got.Version != want.Version {
			t.Fatalf("%s at %q: %+v %v, want %+v", read.path, read.revision, got, err, want)
		}
	}
	want, _ := s.notes.Stat(ctx, gitobj.ID{}, "notes/idea.md")
	got, _ := restored.notes.Stat(ctx, gitobj.ID{}, "notes/idea.md")
	if got.EntryID != want.EntryID || got.EntryID != strings.Repeat("b", 32) {
		t.Fatalf("the note's identity is %q, not %q", got.EntryID, want.EntryID)
	}
	if _, err = restored.Save(ctx, "plain.md", []byte("# Plain, restored\n"), last.Revision, "writer"); err != nil {
		t.Fatal("the restored notebook refused a save on its imported head:", err)
	}
}

// An import never touches a notebook that exists, and refuses it before
// reading the upload.
func TestImportRefusesAnExistingNotebook(t *testing.T) {
	s := backupTestStore(t)
	ctx := context.Background()
	doc, err := s.Save(ctx, "kept.md", []byte("# Kept"), "", "operator")
	if err != nil {
		t.Fatal(err)
	}
	other := backupTestStore(t)
	if _, err = other.Save(ctx, "other.md", []byte("# Other"), "", "operator"); err != nil {
		t.Fatal(err)
	}
	if err = importFile(t, s.noteStore, s.notebook, backupFile(t, other)); !errors.Is(err, ErrConflict) {
		t.Fatal("an import into an existing notebook:", err)
	}
	if now, err := s.Read(ctx, "kept.md", ""); err != nil || now.Revision != doc.Revision {
		t.Fatalf("the import changed the notebook: %+v %v", now, err)
	}
}

// A refused or failed import leaves no notebook behind, so importing the
// bundle again succeeds.
func TestFailedImportsStoreNothingAndCanBeRetried(t *testing.T) {
	source := backupTestStore(t)
	ctx := context.Background()
	if _, err := source.Save(ctx, "notes/a.md", []byte("# A\n"), "", "writer"); err != nil {
		t.Fatal(err)
	}
	good := backupFile(t, source)
	raw, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"not a bundle", "oversized", "storage failure", "cancelled", "invalid Trash index"} {
		t.Run(scenario, func(t *testing.T) {
			s := newTestStore(t) // its org's notes store, with failure injection
			notes := s.noteStore
			attempt, retry := good, func() {}
			var want error
			text := ""
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			switch scenario {
			case "not a bundle":
				attempt = filepath.Join(t.TempDir(), "note.md")
				if err := os.WriteFile(attempt, []byte("# not a Git bundle\n"), 0600); err != nil {
					t.Fatal(err)
				}
				want = ErrBundle
			case "oversized":
				t.Setenv("FMN_LIMIT_BUNDLE_BYTES", strconv.Itoa(len(raw)-1))
				want, retry = ErrBundleTooLarge, func() { t.Setenv("FMN_LIMIT_BUNDLE_BYTES", strconv.Itoa(len(raw))) }
			case "storage failure":
				retry = failCommits(t, s)
			case "cancelled":
				cancel()
				want = context.Canceled
			case "invalid Trash index":
				// It passes the bundle's checks, so its history is stored
				// before the notebook fails to open.
				attempt = gitBundle(t, map[string]string{trashIndexPath: `{"version":1,"records":[{"id":"x"}]}`})
				text = "invalid Trash entry"
			}
			f, err := os.Open(attempt)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			err = ImportBundleDB(ctx, notes, "restored", t.TempDir(), f)
			if err == nil || want != nil && !errors.Is(err, want) || !strings.Contains(err.Error(), text) {
				t.Fatalf("import: %v, want %v %q", err, want, text)
			}
			absent(t, notes, "restored")
			retry()
			if err := importFile(t, notes, "restored", good); err != nil {
				t.Fatal("the retry failed:", err)
			}
			if head, _ := notes.Notebook("restored").Head(context.Background()); head.Commit.String() != bundleHead(t, good) {
				t.Fatalf("the retry is at %s, the bundle at %s", head.Commit, bundleHead(t, good))
			}
		})
	}
}

// holdDeletes makes deleting notebook's row of table in db's org wait, as a
// slow database would, until release runs or the test ends.
func holdDeletes(t testing.TB, db *pg.DB, table, notebook string) (release func()) {
	t.Helper()
	name := "workspace_hold_delete_" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, strings.ToLower(db.Org+"_"+table))
	ctx := context.Background()
	hold, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { hold.Rollback(ctx) }) }
	t.Cleanup(func() {
		release()
		if _, err := db.Pool.Exec(ctx, "DROP TRIGGER IF EXISTS "+name+" ON "+table+"; DROP FUNCTION IF EXISTS "+name+"()"); err != nil {
			t.Errorf("stop holding deletes: %v", err)
		}
	})
	if _, err = hold.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", name); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Pool.Exec(ctx, "CREATE FUNCTION "+name+"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock_shared(hashtext('"+name+"')); RETURN OLD; END $$"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Pool.Exec(ctx, "CREATE TRIGGER "+name+" BEFORE DELETE ON "+table+" FOR EACH ROW WHEN (OLD.org = '"+db.Org+"' AND OLD.notebook = '"+notebook+"') EXECUTE FUNCTION "+name+"()"); err != nil {
		t.Fatal(err)
	}
	return release
}

// A failed import that stored history also removes the Git export the
// running exporter made of it, so a notebook imported later under the same
// ID exports and backs up afresh instead of diverging from that export.
func TestFailedImportRemovesItsExport(t *testing.T) {
	ctx := context.Background()
	source := backupTestStore(t)
	if _, err := source.Save(ctx, "notes/a.md", []byte("# A\n"), "", "writer"); err != nil {
		t.Fatal(err)
	}
	good := backupFile(t, source)
	s := newTestStore(t)
	notes := s.noteStore
	exporter, err := gitexport.New(notes, filepath.Join(t.TempDir(), "exports"), gitexport.Options{Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(exporter.Close)
	t.Cleanup(UseExporter(notes, exporter))
	exporter.Start()
	// The failed import's cleanup waits until the export holds what it
	// stored, as a background export within milliseconds of the store does.
	release := holdDeletes(t, testEnv(t, s).db, "notebook_heads", "restored")
	bad := gitBundle(t, map[string]string{trashIndexPath: `{"version":1,"records":[{"id":"x"}]}`})
	upload, err := os.Open(bad)
	if err != nil {
		t.Fatal(err)
	}
	defer upload.Close()
	failed := make(chan error, 1)
	go func() { failed <- ImportBundleDB(ctx, notes, "restored", t.TempDir(), upload) }()
	// The import stores the bundle's commits one at a time, so the notebook
	// exists at its first commit before it reaches the bundle's head.
	want := bundleHead(t, bad)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		head, err := notes.Notebook("restored").Head(ctx)
		if err == nil && head.Commit.String() == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the import stored %v, not the bundle's head %s: %v", head, want, err)
		}
	}
	if head, err := exporter.Sync(ctx, "restored"); err != nil || head.Commit.String() != want {
		t.Fatalf("the export is at %v, the bundle at %s: %v", head, want, err)
	}
	release()
	if err := <-failed; err == nil || !strings.Contains(err.Error(), "invalid Trash entry") {
		t.Fatal("an import with an invalid Trash index:", err)
	}
	absent(t, notes, "restored")
	if _, err := os.Stat(exporter.Path("restored")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the failed import kept its export:", err)
	}
	if err := importFile(t, notes, "restored", good); err != nil {
		t.Fatal("the retry failed:", err)
	}
	if head := bundleHead(t, backupFile(t, openImported(t, notes, "restored"))); head != bundleHead(t, good) {
		t.Fatalf("the retry's backup is at %s, its bundle at %s", head, bundleHead(t, good))
	}
}

// While a notebook is being imported, another import of it is a conflict and
// leaves the first to finish.
func TestImportsOfOneNotebookRunOneAtATime(t *testing.T) {
	source := backupTestStore(t)
	ctx := context.Background()
	doc, err := source.Save(ctx, "notes/a.md", []byte("# A\n"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(backupFile(t, source))
	if err != nil {
		t.Fatal(err)
	}
	notes := newTestNotes(t)
	upload, send := io.Pipe()
	first := make(chan error, 1)
	go func() { first <- ImportBundleDB(ctx, notes, "restored", t.TempDir(), upload) }()
	// The first import reads its upload only once it holds the notebook.
	if _, err = send.Write(raw[:1]); err != nil {
		t.Fatal(err)
	}
	if err = ImportBundleDB(ctx, notes, "restored", t.TempDir(), bytes.NewReader(raw)); !errors.Is(err, ErrConflict) {
		t.Fatal("a second import of a notebook being imported:", err)
	}
	if _, err = send.Write(raw[1:]); err != nil {
		t.Fatal(err)
	}
	send.Close()
	if err = <-first; err != nil {
		t.Fatal("the first import failed:", err)
	}
	if head, err := notes.Notebook("restored").Head(ctx); err != nil || head.Commit.String() != doc.Revision {
		t.Fatalf("the import is at %v, the notebook at %s: %v", head, doc.Revision, err)
	}
}
