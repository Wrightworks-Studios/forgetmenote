package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestDefaultConnectionsForwardConcurrentCallsWithoutAQueue(t *testing.T) {
	const count = 32
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	entered := make(chan struct{}, count)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Write([]byte(`{"value":7}`))
	}))
	defer upstream.Close()
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	manager, err := New(Config{Version: 1, Connections: []Connection{{ID: "data", Kind: "http", Endpoint: upstream.URL, Operations: []Operation{{Name: "read", ReadOnly: true, Method: "GET", Path: "/"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	done := make(chan error, count)
	for i := 0; i < count; i++ {
		go func() { _, err := manager.Call(ctx, "data", "read", json.RawMessage(`{}`), ""); done <- err }()
	}
	for i := 0; i < count; i++ {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatalf("only %d of %d calls reached upstream while other calls were active", i, count)
		}
	}
	// Discovery must also remain independent of those long-running calls.
	if _, err := manager.Tools(ctx, "data"); err != nil {
		t.Fatal("discovery waited behind data calls", err)
	}
	// Release only after every request has reached the adapter.
	finish()
	for i := 0; i < count; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	if _, err := manager.Tools(ctx, "data"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled discovery: %v", err)
	}
}
