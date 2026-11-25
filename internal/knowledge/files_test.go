package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/filestate"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"

	"github.com/jackc/pgx/v5"
)

// releaseA copies the files the file-based release wrote in
// testdata/release-a: a catalog with a local notebook "work" and a notebook
// "field" on an owner, then work's settings, retrieval settings, archived and
// active proposals, checkpoint and vectors, and field's settings and vectors in
// the older semantic.json. testdata/release-a-converted holds the semantic.bin
// that release wrote when it next opened field.
func releaseA(t *testing.T) string {
	t.Helper()
	data := t.TempDir()
	if err := os.CopyFS(data, os.DirFS("testdata/release-a")); err != nil {
		t.Fatal(err)
	}
	return data
}

// releaseANotes are work's notes, which the fixture's vectors embed.
var releaseANotes = map[string]string{
	"alpha.md": "---\ntags: [research]\n---\n# Alpha\nA synthetic observation about tides.\n",
	"beta.md":  "# Beta\nA synthetic note about orbits.\n",
}

// importFiles imports the catalog, which places each notebook, and then the
// knowledge files in one transaction, as import-files does.
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
	if _, err = catalog.ImportFiles(ctx, db, tx, src); err != nil {
		t.Fatal(err)
	}
	sum, err := ImportFiles(ctx, db, tx, src)
	if err == nil {
		err = tx.Commit(ctx)
	}
	return sum, err
}

// exportFiles exports the catalog and the knowledge files from one snapshot.
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
	if _, err = catalog.ExportFiles(ctx, db, tx, out); err != nil {
		t.Fatal(err)
	}
	sum, err := ExportFiles(ctx, db, tx, out)
	if err != nil {
		t.Fatal(err)
	}
	return sum, dir
}

