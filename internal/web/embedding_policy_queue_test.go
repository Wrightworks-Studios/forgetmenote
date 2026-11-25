package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/chat"
)

func TestQueuedEmbeddingRechecksCurrentLocalOnlyPolicy(t *testing.T) {
	app, _ := fixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(entered)
			<-release
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{1, 0, 0}}})
	}))
	defer server.Close()
	var releaseOnce sync.Once
	releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseRequest()
	if err := app.ConfigureEmbeddings(server.URL+"/api/embed", ""); err != nil {
		t.Fatal(err)
	}
	endpoint := app.indexes.Endpoint()
	authorize := endpoint.Authorize
	var checks atomic.Int32
	queued := make(chan struct{})
	endpoint.Authorize = func(model string, dimensions int) error {
		err := authorize(model, dimensions)
		if checks.Add(1) == 3 {
			close(queued)
		}
		return err
	}
	done := make(chan error, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	call := func() {
		_, err := endpoint.EmbedModel(ctx, "synthetic", 3, []string{"Synthetic notebook evidence"}, false)
		done <- err
	}
	go call()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("embedding did not start: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go call()
	select {
	case <-queued:
	case err := <-done:
		t.Fatalf("second embedding was not queued: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	config := chat.Config{LocalOnly: true, Connections: []chat.Connection{{ID: "local", Label: "Synthetic local", Format: "chat-completions", BaseURL: server.URL, Local: true, Models: []string{"synthetic"}}}}
	file := filepath.Join(t.TempDir(), "chat.json")
	raw, _ := json.Marshal(config)
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureChat(file); err != nil {
		t.Fatal(err)
	}
	releaseRequest()
	first, second := <-done, <-done
	if requests.Load() != 1 || second == nil || first == nil {
		t.Fatalf("queued or disclosed embedding ignored current privacy: requests=%d first=%v second=%v", requests.Load(), first, second)
	}
}
