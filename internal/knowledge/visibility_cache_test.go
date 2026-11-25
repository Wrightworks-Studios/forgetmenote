package knowledge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/workspace"
)

// An evidenceFixture is a notebook holding public.md and two versions of
// private/hidden.md, three historical versions in all, and an engine on it.
type evidenceFixture struct {
	store  *workspace.Store
	engine *Engine
	dir    string
}

func evidenceNotebook(t *testing.T) *evidenceFixture {
	t.Helper()
	f := &evidenceFixture{dir: t.TempDir()}
	f.store = openNotebook(t, f.dir, nil)
	for _, path := range []string{"public.md", "private/hidden.md"} {
		if _, err := f.store.Save(context.Background(), path, []byte("# Evidence\n"+path+"\n"), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	old, err := f.store.Read(context.Background(), "private/hidden.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.Save(context.Background(), old.Path, []byte("# Current hidden evidence\n"), old.Revision, ""); err != nil {
		t.Fatal(err)
	}
	f.engine, err = Open(database(t, f.store), f.store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.engine.Close)
	return f
}

// restart closes the engine and the notebook's store and opens both again
// with excluded hidden, as a gateway restarted with changed exclusions does:
// a notebook's exclusions are fixed while its store is open.
func (f *evidenceFixture) restart(t *testing.T, excluded []string) {
	t.Helper()
	head, err := f.store.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.engine.Close()
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store = openNotebook(t, f.dir, excluded)
	f.engine, err = Open(database(t, f.store), f.store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.engine.Close)
	after, err := f.store.Status(context.Background())
	if err != nil || after.Revision != head.Revision {
		t.Fatal("restart changed the notebook's head", after, err)
	}
}

func TestRestartWithExclusionsInvalidatesCurrentSearchAndPending(t *testing.T) {
	for _, consumer := range []string{"semantic", "pending"} {
		t.Run(consumer, func(t *testing.T) {
			f := evidenceNotebook(t)
			ctx := context.Background()
			config := testConfig()
			if _, err := f.engine.Configure(ctx, config, false); err != nil {
				t.Fatal(err)
			}
			before, err := f.engine.PendingScope(ctx, 32, "current")
			if err != nil || before.Status.Total != 2 {
				t.Fatalf("prime current cache: %+v %v", before, err)
			}
			query := SearchQuery{Vector: []float32{1, 0, 0}, ConfigID: config.ID()}
			if consumer == "semantic" {
				fillIndex(t, f.engine, "")
				fillIndex(t, f.engine, "history")
				matches, err := f.engine.Search(ctx, query)
				if err != nil || len(matches) != 2 {
					t.Fatalf("prime semantic cache: %+v %v", matches, err)
				}
			}
			f.restart(t, []string{"private"})
			snapshot, err := f.store.Snapshot(ctx, "")
			if err != nil || len(snapshot.Files) != 1 {
				t.Fatalf("snapshot after the restart: %+v %v", snapshot, err)
			}
			if consumer == "semantic" {
				matches, err := f.engine.Search(ctx, query)
				if err != nil || len(matches) != 1 || matches[0].Path != "public.md" {
					t.Fatalf("excluded note survived current cache: %+v %v", matches, err)
				}
				history, err := f.engine.PendingScope(ctx, 32, "history")
				if err != nil || len(history.Passages) != 0 {
					t.Fatalf("visible historical vectors were lost: %+v %v", history, err)
				}
				f.restart(t, nil)
				matches, err = f.engine.Search(ctx, query)
				if err != nil || len(matches) != 2 {
					t.Fatalf("restored visibility did not reuse current vectors: %+v %v", matches, err)
				}
				history, err = f.engine.PendingScope(ctx, 32, "history")
				if err != nil || len(history.Passages) != 0 || history.Status.History.Total != 3 {
					t.Fatalf("historical vector reuse after visibility restoration: %+v %v", history, err)
				}
			} else {
				pending, err := f.engine.PendingScope(ctx, 32, "current")
				if err != nil || pending.Status.Total != 1 || len(pending.Passages) != 1 || pending.Passages[0].Path != "public.md" {
					t.Fatalf("excluded note survived pending cache: %+v %v", pending, err)
				}
			}
		})
	}
}

func TestHistoryVisibilityChangeRescansAndReusesVectors(t *testing.T) {
	f := evidenceNotebook(t)
	ctx := context.Background()
	f.restart(t, []string{"private"})
	if _, err := f.engine.Configure(ctx, testConfig(), false); err != nil {
		t.Fatal(err)
	}
	if got := fillIndex(t, f.engine, "history"); got != 1 {
		t.Fatalf("public historical vectors: %d", got)
	}
	for round, exclusions := range [][]string{nil, {"private"}, nil, {"private"}, nil} {
		f.restart(t, exclusions)
		status, err := f.engine.Pending(ctx, 32)
		if err != nil || status.Status.History == nil || status.Status.History.State != "scanning" || status.Status.History.CompletedRevision != "" {
			t.Fatalf("round %d status claimed coverage before visibility rescan: %+v %v", round, status.Status.History, err)
		}
		want := 3
		if len(exclusions) > 0 {
			want = 1
		}
		result, err := f.engine.HybridSearch(ctx, historyQuery("evidence"))
		if err != nil || result.History == nil || result.History.Versions != want || result.History.Total != want || len(result.Matches) != want || result.History.CompletedRevision != result.History.Revision {
			t.Fatalf("round %d visibility scan coverage: %+v %v", round, result, err)
		}
		pending, err := f.engine.PendingScope(ctx, 32, "history")
		wantPending := 0
		if round == 0 {
			wantPending = 2
		}
		if err != nil || len(pending.Passages) != wantPending || pending.Status.History == nil || pending.Status.History.Total != want || pending.Status.History.Indexed != want-wantPending {
			t.Fatalf("round %d pending coverage or reusable vectors: %+v %v", round, pending, err)
		}
		if got := fillIndex(t, f.engine, "history"); got != wantPending {
			t.Fatalf("round %d unnecessary historical embeddings: %d", round, got)
		}
		visibility, err := f.store.HistoryVisibilityID(ctx)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(f.engine.dir, "cache", "history.json"))
		var persisted historyCache
		if err != nil || json.Unmarshal(raw, &persisted) != nil || persisted.Visibility != visibility || persisted.Completed != result.History.Revision {
			t.Fatalf("round %d persisted history binding is stale: %+v %v", round, persisted, err)
		}
	}
}

func TestPendingWorkKeepsApplicableEmbeddingFailure(t *testing.T) {
	f := evidenceNotebook(t)
	config := testConfig()
	config.Endpoint = true
	if _, err := f.engine.Configure(context.Background(), config, false); err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "provider still offline", 503) }))
	defer endpoint.Close()
	provider, err := NewEndpoint(endpoint.URL+"/api/embed", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.engine.Start(provider); err != nil {
		t.Fatal(err)
	}
	waitWorkerFailure(t, f.engine, "embedding service")
	for i := 0; i < 3; i++ {
		pending, err := f.engine.Pending(context.Background(), 32)
		if err != nil || pending.Status.State != "unavailable" || len(pending.Passages) != 2 || !strings.Contains(pending.Status.Message, "embedding service") {
			t.Fatalf("successful notebook sync hid ongoing provider failure: %+v %v", pending, err)
		}
	}
}

