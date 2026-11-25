package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
)

func TestEmbeddingPolicyCoversExistingAndLaterLazyEngines(t *testing.T) {
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		calls.Add(1)
		vectors := make([][]float32, len(input.Input))
		for i := range vectors {
			vectors[i] = []float32{1, 0, 0}
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	}))
	t.Cleanup(endpoint.Close)
	app := chatFixture(t)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureEmbeddings(endpoint.URL+"/api/embed", ""); err != nil {
		t.Fatal(err)
	}
	configure := func(project string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"config": knowledge.Config{Enabled: true, Endpoint: true, Model: "synthetic", Dimensions: 3, PassageRunes: 256, Splitter: "markdown-heading-v1"}})
		response := proxyCall(app, "alice", "POST", "/p/"+project+"/api/knowledge/config", string(body))
		if response.Code != 200 {
			t.Fatal("configure", response.Code, response.Body)
		}
	}
	search := func(project string, allowed bool) {
		t.Helper()
		before := calls.Load()
		response := proxyCall(app, "alice", "POST", "/p/"+project+"/api/knowledge/search", `{"text":"private synthetic query"}`)
		if allowed {
			if response.Code != 200 || calls.Load() != before+1 {
				t.Fatalf("permitted embedding did not run: status=%d calls=%d body=%s", response.Code, calls.Load()-before, response.Body)
			}
		} else if response.Code != 500 || !strings.Contains(response.Body.String(), "compatible query-embedding endpoint") || calls.Load() != before {
			t.Fatalf("local-only policy lost: status=%d calls=%d body=%s", response.Code, calls.Load()-before, response.Body)
		}
	}
	policy := func(local bool) {
		t.Helper()
		app.chat.Mu.Lock()
		config := *app.chat.Config
		app.chat.Mu.Unlock()
		config.LocalOnly = true
		config.EmbeddingsLocal = local
		raw, _ := json.Marshal(config)
		file := filepath.Join(t.TempDir(), "chat.json")
		if err := os.WriteFile(file, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := app.ConfigureChat(file); err != nil {
			t.Fatal(err)
		}
	}
	configure("default")
	search("default", true)
	policy(false)
	search("default", false)
	// Replacing endpoint configuration must still apply the current privacy policy.
	if err := app.ConfigureEmbeddings(endpoint.URL+"/api/embed", ""); err != nil {
		t.Fatal(err)
	}
	search("default", false)
	// This notebook did not exist in either eager initialization or the restart snapshot.
	if err := app.catalog.Create(catalog.Bootstrap, "later", "Later"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "later", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	configure("later")
	search("later", false)
	policy(true)
	search("default", true)
	search("later", true)
}

func TestExtractedSearchPreservesInstancePolicyConflict(t *testing.T) {
	app, store := fixture(t)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer provider.Close()
	engine, releaseEngine, err := app.acquireEngine(t.Context(), store)
	if releaseEngine != nil {
		t.Cleanup(releaseEngine)
	}

	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Configure(t.Context(), knowledge.Config{Enabled: true, Endpoint: true, Model: "synthetic", Dimensions: 3, PassageRunes: 256, Splitter: "markdown-heading-v1"}, false); err != nil {
		t.Fatal(err)
	}
	endpoint, err := knowledge.NewEndpoint(provider.URL+"/api/embed", "")
	if err != nil {
		t.Fatal(err)
	}
	// The concrete policy guard returns the same sentinel installed by
	// ConfigureEmbeddings, before transmitting any query or passage.
	endpoint.Authorize = func(string, int) error { return errInstanceAI }
	if err = app.indexes.Configure(endpoint); err != nil {
		t.Fatal(err)
	}
	response := call(app, "POST", "/api/knowledge/search", `{"text":"private query"}`, testToken)
	var detail struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if response.Code != 409 || json.Unmarshal(response.Body.Bytes(), &detail) != nil || detail.Code != "conflict" || detail.Error != errInstanceAI.Error() || calls.Load() != 0 {
		t.Fatalf("instance-policy conflict changed across extraction: status=%d calls=%d body=%s", response.Code, calls.Load(), response.Body)
	}
}
