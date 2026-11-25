package search

import (
	"context"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/pg/pgtest"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
)

func retainedIndexes(t *testing.T, count int) (*Indexes, []*workspace.Store) {
	t.Helper()
	db := pgtest.Org(t)
	projects, err := catalog.Open(t.TempDir(), nil, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(projects.Close)
	stores := []*workspace.Store{}
	for i := 0; i < count; i++ {
		id := "default"
		if i > 0 {
			id = fmt.Sprintf("book%d", i)
			if err = projects.CreateKind(catalog.Bootstrap, id, id, "notes"); err != nil {
				t.Fatal(err)
			}
		}
		store, err := projects.Access(catalog.Bootstrap, id, "edit")
		if err != nil {
			t.Fatal(err)
		}
		stores = append(stores, store)
	}
	indexes := NewIndexes(db, projects, func(endpoint *knowledge.Endpoint) *knowledge.Endpoint { return endpoint })
	t.Cleanup(indexes.Close)
	return indexes, stores
}
func TestActiveAndNestedIndexLeasesSurviveIdleEviction(t *testing.T) {
	t.Setenv("FMN_LIMIT_IDLE_ENGINES", "1")
	indexes, stores := retainedIndexes(t, 3)
	first, releaseFirst, err := indexes.Acquire(t.Context(), stores[0])
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()
	for _, store := range stores[1:] {
		_, release, err := indexes.Acquire(t.Context(), store)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	again, releaseAgain, err := indexes.Acquire(t.Context(), stores[0])
	if err != nil {
		t.Fatal(err)
	}
	defer releaseAgain()
	if first != again {
		t.Fatal("idle eviction replaced an actively leased engine")
	}
	// A nested multi-notebook retrieval must not wait for the outer lease to end.
	nested, releaseNested, err := indexes.Acquire(t.Context(), stores[1])
	if err != nil {
		t.Fatal(err)
	}
	if nested == nil {
		t.Fatal("nested lease did not acquire an engine")
	}
	releaseNested()
	releaseNested()
}
func TestIdleByteBudgetEvictsOversizedSingleEngine(t *testing.T) {
	t.Setenv("FMN_LIMIT_IDLE_INDEX_BYTES", "65536")
	indexes, stores := retainedIndexes(t, 1)
	if _, err := stores[0].Save(t.Context(), "large.md", []byte("# Large\n"+strings.Repeat("Notebook evidence. ", 4000)), "", ""); err != nil {
		t.Fatal(err)
	}
	engine, release, err := indexes.Acquire(t.Context(), stores[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Configure(t.Context(), knowledge.Config{Enabled: true, Model: "synthetic", Dimensions: 3, PassageRunes: 1400, Splitter: "markdown-heading-v1"}, false); err != nil {
		release()
		t.Fatal(err)
	}
	release()
	reopened, release, err := indexes.Acquire(t.Context(), stores[0])
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if reopened == engine {
		t.Fatal("oversized idle engine bypassed the byte budget")
	}
}
func TestIndexesCloseDrainsActiveLeasesAndRejectsNewRequests(t *testing.T) {
	indexes, stores := retainedIndexes(t, 1)
	_, release, err := indexes.Acquire(t.Context(), stores[0])
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	done := make(chan struct{})
	go func() { indexes.Close(); close(done) }()
	until := time.Now().Add(time.Second)
	for {
		_, closeLease, err := indexes.Acquire(t.Context(), stores[0])
		if err != nil {
			break
		}
		closeLease()
		if time.Now().After(until) {
			t.Fatal("shutdown did not reject new leases")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("shutdown returned before the active lease drained")
	default:
	}
	release()
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after release")
	}
}
func TestColdNotebookProcessingIsFairAndPreservesVectorsAcrossEviction(t *testing.T) {
	t.Setenv("FMN_LIMIT_IDLE_ENGINES", "1")
	indexes, stores := retainedIndexes(t, 3)
	config := knowledge.Config{Enabled: true, Endpoint: true, Model: "synthetic", Dimensions: 3, PassageRunes: 256, Splitter: "markdown-heading-v1"}
	for i, store := range stores {
		var text strings.Builder
		for section := 0; section < 80; section++ {
			fmt.Fprintf(&text, "# Book%d section%d\nUnique book%d evidence%d.\n", i, section, i, section)
		}
		if _, err := store.Save(t.Context(), "document.md", []byte(text.String()), "", ""); err != nil {
			t.Fatal(err)
		}
		engine, err := knowledge.Open(indexes.db, store)
		if err != nil {
			t.Fatal(err)
		}
		_, err = engine.Configure(t.Context(), config, false)
		engine.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err = indexes.Register(t.Context(), store); err != nil {
			t.Fatal(err)
		}
	}
	events := make(chan int, 100)
	var mu sync.Mutex
	seen := map[string]int{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		book := -1
		vectors := make([][]float32, len(input.Input))
		mu.Lock()
		for i, text := range input.Input {
			seen[text]++
			for n := 0; n < 3; n++ {
				if strings.Contains(text, fmt.Sprintf("book%d evidence", n)) {
					book = n
				}
			}
			vectors[i] = []float32{1, 0, 0}
		}
		mu.Unlock()
		events <- book
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
	if err = indexes.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for expected := 0; expected < 3; expected++ {
		select {
		case actual := <-events:
			if actual != expected {
				t.Fatalf("background batch did not yield between cold notebooks: %d wanted%d", actual, expected)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	for {
		ready := true
		for _, store := range stores {
			engine, release, err := indexes.Acquire(ctx, store)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := engine.Pending(ctx, 1)
			release()
			if err != nil {
				t.Fatal(err)
			}
			ready = ready && pending.Status.State == "ready"
		}
		if ready {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("cold processing did not complete", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 240 {
		t.Fatalf("cold notebooks lost evidence: %d unique passages", len(seen))
	}
	for _, count := range seen {
		if count != 1 {
			t.Fatalf("eviction repeated paid inference: %d calls for the same passage", count)
		}
	}
}
