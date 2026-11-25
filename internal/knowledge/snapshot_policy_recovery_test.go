package knowledge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEvidenceCacheTracksSnapshotPolicyAtUnchangedHEAD(t *testing.T) {
	f := evidenceNotebook(t)
	ctx := context.Background()
	note, err := f.store.Read(ctx, "private/hidden.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.Save(ctx, note.Path, []byte("[Public evidence](../public.md)\n"), note.Revision, ""); err != nil {
		t.Fatal(err)
	}
	for _, excluded := range [][]string{{"private"}, nil, {"private"}, nil} {
		f.restart(t, excluded)
		evidence, err := f.engine.Evidence(ctx, "public.md")
		want := 1
		if len(excluded) > 0 {
			want = 0
		}
		if err != nil || len(evidence.Backlinks) != want {
			t.Fatalf("snapshot policy %v did not invalidate same-HEAD backlinks: %+v %v", excluded, evidence, err)
		}
	}
}

func TestHistoricalEmbeddingFailureClearsAfterRecovery(t *testing.T) {
	for _, name := range []string{"worker", "history-submission", "current-submission"} {
		t.Run(name, func(t *testing.T) {
			f := evidenceNotebook(t)
			ctx := context.Background()
			config := testConfig()
			config.Endpoint = true
			if _, err := f.engine.Configure(ctx, config, false); err != nil {
				t.Fatal(err)
			}
			fillIndex(t, f.engine, "")
			if pending, err := f.engine.PendingScope(ctx, 32, "history"); err != nil || len(pending.Passages) != 1 {
				t.Fatalf("expected one historical embedding: %+v %v", pending, err)
			}
			var recovered atomic.Bool
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !recovered.Load() {
					http.Error(w, "synthetic provider offline", 503)
					return
				}
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
			if err = f.engine.Start(provider); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				pending, err := f.engine.PendingScope(ctx, 32, "history")
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(pending.Status.History.Message, "embedding service") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("provider failure not observed: %+v", pending.Status.History)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if name != "worker" {
				if err = f.engine.Start(nil); err != nil {
					t.Fatal(err)
				}
				if name == "current-submission" {
					note, err := f.store.Read(ctx, "private/hidden.md", "")
					if err != nil {
						t.Fatal(err)
					}
					if _, err = f.store.Save(ctx, note.Path, []byte("# Evidence\nprivate/hidden.md\n"), note.Revision, ""); err != nil {
						t.Fatal(err)
					}
					pending, err := f.engine.Pending(ctx, 32)
					if err != nil || len(pending.Passages) != 1 {
						t.Fatalf("current submission did not request the missing historical hash: %+v %v", pending, err)
					}
					p := pending.Passages[0]
					submission := Submission{Revision: pending.Status.Revision, ConfigID: pending.Status.ConfigID, Vectors: []VectorSubmission{{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: []float32{1, 0, 0}}}}
					restore := failVectors(t, f.engine)
					if _, err = f.engine.Submit(ctx, submission); err == nil {
						t.Fatal("current vectors unexpectedly persisted while Postgres refused writes")
					}
					restore()
					pending, err = f.engine.PendingScope(ctx, 32, "history")
					if err != nil || len(pending.Passages) != 1 || !strings.Contains(pending.Status.History.Message, "embedding service") {
						t.Fatalf("failed persistence cleared unresolved historical failure: %+v %v", pending, err)
					}
					if _, err = f.engine.Submit(ctx, submission); err != nil {
						t.Fatal(err)
					}
				} else {
					fillIndex(t, f.engine, "history")
				}
			} else {
				recovered.Store(true)
				if err = f.engine.Start(provider); err != nil {
					t.Fatal(err)
				}
			}
			deadline = time.Now().Add(3 * time.Second)
			for {
				pending, err := f.engine.PendingScope(ctx, 32, "history")
				if err != nil {
					t.Fatal(err)
				}
				if len(pending.Passages) == 0 {
					if pending.Status.History.State != "ready" || pending.Status.History.Indexed != 3 || pending.Status.History.Total != 3 || pending.Status.History.Message != "" {
						t.Fatalf("recovered historical vectors retained obsolete provider failure: %+v", pending.Status.History)
					}
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("historical vectors did not recover: %+v", pending.Status.History)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