// openWork opens work's engine on a workspace holding the notes its vectors
// embed.
func openWork(t *testing.T, db *pg.DB, data string) *Engine {
	t.Helper()
	store := openNotebook(t, filepath.Join(data, "projects", "work"), nil)
	for _, path := range []string{"alpha.md", "beta.md"} {
		if _, err := store.Save(context.Background(), path, []byte(releaseANotes[path]), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	e, err := Open(db, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

// An imported notebook keeps its settings, proposals, checkpoint and paid
// vectors, a notebook the file-based release kept on an owner keeps its
// converted legacy vectors where every notebook is now served, and both export
// to what the file-based release wrote.
func TestKnowledgeFilesRoundTrip(t *testing.T) {
	data := releaseA(t)
	db := pgtest.Org(t)
	imported, err := importFiles(t, db, data)
	if err != nil {
		t.Fatal(err)
	}
	if imported.Files != 6 {
		t.Fatalf("import read %d files", imported.Files)
	}
	ctx := context.Background()
	e := openWork(t, db, data)
	if e.Config() != testConfig() {
		t.Fatalf("the configuration was lost: %+v", e.Config())
	}
	if settings := e.RetrievalSettings().Settings; settings.Search.Archives != "exclude" || settings.AI.Drafts != "include" {
		t.Fatalf("the retrieval settings were lost: %+v", settings)
	}
	if checkpoints := e.Checkpoints(); len(checkpoints) != 1 || checkpoints[0].Summary != "Reviewed the synthetic snapshot" {
		t.Fatalf("the checkpoint was lost: %+v", checkpoints)
	}
	active, err := e.Proposals(ctx)
	if err != nil || len(active) != 1 || active[0].Group.Summary != "Add a gamma note" {
		t.Fatalf("the active proposal was lost: %+v %v", active, err)
	}
	archived, err := e.Decide(ctx, "bob", workspace.ContentHash([]byte("alice\x00fixture-dismissed")), "archive")
	if err != nil || archived.Status != "dismissed" || archived.Group.Summary != "Clarify the tide note" {
		t.Fatalf("the archived proposal was lost: %+v %v", archived, err)
	}
	field, err := Open(db, openNotebook(t, filepath.Join(data, "projects", "field"), nil))
	if err != nil {
		t.Fatal(err)
	}
	defer field.Close()
	if field.Config() != testConfig() || len(field.cache.Vectors) != 2 {
		t.Fatalf("the formerly owner-hosted notebook's state was lost: %+v %d", field.Config(), len(field.cache.Vectors))
	}

	exported, dir := exportFiles(t, db)
	if exported != imported {
		t.Fatalf("export %+v differs from import %+v", exported, imported)
	}
	written := 0
	for _, tree := range []string{"projects", "owner-state"} {
		filepath.WalkDir(filepath.Join(dir, tree), func(_ string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				written++
			}
			return err
		})
		err = filepath.WalkDir(filepath.Join("testdata/release-a", tree), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel("testdata/release-a", path)
			want, _ := os.ReadFile(path)
			if strings.HasSuffix(rel, "semantic.json") {
				// The file-based release converts the older vectors to the
				// semantic.bin it wrote in release-a-converted.
				if _, err := os.Stat(filepath.Join(dir, rel)); !os.IsNotExist(err) {
					t.Errorf("%s was exported in the older layout", rel)
				}
				rel = strings.TrimSuffix(rel, "semantic.json") + "semantic.bin"
				want, _ = os.ReadFile(filepath.Join("testdata/release-a-converted", rel))
			}
			if got, err := os.ReadFile(filepath.Join(dir, rel)); err != nil || len(want) == 0 || !bytes.Equal(got, want) {
				t.Errorf("%s changed in the round trip (%v)", rel, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if written != exported.Files {
		t.Fatalf("export wrote %d knowledge files and reported %d", written, exported.Files)
	}

	// The imported vectors answer searches without another submission.
	pending, err := e.Pending(ctx, 10)
	if err != nil || len(pending.Passages) != 0 || pending.Status.State != "ready" {
		t.Fatalf("imported vectors were not reused: %+v %v", pending, err)
	}
	for vector, want := range map[[3]float32]string{{1, 0, 0}: "alpha.md", {0, 0.6, 0.8}: "beta.md"} {
		results, err := e.Search(ctx, SearchQuery{Vector: vector[:], ConfigID: testConfig().ID()})
		if err != nil || len(results) == 0 || results[0].Path != want {
			t.Fatalf("search for %s found %+v %v", want, results, err)
		}
	}
}

// Changes made after the import export as the file-based release would have
// written them, and the export imports again.
func TestKnowledgeExportCarriesLaterChanges(t *testing.T) {
	data := releaseA(t)
	db := pgtest.Org(t)
	if _, err := importFiles(t, db, data); err != nil {
		t.Fatal(err)
	}
	e := openWork(t, db, data)
	changed := testConfig()
	changed.Model = "fixture-v2"
	if _, err := e.Configure(context.Background(), changed, true); err != nil {
		t.Fatal(err)
	}
	sum, dir := exportFiles(t, db)
	if sum.Files != 5 {
		t.Fatalf("export wrote %d files", sum.Files)
	}
	// A new configuration drops the stored vectors, as it removed semantic.bin.
	if _, err := os.Stat(filepath.Join(dir, "projects/work/cache/semantic.bin")); !os.IsNotExist(err) {
		t.Fatal("vectors of the replaced configuration were exported", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "projects/work/knowledge.json"))
	var stored state
	if err := json.Unmarshal(raw, &stored); err != nil || stored.Config != changed || len(stored.Proposals) != 1 {
		t.Fatalf("the new configuration was not exported: %v %+v", err, stored)
	}
	again, err := importFiles(t, pgtest.Org(t), dir)
	if err != nil || again != sum {
		t.Fatalf("the export imported as %+v (%v), not %+v", again, err, sum)
	}
}

// Legacy vectors import as the file-based release converts them: normalized,
// without keys or vectors that do not validate, and without a damaged revision.
func TestKnowledgeImportConvertsLegacyVectorsAsReleaseA(t *testing.T) {
	data := releaseA(t)
	legacy := filepath.Join(data, "owner-state/field/cache/semantic.json")
	raw, _ := os.ReadFile(legacy)
	var cache vectorCache
	if err := json.Unmarshal(raw, &cache); err != nil {
		t.Fatal(err)
	}
	cache.Revision = "damaged-revision-metadata"
	cache.Vectors["not-a-content-hash"] = []float32{1, 0, 0}
	cache.Vectors[strings.Repeat("c", 64)] = []float32{1, 0}
	cache.Vectors[strings.Repeat("d", 64)] = []float32{0, 3, 4}
	raw, _ = json.Marshal(cache)
	os.WriteFile(legacy, raw, 0600)
	db := pgtest.Org(t)
	if _, err := importFiles(t, db, data); err != nil {
		t.Fatal(err)
	}
	var revision string
	var count int
	var converted []byte
	err := db.Pool.QueryRow(context.Background(), `SELECT revision, (SELECT count(*) FROM knowledge_vectors v WHERE v.org = s.org AND v.notebook = s.notebook),
		(SELECT vector FROM knowledge_vectors v WHERE v.org = s.org AND v.notebook = s.notebook AND hash = $2)
		FROM knowledge_vector_sets s WHERE org = $1 AND notebook = 'field'`, db.Org, strings.Repeat("d", 64)).Scan(&revision, &count, &converted)
	if err != nil || revision != "" || count != 3 || !bytes.Equal(converted, encodeVector([]float32{0, 0.6, 0.8})) {
		t.Fatalf("legacy vectors were not converted as the file-based release converts them: %q %d %v", revision, count, err)
	}
}

// An import refuses what the file-based release refuses or never reads, and
// writes nothing.
func TestKnowledgeImportRefusesUnreadableFiles(t *testing.T) {
	cases := map[string]func(data string){
		"corrupt vectors": func(data string) {
			path := filepath.Join(data, "projects/work/cache/semantic.bin")
			raw, _ := os.ReadFile(path)
			raw[len(raw)/2] ^= 1
			os.WriteFile(path, raw, 0600)
		},
		"legacy vectors of another configuration": func(data string) {
			path := filepath.Join(data, "owner-state/field/cache/semantic.json")
			raw, _ := os.ReadFile(path)
			os.WriteFile(path, bytes.Replace(raw, []byte(testConfig().ID()), []byte(strings.Repeat("e", 64)), 1), 0600)
		},
		"unknown retrieval setting": func(data string) {
			os.WriteFile(filepath.Join(data, "projects/work/retrieval.json"), []byte(`{"version":1,"search":{"version":1,"archives":"lower","drafts":"include"},"ai":{"version":1,"archives":"exclude","drafts":"exclude"},"extra":true}`), 0600)
		},
		"misnamed archived proposal": func(data string) {
			files, _ := filepath.Glob(filepath.Join(data, "projects/work/proposal-archive/*.json"))
			os.Rename(files[0], filepath.Join(data, "projects/work/proposal-archive", strings.Repeat("f", 64)+".json"))
		},
		"unexpected archive entry": func(data string) {
			os.WriteFile(filepath.Join(data, "projects/work/proposal-archive/notes.txt"), []byte("synthetic"), 0600)
		},
		"local data of an owner-hosted notebook": func(data string) {
			os.MkdirAll(filepath.Join(data, "projects/field"), 0700)
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
			if err := db.Pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM documents WHERE org = $1) + (SELECT count(*) FROM knowledge_vector_sets WHERE org = $1)", db.Org).Scan(&n); err != nil || n != 0 {
				t.Fatal("a refused import wrote rows", n, err)
			}
		})
	}
	// Without the catalog, the engine directories have no known placement.
	src, err := filestate.OpenSource(releaseA(t))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	db := pgtest.Org(t)
	tx, err := db.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = ImportFiles(context.Background(), db, tx, src); err == nil {
		t.Fatal("knowledge files imported without a catalog")
	}
}
