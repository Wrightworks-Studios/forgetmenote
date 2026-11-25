package search

import (
	"encoding/json"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/pg/pgtest"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestActiveLeaseDoesNotSuppressBackgroundRecovery(t *testing.T) {
	db := pgtest.Org(t)
	projects, err := catalog.Open(t.TempDir(), nil, db)
	if err != nil {
		t.Fatal(err)
	}
	defer projects.Close()
	store, err := projects.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(t.Context(), "one.md", []byte("# Active lease\nRecover this notebook after a temporary failure.\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	retry := make(chan struct{}, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		if calls.Add(1) == 1 {
			http.Error(w, "temporary failure", 503)
			return
		}
		select {
		case retry <- struct{}{}:
		default:
		}
		vectors := make([][]float32, len(input.Input))
		for i := range vectors {
			vectors[i] = []float32{1, 0, 0}
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	}))
	indexes := NewIndexes(db, projects, func(e *knowledge.Endpoint) *knowledge.Endpoint { return e })
	// The first failed batch is retried only by the recovery timer.
	indexes.recovery = time.Second
	defer func() { indexes.Close(); provider.Close() }()
	endpoint, err := knowledge.NewEndpoint(provider.URL+"/api/embed", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = indexes.Configure(endpoint); err != nil {
		t.Fatal(err)
	}
	engine, release, err := indexes.Acquire(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err = engine.Configure(t.Context(), knowledge.Config{Enabled: true, Endpoint: true, Model: "synthetic", Dimensions: 3, PassageRunes: 1400, Splitter: "markdown-heading-v1"}, false); err != nil {
		t.Fatal(err)
	}
	if err = indexes.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-retry:
	case <-time.After(15 * time.Second):
		t.Fatalf("active lease suppressed background retry on the recovery timer: calls=%d", calls.Load())
	}
}
