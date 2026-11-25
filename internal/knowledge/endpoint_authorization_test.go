package knowledge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func TestEndpointRevocationBlocksQueuedCallsAndDiscardsInflightResult(t *testing.T) {
	started, release, queued := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	var calls, checks atomic.Int32
	var disabled atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(started)
		<-release
		w.Write([]byte(`{"model":"test","embeddings":[[1,0,0]]}`))
	}))
	defer provider.Close()
	defer finish()
	e, err := NewEndpoint(provider.URL+"/api/embed", "")
	if err != nil {
		t.Fatal(err)
	}
	denied := errors.New("disabled by administrator")
	e.Authorize = func(string, int) error {
		if checks.Add(1) == 3 {
			close(queued)
		}
		if disabled.Load() {
			return denied
		}
		return nil
	}
	results := make(chan error, 2)
	call := func() {
		v, err := e.EmbedModel(context.Background(), "test", 3, []string{"synthetic"}, false)
		if v != nil {
			results <- errors.New("revoked result escaped")
			return
		}
		results <- err
	}
	go call()
	<-started
	go call()
	<-queued
	disabled.Store(true)
	finish()
	for range 2 {
		if err := <-results; !errors.Is(err, denied) {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("queued request reached provider")
	}
}
