package search

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecoveryTickIsNotStarvedByContinuousBatches(t *testing.T) {
	db := pgtest.Org(t)
	projects, err := catalog.Open(t.TempDir(), nil, db)
	if err != nil {
		t.Fatal(err)
	}
	defer projects.Close()
	if err = projects.Create(catalog.Bootstrap, "busy", "Busy"); err != nil {
		t.Fatal(err)
	}
	var recoveredCalls atomic.Int64
	retry := make(chan struct{}, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			return
		}
		if len(in.Input) == 0 {
			t.Error("empty input")
			return
		}
		if strings.Contains(in.Input[0], "Retry notebook") {
			if recoveredCalls.Add(1) == 1 {
				http.Error(w, "temporary failure", 503)
				return
			}
			select {
			case retry <- struct{}{}:
			default:
			}
		} else {
			select {
			case <-time.After(400 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		vectors := make([][]float32, len(in.Input))
		for i := range vectors {
			vectors[i] = []float32{1, 0, 0}
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	}))
	indexes := NewIndexes(db, projects, func(e *knowledge.Endpoint) *knowledge.Endpoint { return e })
	// The busy notebook's batches outlast this wait many times over, so only
	// the recovery timer can bring the failed notebook back.
	indexes.recovery = time.Second
	defer func() { indexes.Close(); provider.Close() }()
	var stores []*workspace.Store
	for _, id := range []string{"default", "busy"} {
		store, err := projects.Access(catalog.Bootstrap, id, "edit")
		if err != nil {
			t.Fatal(err)
		}
		text := "# Retry notebook\nTemporary provider recovery.\n"
		if id == "busy" {
			var content strings.Builder
			for i := 0; i < 3500; i++ {
				fmt.Fprintf(&content, "# Batch section %d\nDistinct processing evidence %d.\n", i, i)
			}
			text = content.String()
		}
		if _, err = store.Save(t.Context(), "evidence.md", []byte(text), "", ""); err != nil {
			t.Fatal(err)
		}
		engine, err := knowledge.Open(db, store)
		if err != nil {
			t.Fatal(err)
		}
		_, err = engine.Configure(t.Context(), knowledge.Config{Enabled: true, Endpoint: true, Model: "synthetic", Dimensions: 3, PassageRunes: 256, Splitter: "markdown-heading-v1"}, false)
		engine.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err = indexes.Register(t.Context(), store); err != nil {
			t.Fatal(err)
		}
		stores = append(stores, store)
	}
	endpoint, err := knowledge.NewEndpoint(provider.URL+"/api/embed", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = indexes.Configure(endpoint); err != nil {
		t.Fatal(err)
	}
	if err = indexes.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-retry:
	case <-time.After(15 * time.Second):
		t.Fatalf("provider recovery was not retried on the recovery tick while another notebook had pending batches: calls=%d", recoveredCalls.Load())
	}
}
