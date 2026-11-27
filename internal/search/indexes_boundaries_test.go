package search

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"

	"github.com/jackc/pgx/v5"
)

func TestIndexesCanRetryFailedStart(t *testing.T) {
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
	if _, err = store.Save(t.Context(), "one.md", []byte("# Index me\nA synthetic observation.\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	configured, err := knowledge.Open(db, store)
	if err != nil {
		t.Fatal(err)
	}
	_, err = configured.Configure(t.Context(), knowledge.Config{Enabled: true, Endpoint: true, Model: "synthetic", Dimensions: 3, PassageRunes: 256, Splitter: "markdown-heading-v1"}, false)
	configured.Close()
	if err != nil {
		t.Fatal(err)
	}
	called := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Input []string `json:"input"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			t.Error("invalid fixture request")
		}
		select {
		case called <- struct{}{}:
		default:
		}
		vectors := make([][]float32, len(in.Input))
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
	indexes := NewIndexes(db, projects, func(p *knowledge.Endpoint) *knowledge.Endpoint { return p })
	defer indexes.Close()
	if err = indexes.Configure(endpoint); err != nil {
		t.Fatal(err)
	}
	if err = indexes.Register(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	var cancels []func()
	for i := 0; i < 128; i++ {
		_, cancel, err := store.Watch()
		if err != nil {
			t.Fatal(err)
		}
		cancels = append(cancels, cancel)
	}
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	if err = indexes.Start(t.Context()); err == nil {
		t.Fatal("fixture did not reject startup with the real watch pool full")
	}
	cancels[0]()
	if err = indexes.Start(t.Context()); err != nil {
		t.Fatal("repaired startup did not retry", err)
	}
	select {
	case <-called:
	case <-time.After(3 * time.Second):
		t.Fatal("Start reported success after retry but never created the background processor")
	}
}

// Policy synchronization reads the notebook's content through the request's
// context: a canceled Acquire returns while that read still waits for the
// database.
func TestCanceledAcquireStopsPolicySynchronization(t *testing.T) {
	db := pgtest.Org(t)
	data := t.TempDir()
	projects, err := catalog.Open(data, nil, db)
	if err != nil {
		t.Fatal(err)
	}
	if err = projects.Create(catalog.Bootstrap, "alpha", "Alpha"); err != nil {
		t.Fatal(err)
	}
	store, err := projects.Access(catalog.Bootstrap, "alpha", "edit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(t.Context(), "one.md", []byte("# Synthetic note\nSynthetic private content.\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = projects.SetAISettings(catalog.Bootstrap, catalog.AISettings{Embeddings: true, Model: "synthetic", Dimensions: 3}); err != nil {
		t.Fatal(err)
	}
	projects.Close()
	// A gateway that has not read the note yet must fetch its content.
	if projects, err = catalog.Open(data, nil, db); err != nil {
		t.Fatal(err)
	}
	defer projects.Close()
	if store, err = projects.Access(catalog.Bootstrap, "alpha", "read"); err != nil {
		t.Fatal(err)
	}
	indexes := NewIndexes(db, projects, nil)
	defer indexes.Close()
	waiting, release := holdTableReads(t, db, "file_versions")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, release, err := indexes.Acquire(ctx, store)
		if release != nil {
			release()
		}
		done <- err
	}()
	for deadline := time.Now().Add(3 * time.Second); waiting(0) == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("policy synchronization did not read the notebook")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("canceled acquire did not report its cancellation:", err)
		}
	case <-time.After(time.Second):
		release()
		<-done
		t.Fatal("Acquire discarded its context during policy synchronization")
	}
}

// holdTableReads makes every read of table wait, as a stalled database
// would, until release runs or the test ends; waiting is pgtest.WaitingFor
// this hold. It locks the table for every org, so it suits only tests that do
// not run in parallel.
func holdTableReads(t *testing.T, db *pg.DB, table string) (waiting func(n int) int, release func()) {
	t.Helper()
	ctx := context.Background()
	hold, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { hold.Rollback(ctx) }) }
	t.Cleanup(release)
	if _, err = hold.Exec(ctx, "LOCK TABLE "+pgx.Identifier{table}.Sanitize()+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	return pgtest.WaitingFor(t, db, pgtest.Backend(t, hold)), release
}
