package web

import (
	"bytes"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/pg/pgtest"
)

func TestDamagedStoredVectorsRemainNotebookLocal(t *testing.T) {
	dir := t.TempDir()
	projects, err := catalog.Open(dir, nil, pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer projects.Close()
	store, err := projects.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(t.Context(), "a.md", []byte("# Synthetic\nDamage isolation.\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	db := pgtest.Org(t)
	engine, err := knowledge.Open(db, store)
	if err != nil {
		t.Fatal(err)
	}
	config := knowledge.Config{Enabled: true, Model: "synthetic", Dimensions: 3, PassageRunes: 256, Splitter: "markdown-heading-v1"}
	if _, err = engine.Configure(t.Context(), config, false); err != nil {
		engine.Close()
		t.Fatal(err)
	}
	pending, err := engine.Pending(t.Context(), 1)
	if err != nil || len(pending.Passages) != 1 {
		engine.Close()
		t.Fatalf("fixture pending: %+v %v", pending, err)
	}
	p := pending.Passages[0]
	_, err = engine.Submit(t.Context(), knowledge.Submission{Revision: pending.Status.Revision, ConfigID: pending.Status.ConfigID, Vectors: []knowledge.VectorSubmission{{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: []float32{1, 0, 0}}}})
	engine.Close()
	if err != nil {
		t.Fatal(err)
	}
	damaged := []byte("damaged vector with retained diagnostic bytes")
	if _, err = db.Pool.Exec(t.Context(), `UPDATE knowledge_vectors SET vector = $3 WHERE org = $1 AND notebook = $2`, db.Org, "default", damaged); err != nil {
		t.Fatal(err)
	}
	app, err := New(projects, db, dir, "http://localhost", testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if err = app.ConfigureEmbeddings("http://127.0.0.1:1/api/embed", ""); err != nil {
		t.Fatal("damaged derived cache prevented instance configuration", err)
	}
	if err = app.Start(t.Context()); err != nil {
		t.Fatal("damaged derived cache prevented startup", err)
	}
	if response := call(app, "GET", "/api/status", "", testToken); response.Code != 200 {
		t.Fatal("ordinary notebook read unavailable", response.Code)
	}
	if response := call(app, "GET", "/api/knowledge/status", "", testToken); response.Code != 500 {
		t.Fatal("damaged cache did not reach its retrieval error boundary", response.Code)
	}
	var after []byte
	if err = db.Pool.QueryRow(t.Context(), `SELECT vector FROM knowledge_vectors WHERE org = $1 AND notebook = $2`, db.Org, "default").Scan(&after); err != nil || !bytes.Equal(damaged, after) {
		t.Fatal("failed retrieval changed retained cache", err)
	}
}
