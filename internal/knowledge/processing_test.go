package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestProcessingReopenKeepsHistoryRequestedBeforeCheckpoint(t *testing.T) {
	engine, _ := indexFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := engine.PendingScope(ctx, 1, "history"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected interrupted history request: %v", err)
	}
	if _, err := os.Stat(filepath.Join(engine.dir, "cache", "history.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture unexpectedly persisted history: %v", err)
	}
	processing := engine.Processing
	engine.Close()
	reopened, err := OpenWithProcessing(database(t, engine.Store), engine.Store, processing)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for i := 0; i < 10 && reopened.ProcessBatch(t.Context(), i == 0); i++ {
	}
	pending, err := reopened.Pending(t.Context(), 1)
	if err != nil || pending.Status.History == nil || pending.Status.History.State != "ready" || pending.Status.History.Versions != 1 {
		t.Fatalf("reopening lost requested historical processing: %+v %v", pending.Status.History, err)
	}
}

func TestProcessingReopenKeepsLearnedBatchCapacity(t *testing.T) {
	engine, doc := indexFixture(t)
	var requests []int
	var requestsMu sync.Mutex
	snapshot := func() []int { requestsMu.Lock(); defer requestsMu.Unlock(); return slices.Clone(requests) }
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		requestsMu.Lock()
		requests = append(requests, len(input.Input))
		requestsMu.Unlock()
		if len(input.Input) > 2 {
			http.Error(w, "synthetic batch bound", http.StatusRequestTimeout)
			return
		}
		vectors := make([][]float32, len(input.Input))
		for i := range vectors {
			vectors[i] = []float32{1, 0, 0}
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	}))
	defer provider.Close()
	endpoint, err := NewEndpoint(provider.URL+"/api/embed", "")
	if err != nil {
		t.Fatal(err)
	}
	write := func(label string) {
		t.Helper()
		var text strings.Builder
		for i := 0; i < 40; i++ {
			fmt.Fprintf(&text, "## %s section %d\n%s evidence %d.\n", label, i, label, i)
		}
		doc, err = engine.Store.Save(t.Context(), doc.Path, []byte(text.String()), doc.Revision, "")
		if err != nil {
			t.Fatal(err)
		}
	}
	write("Original")
	config := testConfig()
	config.Endpoint = true
	if _, err := engine.Configure(t.Context(), config, false); err != nil {
		t.Fatal(err)
	}
	if err := engine.SetEndpoint(endpoint); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100 && engine.ProcessBatch(t.Context(), i == 0); i++ {
	}
	pending, err := engine.Pending(t.Context(), 1)
	if err != nil || pending.Status.State != "ready" {
		t.Fatalf("initial worker did not complete: %+v %v", pending.Status, err)
	}
	learned := snapshot()
	if len(learned) < 2 || learned[0] <= 2 {
		t.Fatalf("fixture did not learn batch capacity: %v", learned)
	}
	write("Replacement")
	boundary := len(learned)
	processing := engine.Processing
	engine.Close()
	reopened, err := OpenWithProcessing(database(t, engine.Store), engine.Store, processing)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.SetEndpoint(endpoint); err != nil {
		t.Fatal(err)
	}
	if !reopened.ProcessBatch(t.Context(), true) {
		t.Fatal("reopened notebook did not continue pending work")
	}
	after := snapshot()
	if len(after) != boundary+1 || after[boundary] > 2 {
		t.Fatalf("reopen forgot learned endpoint capacity or drained multiple batches: %v", after[boundary:])
	}
}
