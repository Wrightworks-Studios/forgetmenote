package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/workspace"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenAIEmbeddingEndpoint(t *testing.T) {
	for _, model := range []string{"text-embedding-3-small", "text-embedding-3-large", "text-embedding-ada-002"} {
		t.Run(model, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer synthetic-key" {
					t.Error("wrong OpenAI request or authentication")
				}
				var request map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if string(request["model"]) != fmt.Sprintf("%q", model) || string(request["encoding_format"]) != `"float"` || string(request["input"]) != `["first passage","second passage"]` {
					t.Errorf("wrong embedding request: %s", request)
				}
				if _, ok := request["truncate"]; ok {
					t.Error("sent Ollama-only option to OpenAI")
				}
				if model == "text-embedding-ada-002" {
					if _, ok := request["dimensions"]; ok {
						t.Error("Ada does not accept the dimensions option")
					}
				} else if string(request["dimensions"]) != "3" {
					t.Error("did not request the notebook's configured dimensions")
				}
				// Associate vectors with their input indices, regardless of response order.
				fmt.Fprintf(w, `{"model":%q,"data":[{"index":1,"embedding":[0,2,0]},{"index":0,"embedding":[3,0,0]}]}`, model)
			}))
			defer ts.Close()
			endpoint, err := NewEndpoint(ts.URL+"/v1/embeddings", "synthetic-key")
			if err != nil {
				t.Fatal(err)
			}
			got, err := endpoint.EmbedModel(t.Context(), model, 3, []string{"first passage", "second passage"}, true)
			if err != nil || !reflect.DeepEqual(got, [][]float32{{1, 0, 0}, {0, 1, 0}}) {
				t.Fatalf("vectors must be ordered and normalized: %v, %v", got, err)
			}
		})
	}
}

func TestOpenAIEmbeddingRejectsUnusableResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"duplicate index", `{"model":"text-embedding-3-small","data":[{"index":0,"embedding":[1,0,0]},{"index":0,"embedding":[0,1,0]}]}`, 200},
		{"missing index", `{"model":"text-embedding-3-small","data":[{"embedding":[1,0,0]},{"index":1,"embedding":[0,1,0]}]}`, 200},
		{"negative index", `{"model":"text-embedding-3-small","data":[{"index":-1,"embedding":[1,0,0]},{"index":1,"embedding":[0,1,0]}]}`, 200},
		{"outside batch", `{"model":"text-embedding-3-small","data":[{"index":0,"embedding":[1,0,0]},{"index":2,"embedding":[0,1,0]}]}`, 200},
		{"partial batch", `{"model":"text-embedding-3-small","data":[{"index":0,"embedding":[1,0,0]}]}`, 200},
		{"wrong dimensions", `{"model":"text-embedding-3-small","data":[{"index":0,"embedding":[1,0]},{"index":1,"embedding":[0,1,0]}]}`, 200},
		{"zero vector", `{"model":"text-embedding-3-small","data":[{"index":0,"embedding":[0,0,0]},{"index":1,"embedding":[0,1,0]}]}`, 200},
		{"wrong model", `{"model":"other-model","data":[{"index":0,"embedding":[1,0,0]},{"index":1,"embedding":[0,1,0]}]}`, 200},
		{"missing model", `{"data":[{"index":0,"embedding":[1,0,0]},{"index":1,"embedding":[0,1,0]}]}`, 200},
		{"provider error", `private provider diagnostic synthetic-key`, 401},
		{"invalid JSON", `private provider diagnostic synthetic-key`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer ts.Close()
			endpoint, err := NewEndpoint(ts.URL+"/v1/embeddings", "synthetic-key")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = endpoint.EmbedModel(t.Context(), "text-embedding-3-small", 3, []string{"one", "two"}, false); err == nil {
				t.Fatal("accepted an unusable response")
			} else if strings.Contains(err.Error(), "synthetic-key") || strings.Contains(err.Error(), "private provider") {
				t.Fatal("provider response leaked through the error")
			}
		})
	}
}

