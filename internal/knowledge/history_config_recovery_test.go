package knowledge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHistoricalConfigChangeRecoversAfterCompatibleIndexing(t *testing.T) {
	f := evidenceNotebook(t)
	ctx := context.Background()
	config := testConfig()
	config.Endpoint = true
	if _, err := f.engine.Configure(ctx, config, false); err != nil {
		t.Fatal(err)
	}
	fillIndex(t, f.engine, "current")
	before, err := f.engine.PendingScope(ctx, 32, "history")
	if err != nil || len(before.Passages) != 1 {
		t.Fatalf("expected one historical passage: %+v %v", before, err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	replacementEntered, compatible := make(chan struct{}), make(chan struct{})
	originalModel := config.Model
	var once, replacementOnce, compatibleOnce sync.Once
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			return
		}
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		if in.Model != originalModel {
			replacementOnce.Do(func() { close(replacementEntered) })
			select {
			case <-compatible:
			case <-r.Context().Done():
				return
			}
		}
		vectors := make([][]float32, len(in.Input))
		for i := range vectors {
			vectors[i] = []float32{1, 0, 0}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": in.Model, "embeddings": vectors})
	}))
	defer endpoint.Close()
	defer once.Do(func() { close(release) })
	defer compatibleOnce.Do(func() { close(compatible) })
	provider, err := NewEndpoint(endpoint.URL+"/api/embed", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.engine.Start(provider); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("historical embedding did not start")
	}
	config.Model += "-replacement"
	if _, err = f.engine.Configure(ctx, config, true); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	select {
	case <-replacementEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement model embedding did not start")
	}
	pending, err := f.engine.PendingScope(ctx, 32, "history")
	if err != nil || pending.Status.History.State != "indexing" || pending.Status.History.Indexed != 0 || !strings.Contains(pending.Status.History.Message, "configuration changed") {
		t.Fatalf("stale work must remain transient pending actual compatible vectors: %+v %v", pending.Status.History, err)
	}
	compatibleOnce.Do(func() { close(compatible) })
	deadline := time.Now().Add(5 * time.Second)
	for {
		pending, err := f.engine.PendingScope(ctx, 32, "history")
		if err != nil {
			t.Fatal(err)
		}
		if pending.Status.History.Indexed == 3 && pending.Status.History.Total == 3 && len(pending.Passages) == 0 {
			for i := 0; i < 3; i++ {
				pending, err = f.engine.PendingScope(ctx, 32, "history")
				if err != nil {
					t.Fatal(err)
				}
				if pending.Status.ConfigID != config.ID() || pending.Status.History.State != "ready" || pending.Status.History.Message != "" || pending.Status.History.Revision != before.Status.History.Revision {
					t.Fatalf("compatible complete history retained stale submission error: %+v", pending.Status.History)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("replacement model did not finish: %+v", pending.Status.History)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
