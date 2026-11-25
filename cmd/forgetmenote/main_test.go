package main

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	runtimeapp "forgetmenote/internal/app"
)

func TestServeDrainsBeforeReturning(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	server := &http.Server{Addr: address, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; w.Write([]byte("saved")) })}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runtimeapp.Serve(ctx, server, time.Second) }()
	go func() {
		for i := 0; i < 100; i++ {
			response, err := http.Get("http://" + address)
			if err == nil {
				response.Body.Close()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler not started")
	}
	cancel()
	select {
	case err := <-done:
		close(release)
		t.Fatalf("returned before handler finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not drain")
	}
}