func waitWorkerFailure(t *testing.T, e *Engine, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		e.mu.Lock()
		failure := e.failure + "\n" + e.syncFailure
		e.mu.Unlock()
		if strings.Contains(failure, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker failure %q not observed: %q", want, failure)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCompleteCacheSyncRetainsWriteFailureUntilPersistenceSucceeds(t *testing.T) {
	f := evidenceNotebook(t)
	ctx := context.Background()
	config := testConfig()
	config.Endpoint = true
	if _, err := f.engine.Configure(ctx, config, false); err != nil {
		t.Fatal(err)
	}
	fillIndex(t, f.engine, "")
	f.engine.Close()
	reopened, err := Open(database(t, f.store), f.store)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	// A copy shares every passage's content hash, so the cache stays complete
	// and only its new revision must be published before reporting a hit.
	public, err := f.store.Read(ctx, "public.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.Save(ctx, "copy.md", public.Content, "", ""); err != nil {
		t.Fatal(err)
	}
	restore := failVectors(t, reopened)
	provider, err := NewEndpoint("http://localhost:1/api/embed", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = reopened.Start(provider); err != nil {
		t.Fatal(err)
	}
	waitWorkerFailure(t, reopened, "injected write failure")
	if pending, err := reopened.Pending(ctx, 32); err == nil {
		t.Fatalf("failed persisted snapshot became a successful cache hit: %+v", pending)
	}
	restore()
	if pending, err := reopened.Pending(ctx, 32); err != nil || pending.Status.State != "ready" || len(pending.Passages) != 0 {
		t.Fatalf("repaired persistence did not recover: %+v %v", pending, err)
	}
}
