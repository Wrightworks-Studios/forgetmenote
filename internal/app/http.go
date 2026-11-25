package app

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"forgetmenote/internal/webcore"
)

// Serve owns request lifetime; callers may close stores only after it returns.
func Serve(ctx context.Context, server *http.Server, timeout time.Duration) error {
	return ServeWithTLS(ctx, server, timeout, "", "")
}

// ServeWithTLS serves until ctx ends, then shuts down: it accepts no more
// connections and lets the requests it serves finish, for up to timeout,
// then closes the connections left and waits for their handlers. A request
// that waits for changes, such as an open page's live poll or chat events
// or an MCP subscription stream, watches the drain that begins as the
// shutdown closes the listeners (webcore.UntilDrain) and ends at once, so it
// holds the shutdown for none of that time; its client reconnects to the
// next process. Every other request keeps the whole timeout.
func ServeWithTLS(ctx context.Context, server *http.Server, timeout time.Duration, cert, key string) error {
	var handlers sync.WaitGroup
	handler := server.Handler
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		handler.ServeHTTP(w, r)
	})
	drain, beginDrain := context.WithCancel(context.Background())
	defer beginDrain()
	server.BaseContext = func(net.Listener) context.Context { return webcore.WithDrain(context.Background(), drain) }
	// Shutdown runs it once the listeners are closed and keep-alives end,
	// so a client that polls again reaches the next process, not this one.
	server.RegisterOnShutdown(beginDrain)
	served := make(chan error, 1)
	go func() {
		if cert != "" {
			served <- server.ListenAndServeTLS(cert, key)
		} else {
			served <- server.ListenAndServe()
		}
	}()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := server.Shutdown(shutdown)
	if err != nil {
		server.Close()
	}
	<-served
	handlers.Wait()
	return err
}
