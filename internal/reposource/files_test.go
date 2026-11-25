package reposource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/blob/blobtest"
	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"

	"github.com/jackc/pgx/v5"
)

// The fixture's notebooks, written by the file-based release's own save: alpha
// with a connected source beside a disconnected one, beta-notes fenced by an
// authority handoff, and gamma with a historical-only source.
var fixtureNotebooks = []string{"alpha", "beta-notes", "gamma"}

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
	return importFilesInto(t, db, data, nil)
}

// importFilesInto imports data with blobs as the instance's blob storage, or
// with data itself when blobs is nil.
func importFilesInto(t *testing.T, db *pg.DB, data string, blobs blob.Store) (filestate.Sum, error) {
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

// exportFiles exports db's state with blobs as the instance's blob storage.
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

// Imported source state keeps its sources, fences and history, and exports
// back to the files the file-based release wrote, byte for byte.
func TestRepositorySourceFilesRoundTrip(t *testing.T) {
	data := releaseA(t)
	root := filepath.Join(data, "repository-sources")
	// A notebook with only a cache has no state, as there.
	os.MkdirAll(filepath.Join(root, "delta", "0123456789abcdef0123456789abcdef"), 0700)
	store, err := blob.OpenDir(data)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db := pgtest.Org(t)
	imported, err := importFiles(t, db, data)
	if err != nil {
		t.Fatal(err)
	}
	if imported.Files != len(fixtureNotebooks) {
		t.Fatalf("import read %d files", imported.Files)
	}
	m, err := Open(db, root, nil, &GitHub{}, func(project, actor string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if projects := m.Projects(); !slices.Equal(projects, fixtureNotebooks) {
		t.Fatalf("notebooks with sources: %v", projects)
	}
	alpha, err := m.Status("alpha")
	if err != nil || !alpha.Connected || alpha.Generation != 5 || alpha.Error != "GitHub answered 502 — retrying <later>" || !alpha.IndexConfig.Embeddings || alpha.Repository.FullName != "example/source" {
		t.Fatalf("alpha's source was lost: %+v %v", alpha, err)
	}
	if beta, err := m.Status("beta-notes"); err != nil || beta.Handoff != "handoff-2" || !beta.NotebookLayout {
		t.Fatalf("beta-notes lost its handoff fence: %+v %v", beta, err)
	}
	if _, err = m.Control("beta-notes", "owner", "pause"); !errors.Is(err, ErrConflict) {
		t.Fatal("the imported fence no longer holds", err)
	}
	if gamma, err := m.Status("gamma"); err != nil || !gamma.HistoricalOnly {
		t.Fatalf("gamma's historical source was lost: %+v %v", gamma, err)
	}
	exported, dir := exportFiles(t, db, store)
	if exported != imported {
		t.Fatalf("export %+v differs from import %+v", exported, imported)
	}
	for _, project := range fixtureNotebooks {
		want, _ := os.ReadFile(filepath.Join("testdata/release-a", stateFile(project)))
		got, err := os.ReadFile(filepath.Join(dir, stateFile(project)))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s changed in the round trip (%v):\n%s\n%s", project, err, want, got)
		}
	}

	// A change made here exports as the file-based release would write it.
	if _, err = m.Control("alpha", "owner", "pause"); err != nil {
		t.Fatal(err)
	}
	_, dir = exportFiles(t, db, store)
	raw, _ := os.ReadFile(filepath.Join(dir, stateFile("alpha")))
	var state diskState
	if err = json.Unmarshal(raw, &state); err != nil || !state.Sources[state.Active].Paused || state.Sources[state.Active].Generation != 6 {
		t.Fatalf("the paused source exported as %s: %v", raw, err)
	}
	// The export passes the file-based release's checks.
	if _, err = importFiles(t, pgtest.Org(t), dir); err != nil {
		t.Fatal("the export does not import again:", err)
	}
}

// An import refuses what the file-based release refuses when it opens, and
// writes nothing.
func TestRepositorySourceImportRefusesInvalidFiles(t *testing.T) {
	cases := map[string]func(root string){
		"truncated state": func(root string) {
			os.WriteFile(filepath.Join(root, "alpha", "state.json"), []byte(`{"version":1,`), 0600)
		},
		"newer version": func(root string) {
			raw, _ := os.ReadFile(filepath.Join(root, "alpha", "state.json"))
			os.WriteFile(filepath.Join(root, "alpha", "state.json"), bytes.Replace(raw, []byte(`{"version":1,`), []byte(`{"version":3,`), 1), 0600)
		},
		"another notebook's state": func(root string) {
			raw, _ := os.ReadFile(filepath.Join(root, "alpha", "state.json"))
			os.WriteFile(filepath.Join(root, "gamma", "state.json"), raw, 0600)
		},
		"missing active source": func(root string) {
			os.WriteFile(filepath.Join(root, "gamma", "state.json"), []byte(`{"version":1,"active":"missing","sources":{}}`), 0600)
		},
		"unknown entry": func(root string) {
			os.WriteFile(filepath.Join(root, "notes.txt"), []byte("stray"), 0600)
		},
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			data := releaseA(t)
			corrupt(filepath.Join(data, "repository-sources"))
			db := pgtest.Org(t)
			if _, err := importFiles(t, db, data); err == nil {
				t.Fatal("the import accepted it")
			}
			var n int
			if err := db.Pool.QueryRow(context.Background(), "SELECT count(*) FROM documents WHERE org = $1", db.Org).Scan(&n); err != nil || n != 0 {
				t.Fatal("a refused import wrote source state", n, err)
			}
		})
	}
	if sum, err := importFiles(t, pgtest.Org(t), t.TempDir()); err != nil || sum.Files != 0 {
		t.Fatal("an instance without repository sources failed to import", sum, err)
	}
}

