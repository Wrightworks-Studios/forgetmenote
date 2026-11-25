package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/reposource"
)

func TestRepositoryMapHTTPAndMCPShareScope(t *testing.T) {
	app, store := fixture(t)
	configureSearchSource(t, app)
	before, _ := store.Status(t.Context())
	r := httptest.NewRequest("GET", app.origin+"/p/default/api/repository/map?query=Health", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("map unavailable: %d %s", w.Code, w.Body.String())
	}
	var fromHTTP reposource.MapResult
	if err := json.Unmarshal(w.Body.Bytes(), &fromHTTP); err != nil {
		t.Fatal(err)
	}
	client := actionClient(t, app, catalog.Bootstrap, "notes:read")
	raw := actionCall(t, client, "repository_source_map", map[string]any{"project": "default", "query": "Health"}, false)
	var fromMCP reposource.MapResult
	if err := json.Unmarshal(raw, &fromMCP); err != nil {
		t.Fatal(err)
	}
	if len(fromMCP.Entries) != 1 || fromMCP.Entries[0].Path != "health.go" || !reflect.DeepEqual(fromHTTP, fromMCP) {
		t.Fatal("map parity", fromHTTP, fromMCP)
	}
	if strings.Contains(string(raw), "SYNTHETIC_SOURCE_READY") {
		t.Fatal("map contains source body")
	}
	actionCall(t, client, "repository_source_map", map[string]any{"project": "missing"}, true)
	actionCall(t, client, "repository_source_map", map[string]any{"project": "default", "revision": strings.Repeat("a", 40)}, true)
	after, _ := store.Status(t.Context())
	if before.Revision != after.Revision {
		t.Fatal("map wrote notebook content")
	}
}

func TestRepositoryMapJevRequiresOwnerConsentAndCachesAbstentions(t *testing.T) {
	app := chatFixture(t)
	configureSearchSource(t, app)
	var calls atomic.Int32
	configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			Questions map[string]decision.Question `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if len(request.Questions) > 8 {
			t.Error("unbounded map classification batch")
		}
		answers := map[string]any{}
		for id, question := range request.Questions {
			if question.Type != "choice" {
				t.Error("classification must choose an existing role")
			}
			probabilities := map[string]float64{}
			for key := range question.Criteria.(map[string]any) {
				probabilities[key] = 0
			}
			probabilities["other"] = 1
			answers[id] = map[string]any{"type": "choice", "choice": "other", "confidence": 1, "probabilities": probabilities}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 100, "output_tokens": 20}})
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	manager := app.repositories
	manager.SetMapClassifier(app.sourceMapModel, app.classifySourceMap)
	if more, err := manager.EnrichMapPending(t.Context(), "default"); err != nil || more || calls.Load() != 0 {
		t.Fatal(more, err)
	}
	state, _ := manager.Status("default")
	cfg := state.IndexConfig
	cfg.MapJev = true
	if _, err := manager.ConfigureIndex("default", "alice", state.Generation, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Refresh(t.Context(), "default"); err != nil {
		t.Fatal(err)
	}
	manager.EnrichMapPending(t.Context(), "default")
	if calls.Load() != 0 {
		t.Fatal("map ignored owner's personal consent")
	}
	if w := proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if more, err := manager.EnrichMapPending(t.Context(), "default"); err != nil || !more || calls.Load() != 1 {
		t.Fatal("consented map classification failed", more, err, calls.Load())
	}
	if more, err := manager.EnrichMapPending(t.Context(), "default"); err != nil || more || calls.Load() != 1 {
		t.Fatal("abstention was reclassified", more, err)
	}
	result, err := manager.Map(t.Context(), "default", reposource.MapQuery{})
	if err != nil || len(result.Entries) != 1 || result.Entries[0].Label == nil || result.Entries[0].Label.Topic != "" {
		t.Fatal(result, err)
	}
}

func TestRepositoryMapRecoversUncertainPurchaseWithoutPayingAgain(t *testing.T) {
	app := chatFixture(t)
	configureSearchSource(t, app)
	var calls atomic.Int32
	configureSyntheticJev(t, app, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "temporarily unavailable", 503)
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	if w := proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	manager := app.repositories
	manager.SetMapClassifier(app.sourceMapModel, app.classifySourceMap)
	enable := func() {
		state, _ := manager.Status("default")
		cfg := state.IndexConfig
		cfg.MapJev = true
		if _, err := manager.ConfigureIndex("default", "alice", state.Generation, cfg); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Refresh(t.Context(), "default"); err != nil {
			t.Fatal(err)
		}
	}
	enable()
	if _, err := manager.EnrichMapPending(t.Context(), "default"); err == nil || calls.Load() != 1 {
		t.Fatal("expected one failed provider attempt", err, calls.Load())
	}
	enable()
	if more, err := manager.EnrichMapPending(t.Context(), "default"); err != nil || !more || calls.Load() != 1 {
		t.Fatal("uncertain purchase blocked progress or was paid again", more, err, calls.Load())
	}
	result, err := manager.Map(t.Context(), "default", reposource.MapQuery{})
	if err != nil || len(result.Entries) != 1 || result.Entries[0].Label == nil || result.Entries[0].Label.Topic != "" {
		t.Fatal("uncertain purchase must fall back to local navigation", result, err)
	}
	if !result.Entries[0].Label.Unavailable {
		t.Fatal("missing result was presented as a provider classification")
	}
}
