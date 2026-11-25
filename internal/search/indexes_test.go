package search

import (
	"encoding/json"
	"forgetmenote/internal/pg/pgtest"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
)

func TestIdleNotebookEnginesDoNotAccumulateAcrossRequests(t *testing.T) {
	t.Setenv("FMN_LIMIT_IDLE_ENGINES", "1")
	db := pgtest.Org(t)
	projects, err := catalog.Open(t.TempDir(), nil, db)
	if err != nil {
		t.Fatal(err)
	}
	defer projects.Close()
	if err := projects.CreateKind(catalog.Bootstrap, "second", "Second", "notes"); err != nil {
		t.Fatal(err)
	}
	indexes := NewIndexes(db, projects, nil)
	defer indexes.Close()
	stores := []*workspace.Store{}
	for _, id := range []string{"default", "second"} {
		store, err := projects.Access(catalog.Bootstrap, id, "read")
		if err != nil {
			t.Fatal(err)
		}
		stores = append(stores, store)
	}
	first, release, err := indexes.Acquire(t.Context(), stores[0])
	if err != nil {
		t.Fatal(err)
	}
	release()
	_, release, err = indexes.Acquire(t.Context(), stores[1])
	if err != nil {
		t.Fatal(err)
	}
	release()
	again, release, err := indexes.Acquire(t.Context(), stores[0])
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if again == first {
		t.Fatal("all visited notebook engines remain resident despite the idle budget")
	}
}

func TestConfiguredIndexesWaitForExplicitStart(t *testing.T) {
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
	if _, err = store.Save(t.Context(), "note.md", []byte("# Index this note\nAn observation.\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	initial, err := knowledge.Open(db, store)
	if err != nil {
		t.Fatal(err)
	}
	_, err = initial.Configure(t.Context(), knowledge.Config{Enabled: true, Endpoint: true, Model: "synthetic", Dimensions: 3, PassageRunes: 256, Splitter: "markdown-heading-v1"}, false)
	initial.Close()
	if err != nil {
		t.Fatal(err)
	}
	called := make(chan struct{}, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- struct{}{}
		var input struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		vectors := make([][]float32, len(input.Input))
		for i := range vectors {
			vectors[i] = []float32{1, 0, 0}
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	}))
	defer server.Close()
	endpoint, err := knowledge.NewEndpoint(server.URL+"/api/embed", "")
	if err != nil {
		t.Fatal(err)
	}
	indexes := NewIndexes(db, projects, func(e *knowledge.Endpoint) *knowledge.Endpoint { return e })
	defer indexes.Close()
	if err = indexes.Configure(endpoint); err != nil {
		t.Fatal(err)
	}
	if err = indexes.Register(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
		t.Fatal("configured index processed notebook content before Start")
	case <-time.After(2 * time.Second):
	}
	if err = indexes.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not activate configured index")
	}
}