// files reads every regular file under dir by its slash path.
func files(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)], err = os.ReadFile(p)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A source's index generation and retained evidence move into S3 with its
// state, and back out byte for byte. The reports match each other and an
// import that left the files in the data directory, and a gateway with an
// empty disk serves what moved without paying for it again.
func TestRepositorySourceBlobsRoundTripThroughS3(t *testing.T) {
	ctx := t.Context()
	// A file-based instance's data directory: the release's cache code wrote
	// its sources' files, and its state files come from an export.
	live := t.TempDir()
	f := newSourceFixture(t, filepath.Join(live, sourcesDir), nil)
	f.buyIndex(t)
	s, _ := f.m.Status("alpha")
	retained, err := f.m.Capture(ctx, "alpha", s.ID, s.Downloaded, []string{"README.md"})
	if err != nil {
		t.Fatal(err)
	}
	liveStore, err := blob.OpenDir(live)
	if err != nil {
		t.Fatal(err)
	}
	defer liveStore.Close()
	_, data := exportFiles(t, f.m.db, liveStore)
	original := files(t, data)
	var durable []string
	for rel := range original {
		if !strings.HasSuffix(rel, "/state.json") && !strings.HasSuffix(rel, "/index/current.json") {
			durable = append(durable, "instance/"+rel)
		}
	}
	slices.Sort(durable)
	if len(durable) != len(durableKeys(t, f, retained[0])) {
		t.Fatalf("the file-based data directory holds %q", slices.Sorted(maps.Keys(original)))
	}

	here, err := importFiles(t, pgtest.Org(t), data)
	if err != nil {
		t.Fatal(err)
	}
	s3 := blobtest.Start(t)
	objects := s3.Store(t, "instance")
	db := pgtest.Org(t)
	imported, err := importFilesInto(t, db, data, objects)
	if err != nil {
		t.Fatal(err)
	}
	if imported != here || imported.Files != len(original) {
		t.Fatalf("import into S3 %+v, in place %+v, of %d files", imported, here, len(original))
	}
	if stored := slices.Sorted(maps.Keys(s3.Objects())); !slices.Equal(stored, durable) {
		t.Fatalf("S3 holds %q, want %q", stored, durable)
	}
	exported, dir := exportFiles(t, db, objects)
	if exported != imported {
		t.Fatalf("export %+v differs from import %+v", exported, imported)
	}
	if got := files(t, dir); !maps.EqualFunc(got, original, bytes.Equal) {
		t.Fatalf("the round trip changed files: %q, want %q", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(original)))
	}

	m, err := Open(db, t.TempDir(), objects, f.m.github, f.m.check)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.cacheFor = f.m.cacheFor
	freeloader(t, m)
	servesAlpha(t, m, s, retained[0])
	if more, err := m.EmbedPending(ctx, "alpha"); err != nil || more {
		t.Fatal("embedding what moved:", more, err)
	}
}