func TestEmbeddingEndpointValidationAndOutage(t *testing.T) {
	mode := "valid"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("wrong endpoint request")
		}
		var in struct {
			Model    string   `json:"model"`
			Input    []string `json:"input"`
			Truncate bool     `json:"truncate"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if in.Model != "fixture-v1" || in.Truncate {
			t.Error("wrong model or silent truncation")
		}
		if mode == "offline" {
			http.Error(w, "private endpoint diagnostic must not leak", 503)
			return
		}
		vectors := [][]float32{{1, 0, 0}}
		if mode == "partial" {
			vectors = nil
		}
		json.NewEncoder(w).Encode(map[string]any{"model": in.Model, "embeddings": vectors})
	}))
	defer ts.Close()
	endpoint, err := NewEndpoint(ts.URL+"/api/embed", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = endpoint.Embed(context.Background(), testConfig(), []string{"query"}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"offline", "partial"} {
		mode = value
		if _, err = endpoint.Embed(context.Background(), testConfig(), []string{"query"}); err == nil {
			t.Fatal("accepted unavailable or partial result")
		}
	}
}

func TestAsyncIndexOutageRetryAndSaveIndependence(t *testing.T) {
	e, doc := indexFixture(t)
	ctx := context.Background()
	var offline atomic.Bool
	offline.Store(true)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			http.Error(w, "offline", 503)
			return
		}
		var in struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		v := make([][]float32, len(in.Input))
		for i := range v {
			v[i] = []float32{1, 0, 0}
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": v})
	}))
	defer ts.Close()
	endpoint, _ := NewEndpoint(ts.URL+"/api/embed", "")
	config := testConfig()
	config.Endpoint = true
	e.Configure(ctx, config, false)
	e.Start(endpoint)
	defer e.Close()
	awaitIndex(t, e, func(s IndexStatus) bool { return s.State == "unavailable" })
	if _, err := e.Store.Save(ctx, "a.md", []byte("# Alpha\nA newer observation."), doc.Revision, ""); err != nil {
		t.Fatal("save depended on embeddings", err)
	}
	literal, err := e.Store.Find(ctx, "newer", "", "", "")
	if err != nil || len(literal) != 1 {
		t.Fatal("literal search failed during outage", literal, err)
	}
	offline.Store(false)
	e.signal()
	awaitIndex(t, e, func(s IndexStatus) bool { return s.State == "ready" })
	matches, err := e.Search(ctx, SearchQuery{Text: "observation"})
	if err != nil || len(matches) != 1 {
		t.Fatal(matches, err)
	}
}

func TestAsyncIndexShrinksRejectedBatchesAndCompletesCurrentAndHistory(t *testing.T) {
	e, doc := indexFixture(t)
	ctx := context.Background()
	for _, version := range []string{"Earlier", "Current"} {
		content := ""
		for i := 0; i < 16; i++ {
			content += fmt.Sprintf("## %s section %d\n%s observation %d.\n", version, i, version, i)
		}
		var err error
		doc, err = e.Store.Save(ctx, "a.md", []byte(content), doc.Revision, "")
		if err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	requests := []int{}
	indexed := map[string]int{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		requests = append(requests, len(in.Input))
		mu.Unlock()
		if len(in.Input) > 2 {
			http.Error(w, "synthetic endpoint batch capacity exceeded", http.StatusRequestTimeout)
			return
		}
		vectors := make([][]float32, len(in.Input))
		mu.Lock()
		for i, text := range in.Input {
			indexed[text]++
			vectors[i] = []float32{1, 0, 0}
		}
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	}))
	defer ts.Close()
	endpoint, err := NewEndpoint(ts.URL+"/api/embed", "")
	if err != nil {
		t.Fatal(err)
	}
	config := testConfig()
	config.Endpoint = true
	if _, err = e.Configure(ctx, config, false); err != nil {
		t.Fatal(err)
	}
	if _, err = e.PendingScope(ctx, 32, "history"); err != nil {
		t.Fatal(err)
	}
	if err = e.Start(endpoint); err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	awaitIndex(t, e, func(s IndexStatus) bool {
		return s.State == "ready" && s.History != nil && s.History.State == "ready"
	})
	mu.Lock()
	defer mu.Unlock()
	if len(requests) < 4 || requests[0] <= 2 {
		t.Fatalf("fixture did not exercise rejected batches: %v", requests)
	}
	for i := 1; i < len(requests); i++ {
		if requests[i-1] > 2 && requests[i] >= requests[i-1] {
			t.Fatalf("failed batch did not shrink: %v", requests)
		}
		if requests[i-1] <= 2 && requests[i] > 2 {
			t.Fatalf("worker forgot the endpoint's learned capacity: %v", requests)
		}
	}
	if len(indexed) != 33 {
		t.Fatalf("passages were dropped or duplicated: %d; batches %v", len(indexed), requests)
	}
	for text, count := range indexed {
		if count != 1 {
			t.Fatalf("successfully embedded passage was retried %d times: %q", count, text)
		}
	}
}
func awaitIndex(t *testing.T, e *Engine, accept func(IndexStatus) bool) {
	t.Helper()
	// Real Git reads and cache writes compete with the other race-test packages.
	until := time.Now().Add(30 * time.Second)
	for time.Now().Before(until) {
		p, err := e.Pending(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if accept(p.Status) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	p, _ := e.Pending(context.Background(), 1)
	status, _ := json.Marshal(p.Status)
	t.Fatalf("index state not reached: %s", status)
}

func TestEmbeddingGemmaRetrievalInputs(t *testing.T) {
	var inputs [][]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model    string   `json:"model"`
			Input    []string `json:"input"`
			Truncate bool     `json:"truncate"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		if in.Truncate {
			t.Error("silently truncated evidence")
		}
		inputs = append(inputs, in.Input)
		json.NewEncoder(w).Encode(map[string]any{"model": in.Model, "embeddings": [][]float32{{1, 0, 0}}})
	}))
	defer ts.Close()
	endpoint, err := NewEndpoint(ts.URL+"/api/embed", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		model      string
		document   bool
		text, want string
	}{
		{"embeddinggemma:latest", false, "Where is readiness checked?", "task: search result | query: Where is readiness checked?"},
		{"embeddinggemma:latest", true, "src/health.py\nHealth checks database readiness.", "title: none | text: src/health.py\nHealth checks database readiness."},
		{"fixture-v1", false, "query", "query"},
		{"fixture-v1", true, "document", "document"},
	} {
		texts := []string{tc.text}
		if _, err = endpoint.EmbedModel(context.Background(), tc.model, 3, texts, tc.document); err != nil {
			t.Fatal(err)
		}
		got := inputs[len(inputs)-1]
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s document=%v sent %q; want %q", tc.model, tc.document, got, tc.want)
		}
		if texts[0] != tc.text {
			t.Fatal("mutated caller's original evidence")
		}
	}
}

