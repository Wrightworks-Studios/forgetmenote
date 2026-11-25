package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"forgetmenote/internal/webcore"
)

// A stopping server ends at once the requests that wait for changes, such
// as an open page's live poll, whose client reconnects to the next process,
// while every other request still running, such as an import or an upload,
// keeps the whole drain and answers.
func TestServeEndsLongPollsAtOnceAndDrainsOtherRequests(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	polling, saving, polled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server := &http.Server{Addr: address, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/poll":
			defer close(polled)
			waiting, stop := webcore.UntilDrain(r.Context())
			defer stop()
			close(polling)
			<-waiting.Done()
			if webcore.Drained(waiting) {
				w.Write([]byte("drained"))
			}
		case "/save":
			close(saving)
			// It runs on after the long poll ended, as a slow import would.
			<-polled
			time.Sleep(200 * time.Millisecond)
			if r.Context().Err() != nil {
				w.Write([]byte("ended"))
				return
			}
			w.Write([]byte("saved"))
		}
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, server, 10*time.Second) }()
	type answer struct {
		body string
		at   time.Time
	}
	get := func(path string, out chan<- answer) {
		for range 100 {
			response, err := http.Get("http://" + address + path)
			if err == nil {
				raw, _ := io.ReadAll(response.Body)
				response.Body.Close()
				out <- answer{string(raw), time.Now()}
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		out <- answer{"unreachable", time.Now()}
	}
	poll, save := make(chan answer, 1), make(chan answer, 1)
	go get("/poll", poll)
	<-polling
	go get("/save", save)
	<-saving
	stopped := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stopping server is still waiting")
	}
	if got := <-poll; got.body != "drained" || got.at.Sub(stopped) > time.Second {
		t.Errorf("the long poll answered %q %v after the shutdown began", got.body, got.at.Sub(stopped))
	}
	if got := <-save; got.body != "saved" {
		t.Errorf("a request still running when the long poll ended answered %q", got.body)
	}
}
