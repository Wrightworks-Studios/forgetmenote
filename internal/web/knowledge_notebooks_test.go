package web

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/pg/pgtest"
)

// knowledgeNotebook serves notebook alpha, whose knowledge state an earlier
// process configured and whose history it searched, beside the default
// notebook, as a restarted gateway does.
func knowledgeNotebook(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	projects, err := catalog.Open(dir, nil, pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if err = projects.Create(catalog.Bootstrap, "alpha", "Alpha"); err != nil {
		t.Fatal(err)
	}
	alpha, err := projects.Access(catalog.Bootstrap, "alpha", "edit")
	if err != nil {
		t.Fatal(err)
	}
	note, err := alpha.Save(context.Background(), "history.md", []byte("# Archived evidence\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = alpha.Save(context.Background(), note.Path, []byte("# Current text\n"), note.Revision, ""); err != nil {
		t.Fatal(err)
	}
	db := pgtest.Org(t)
	primed, err := knowledge.Open(db, alpha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = primed.Configure(context.Background(), knowledge.Config{Enabled: true, Endpoint: true, Model: "fixture-v1", Dimensions: 3, PassageRunes: 256, Splitter: "markdown-heading-v1"}, false); err != nil {
		t.Fatal(err)
	}
	result, err := primed.HybridSearch(context.Background(), knowledge.HybridQuery{SearchQuery: knowledge.SearchQuery{Text: "Archived evidence", Scope: "history"}, Mode: "text"})
	if err != nil || len(result.Matches) != 1 {
		t.Fatalf("seed history search: %+v %v", result, err)
	}
	projects.Close()
	projects, err = catalog.Open(dir, nil, pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(projects.Close)
	app, err := New(projects, db, dir, "http://localhost", testToken)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app
}

func knowledgeCall(app *Server, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, app.origin+path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	return response
}

func TestKnowledgeInitializationAndCloseAreCoordinated(t *testing.T) {
	app := knowledgeNotebook(t)
	if err := app.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if response := knowledgeCall(app, "POST", "/p/alpha/api/knowledge/hybrid-search", `{"text":"Archived evidence","scope":"history","mode":"text"}`); response.Code != 200 || !strings.Contains(response.Body.String(), "Archived evidence") {
		t.Fatalf("history search after restart: %d %s", response.Code, response.Body)
	}
	alpha, err := app.catalog.Access(catalog.Bootstrap, "alpha", "read")
	if err != nil {
		t.Fatal(err)
	}
	type initialized struct {
		engine *knowledge.Engine
		err    error
	}
	results := make(chan initialized, 24)
	for i := 0; i < cap(results); i++ {
		go func() {
			e, releaseEngine, err := app.acquireEngine(t.Context(), alpha)
			if releaseEngine != nil {
				defer releaseEngine()
			}
			results <- initialized{e, err}
		}()
	}
	var engine *knowledge.Engine
	for i := 0; i < cap(results); i++ {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatal(result.err)
			}
			if engine != nil && engine != result.engine {
				t.Fatal("same notebook initialized multiple workers")
			}
			engine = result.engine
		case <-time.After(5 * time.Second):
			t.Fatal("initialization did not finish")
		}
	}
	configured := make(chan error, 2)
	for i := 0; i < cap(configured); i++ {
		go func() { configured <- app.ConfigureEmbeddings("http://localhost:11434/api/embed", "") }()
	}
	for i := 0; i < cap(configured); i++ {
		select {
		case err := <-configured:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("provider update did not finish")
		}
	}
	closed := make(chan struct{})
	go func() { app.Close(); close(closed) }()
	var requests sync.WaitGroup
	for i := 0; i < 24; i++ {
		requests.Add(1)
		go func() {
			defer requests.Done()
			e, releaseEngine, err := app.acquireEngine(t.Context(), alpha)
			if releaseEngine != nil {
				defer releaseEngine()
			}
			if err == nil && e != engine {
				t.Error("close race created another worker")
			}
		}()
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	requests.Wait()
	if _, _, err = app.acquireEngine(t.Context(), alpha); err == nil {
		t.Fatal("closed server accepted initialization")
	}
	if err = engine.Start(nil); err == nil {
		t.Fatal("closed engine restarted")
	}
}

func TestKnowledgeInitializationFailureCanRetry(t *testing.T) {
	app := knowledgeNotebook(t)
	db := app.db
	version, before, err := db.ReadDocument(t.Context(), db.Pool, "knowledge", "alpha", -1)
	if err != nil || version == 0 {
		t.Fatal("seeded knowledge state", err)
	}
	if version, err = db.WriteDocument(t.Context(), db.Pool, "knowledge", "alpha", version, []byte(`{"config":"invalid synthetic config"}`)); err != nil {
		t.Fatal(err)
	}
	if err = app.ConfigureEmbeddings("http://localhost:11434/api/embed", ""); err == nil || !strings.Contains(err.Error(), "knowledge state is unreadable") {
		t.Fatalf("invalid local configuration was hidden: %v", err)
	}
	if response := knowledgeCall(app, "GET", "/p/default/api/knowledge/status", ""); response.Code != 200 {
		t.Fatalf("other notebook failed: %d %s", response.Code, response.Body)
	}
	if _, err = db.WriteDocument(t.Context(), db.Pool, "knowledge", "alpha", version, before); err != nil {
		t.Fatal(err)
	}
	if err = app.ConfigureEmbeddings("http://localhost:11434/api/embed", ""); err != nil {
		t.Fatalf("repaired notebook did not retry initialization: %v", err)
	}
}