func TestEmbeddingGemmaWorkerUsesDocumentInputs(t *testing.T) {
	e, _ := indexFixture(t)
	var wrong atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		vectors := make([][]float32, len(in.Input))
		for i, text := range in.Input {
			if !strings.HasPrefix(text, "title: none | text: ") {
				wrong.Store(true)
			}
			vectors[i] = []float32{1, 0, 0}
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	}))
	defer ts.Close()
	endpoint, _ := NewEndpoint(ts.URL+"/api/embed", "")
	config := testConfig()
	config.Endpoint = true
	config.Model = "embeddinggemma:latest"
	if _, err := e.Configure(context.Background(), config, false); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(endpoint); err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	awaitIndex(t, e, func(s IndexStatus) bool { return s.State == "ready" })
	if wrong.Load() {
		t.Fatal("background notebook passages did not use document embeddings")
	}
}

func TestEmbeddingGemmaRejectsUnprefixedVectorSubmission(t *testing.T) {
	e, _ := indexFixture(t)
	defer e.Close()
	config := testConfig()
	config.Model, config.Endpoint = "embeddinggemma:latest", true
	if _, err := e.Configure(context.Background(), config, false); err != nil {
		t.Fatal(err)
	}
	pending, err := e.Pending(context.Background(), 10)
	if err != nil || len(pending.Passages) == 0 {
		t.Fatal(pending, err)
	}
	p := pending.Passages[0]
	legacy := workspace.ContentHash([]byte(`{"Model":"embeddinggemma:latest","Dimensions":3,"PassageRunes":128,"Splitter":"markdown-heading-v1"}`))
	sub := Submission{Revision: pending.Status.Revision, ConfigID: legacy, Vectors: []VectorSubmission{{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: []float32{1, 0, 0}}}}
	if _, err := e.Submit(context.Background(), sub); err == nil {
		t.Fatal("accepted vectors made without document prefixes")
	}
	sub.ConfigID = pending.Status.ConfigID
	if _, err := e.Submit(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
}
