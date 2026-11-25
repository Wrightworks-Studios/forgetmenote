package search

import (
	"encoding/json"
	"forgetmenote/internal/pg/pgtest"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"forgetmenote/internal/knowledge"
)

func TestColdCheckpointInvalidatesAfterUnpublishedVectorFailure(t *testing.T) {
	t.Setenv("FMN_LIMIT_IDLE_ENGINES", "1")
	indexes, stores := retainedIndexes(t, 2)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		vectors := make([][]float32, len(request.Input))
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
	if err = indexes.Configure(endpoint); err != nil {
		t.Fatal(err)
	}
	if _, err = stores[0].Save(t.Context(), "a.md", []byte("# Ready\nA synthetic passage.\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	engine, release, err := indexes.Acquire(t.Context(), stores[0])
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err = engine.Configure(t.Context(), knowledge.Config{Enabled: true, Endpoint: true, Model: "synthetic", Dimensions: 3, PassageRunes: 256, Splitter: "markdown-heading-v1"}, false); err != nil {
		t.Fatal(err)
	}
	pending, err := engine.Pending(t.Context(), 1)
	if err != nil || len(pending.Passages) != 1 {
		t.Fatalf("fixture pending: %+v %v", pending, err)
	}
	passage := pending.Passages[0]
	if err = indexes.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		indexes.mu.Lock()
		slot := indexes.slots[stores[0]]
		ready := slot.checkpoint.Load() != nil && !slot.pending.Load() && slot.references == 1
		indexes.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background processor did not publish a ready checkpoint")
		}
		time.Sleep(time.Millisecond)
	}
	// Pause only the worker between operations, so the cache decision is observed
	// before a recovery batch can race the storage repair. All file I/O is real.
	indexes.mu.Lock()
	cancel, done := indexes.workerCancel, indexes.workerDone
	indexes.mu.Unlock()
	cancel()
	<-done
	if engine.BackgroundCheckpoint() == nil {
		t.Fatal("fixture is not completely ready")
	}
	restore := pgtest.FailTable(t, indexes.db, "knowledge_vector_sets")
	_, submitErr := engine.Submit(t.Context(), knowledge.Submission{Revision: pending.Status.Revision, ConfigID: pending.Status.ConfigID, Vectors: []knowledge.VectorSubmission{{ID: passage.ID, Hash: passage.Hash, NoteHash: passage.NoteHash, Vector: []float32{0, 1, 0}}}})
	restore()
	if submitErr == nil {
		t.Fatal("fixture did not reject publication while Postgres refused it")
	}
	if engine.BackgroundCheckpoint() != nil {
		t.Fatal("new checkpoint ignored recorded storage failure")
	}
	release()
	_, evict, err := indexes.Acquire(t.Context(), stores[1])
	if err != nil {
		t.Fatal(err)
	}
	evict()
	indexes.mu.Lock()
	slot := indexes.slots[stores[0]]
	cold := slot.engine == nil && slot.transition == nil
	indexes.mu.Unlock()
	if !cold {
		t.Fatal("fixture did not evict the first engine")
	}
	if indexes.coldUnchanged(t.Context(), slot) {
		t.Fatal("ready checkpoint suppressed a later unpublished vector persistence failure")
	}
}
