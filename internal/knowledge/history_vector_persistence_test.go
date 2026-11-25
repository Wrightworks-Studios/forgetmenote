package knowledge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHistoricalVectorPersistenceRecovery(t *testing.T) {
	for _, recovery := range []string{"worker", "history-submission", "current-submission"} {
		t.Run(recovery, func(t *testing.T) {
			f := evidenceNotebook(t)
			ctx := context.Background()
			config := testConfig()
			config.Endpoint = true
			if _, err := f.engine.Configure(ctx, config, false); err != nil {
				t.Fatal(err)
			}
			fillIndex(t, f.engine, "")
			initial, err := f.engine.PendingScope(ctx, 32, "history")
			if err != nil || len(initial.Passages) != 1 || initial.Status.History.CompletedRevision != initial.Status.History.Revision {
				t.Fatalf("expected one historical vector after completed scan: %+v %v", initial, err)
			}
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
				_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
			}))
			defer endpoint.Close()
			provider, err := NewEndpoint(endpoint.URL+"/api/embed", "")
			if err != nil {
				t.Fatal(err)
			}
			restore := failVectors(t, f.engine)
			if err = f.engine.Start(provider); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				pending, err := f.engine.PendingScope(ctx, 32, "history")
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(pending.Status.History.Message, "injected write failure") {
					if len(pending.Passages) != 1 {
						t.Fatal("failed write published its vector")
					}
					if pending.Status.State != "ready" {
						t.Fatalf("historical write failure disabled the complete current index: %+v", pending.Status)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("worker cache-write failure not reported: %+v", pending.Status.History)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if recovery != "worker" {
				if err = f.engine.Start(nil); err != nil {
					t.Fatal(err)
				}
			}
			restore()
			if recovery == "worker" {
				if err = f.engine.Start(provider); err != nil {
					t.Fatal(err)
				}
			} else {
				scope := "history"
				if recovery == "current-submission" {
					scope = "current"
					note, err := f.store.Read(ctx, "private/hidden.md", "")
					if err != nil {
						t.Fatal(err)
					}
					if _, err = f.store.Save(ctx, note.Path, []byte("# Evidence\nprivate/hidden.md\n"), note.Revision, ""); err != nil {
						t.Fatal(err)
					}
				}
				fillIndex(t, f.engine, scope)
			}
			deadline = time.Now().Add(5 * time.Second)
			for {
				pending, err := f.engine.PendingScope(ctx, 32, "history")
				if err != nil {
					t.Fatal(err)
				}
				if len(pending.Passages) == 0 {
					if _, ok := storedVectors(t, f.engine)[initial.Passages[0].Hash]; !ok {
						t.Fatal("recovered vector was not persisted")
					}
					if h := pending.Status.History; h.State != "ready" || h.Message != "" || h.Indexed != 3 || h.Total != 3 {
						t.Fatalf("persisted all vectors but kept obsolete write error: %+v", h)
					}
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("historical vector did not recover: %+v", pending.Status.History)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
