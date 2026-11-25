package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/connectors"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPDataCallsDoNotWaitForBackgroundOrNotebookWork(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint("legacy-", legacy), func(t *testing.T) {
			t.Setenv("FMN_LIMIT_DECODE_REQUESTS", "4")
			t.Setenv("FMN_LIMIT_READ_REQUESTS", "1")
			t.Setenv("FMN_LIMIT_WRITE_REQUESTS", "1")
			const clientsCount = 64
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			entered := make(chan struct{}, clientsCount+1)
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
			manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "metrics", Kind: "http", Endpoint: upstream.URL, Operations: []connectors.Operation{{Name: "read", ReadOnly: true, Method: "GET", Path: "/"}}}}})
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			app, _ := fixture(t)
			app.dataConnections = manager
			server := httptest.NewServer(app)
			defer server.Close()
			app.origin = server.URL
			clients := make([]*mcp.ClientSession, 0, clientsCount)
			for i := 0; i < clientsCount; i++ {
				client := mcp.NewClient(&mcp.Implementation{Name: "concurrent-data-client", Version: "1"}, nil)
				if legacy {
					client.AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
						return func(ctx context.Context, method string, r mcp.Request) (mcp.Result, error) {
							if method == "server/discover" {
								return nil, &jsonrpc.Error{Code: -32601, Message: "legacy client fixture"}
							}
							return next(ctx, method, r)
						}
					})
				}
				session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: tokenTransport{}}}, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { finish(); session.Close() }()
				clients = append(clients, session)
			}
			// Hold a background consumer on exactly the same configured connection.
			background := make(chan error, 1)
			go func() { _, err := manager.Call(ctx, "metrics", "read", json.RawMessage(`{}`), ""); background <- err }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("background call did not enter")
			}
			app.rpcWork <- struct{}{}
			app.rpcReadWork <- struct{}{}
			defer func() { <-app.rpcWork; <-app.rpcReadWork }()
			// Discovering connector contracts must not need notebook read/write slots.
			discovered, err := clients[0].CallTool(ctx, &mcp.CallToolParams{Name: "list_data_operations", Arguments: map[string]any{"connection": "metrics"}})
			if err != nil || discovered.IsError {
				t.Fatalf("data discovery blocked by notebook work: %+v %v", discovered, err)
			}
			done := make(chan error, clientsCount)
			for i, client := range clients {
				go func() {
					result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "call_data_operation", Arguments: map[string]any{"connection": "metrics", "operation": "read", "arguments": map[string]any{}}})
					if err == nil && result.IsError {
						err = fmt.Errorf("data call failed: %+v", result.Content)
					}
					done <- err
				}()
				// Ramp admission deterministically while every earlier call stays blocked.
				// This tests long I/O concurrency independently of JSON decoding bursts.
				select {
				case <-entered:
				case err := <-done:
					t.Fatalf("client %d did not reach upstream: %v", i, err)
				case <-ctx.Done():
					t.Fatalf("only %d client calls reached upstream", i)
				}
			}
			if len(app.rpcDecodeWork) != 0 {
				t.Fatal("data I/O retained request decoding capacity")
			}
			finish()
			if err := <-background; err != nil {
				t.Fatal(err)
			}
			for i := 0; i < clientsCount; i++ {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
