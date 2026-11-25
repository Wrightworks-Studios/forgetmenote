package knowledge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestQueryEmbeddingReusedOnlyWithinCompatibleRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{1, 0, 0}}})
	}))
	defer server.Close()
	p, err := NewEndpoint(server.URL+"/api/embed", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithQueryCache(t.Context())
	for range 2 {
		if _, err = p.EmbedModel(ctx, "model", 3, []string{"same query"}, false); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("identical query embedded %d times", calls.Load())
	}
	for _, c := range []struct {
		model      string
		background bool
	}{{"other-model", false}, {"model", true}} {
		if _, err = p.EmbedModel(ctx, c.model, 3, []string{"same query"}, c.background); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = p.EmbedModel(WithQueryCache(t.Context()), "model", 3, []string{"same query"}, false); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 {
		t.Fatalf("cross-model, document or cross-request reuse: %d", calls.Load())
	}
}
