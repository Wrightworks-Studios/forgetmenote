package web

import (
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg/pgtest"
	"testing"
)

func TestUnreadableRetrievalSettingsRemainNotebookLocalAtStartup(t *testing.T) {
	dir := t.TempDir()
	projects, err := catalog.Open(dir, nil, pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer projects.Close()
	db := pgtest.Org(t)
	if _, err = db.WriteDocument(t.Context(), db.Pool, "knowledge-retrieval", "default", 0, []byte(`{"version":2}`)); err != nil {
		t.Fatal(err)
	}
	app, err := New(projects, db, dir, "http://localhost", testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if err = app.ConfigureEmbeddings("http://127.0.0.1:1/api/embed", ""); err != nil {
		t.Fatalf("configuration failed instead of isolating bad retrieval settings: %v", err)
	}
	if err = app.Start(t.Context()); err != nil {
		t.Fatalf("isolated notebook retrieval settings prevented instance startup: %v", err)
	}
	response := call(app, "GET", "/api/status", "", testToken)
	if response.Code != 200 {
		t.Fatalf("ordinary notebook reads unavailable: %d %s", response.Code, response.Body.String())
	}
}
