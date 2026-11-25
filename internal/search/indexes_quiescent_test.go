package search

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/knowledge"
)

func TestRefreshDoesNotReopenUnchangedColdReadyNotebook(t *testing.T) {
	t.Setenv("FMN_LIMIT_IDLE_ENGINES", "1")
	indexes, stores := retainedIndexes(t, 3)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		vectors := make([][]float32, len(body.Input))
		for i := range vectors {
			vectors[i] = []float32{1, 0, 0}
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	}))
	defer provider.Close()
	endpoint, err := knowledge.NewEndpoint(provider.URL+"/api/embed", "")
	if err != nil {
		t.Fatal(err)
	}
	for i, store := range stores[:2] {
		if _, err = store.Save(t.Context(), "note.md", []byte(fmt.Sprintf("# Note%d\nReady content%d.\n", i, i)), "", ""); err != nil {
			t.Fatal(err)
		}
		engine, release, err := indexes.Acquire(t.Context(), store)
		if err != nil {
			t.Fatal(err)
		}
		_, err = engine.Configure(t.Context(), knowledge.Config{Enabled: true, Endpoint: true, Model: "synthetic", Dimensions: 3, PassageRunes: 256, Splitter: "markdown-heading-v1"}, false)
		release()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = indexes.Configure(endpoint); err != nil {
		t.Fatal(err)
	}
	if err = indexes.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(5 * time.Second)
	for {
		ready := true
		indexes.mu.Lock()
		for _, store := range stores[:2] {
			slot := indexes.slots[store]
			ready = ready && slot.checkpoint.Load() != nil && slot.references == 0 && !slot.pending.Load() && slot.transition == nil
		}
		indexes.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(until) {
			t.Fatal("background indexing did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Observe from another request after all queued progress has drained.
	engine, release, err := indexes.Acquire(t.Context(), stores[1])
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, evict, err := indexes.Acquire(t.Context(), stores[2])
	if err != nil {
		t.Fatal(err)
	}
	evict()
	indexes.mu.Lock()
	cold := indexes.slots[stores[0]].engine == nil
	indexes.mu.Unlock()
	if !cold {
		t.Fatal("fixture did not evict the first notebook")
	}
	indexes.mu.Lock()
	coldSlot := indexes.slots[stores[0]]
	indexes.mu.Unlock()
	if !coldSlot.background.Load() {
		t.Fatal("refresh does not probe the cold notebook")
	}
	indexes.refreshPending()
	indexes.signal()
	until = time.Now().Add(5 * time.Second)
	for coldSlot.pending.Load() {
		if time.Now().After(until) {
			t.Fatal("refresh did not probe the cold notebook")
		}
		time.Sleep(time.Millisecond)
	}
	// The processor took the probe; reloading the corpus would reopen the
	// notebook's engine.
	time.Sleep(100 * time.Millisecond)
	indexes.mu.Lock()
	periodicReopen := coldSlot.engine != nil
	indexes.mu.Unlock()
	if periodicReopen {
		t.Fatal("periodic refresh reopened an unchanged cold ready notebook")
	}
	reopened, releaseCold, err := indexes.Acquire(context.Background(), stores[0])
	if err != nil {
		t.Fatal(err)
	}
	defer releaseCold()
	pending, err := reopened.Pending(t.Context(), 1)
	if err != nil || pending.Status.State != "ready" {
		t.Fatalf("cold index lost its coverage: %+v %v", pending.Status, err)
	}
	releaseCold()
	before := calls.Load()
	note, err := stores[0].Read(t.Context(), "note.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stores[0].Save(t.Context(), note.Path, []byte("# Changed\nNew committed evidence."), note.Revision, ""); err != nil {
		t.Fatal(err)
	}
	until = time.Now().Add(5 * time.Second)
	for calls.Load() == before {
		if time.Now().After(until) {
			t.Fatal("commit hint did not invalidate the cold checkpoint")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = engine
}
