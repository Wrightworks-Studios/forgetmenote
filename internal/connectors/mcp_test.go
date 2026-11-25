package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestMCPConfiguredToolsAndSchemaDrift(t *testing.T) {
	upstream := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	var initializations atomic.Int32
	upstream.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "initialize" || method == "server/discover" {
				initializations.Add(1)
			}
			return next(ctx, method, req)
		}
	})
	type Args struct {
		Term string `json:"term"`
	}
	type Output struct {
		Answer string `json:"answer"`
	}
	called := 0
	mcp.AddTool(upstream, &mcp.Tool{Name: "find", Description: "Find a record"}, func(ctx context.Context, _ *mcp.CallToolRequest, in Args) (*mcp.CallToolResult, Output, error) {
		called++
		return nil, Output{in.Term}, nil
	})
	mcp.AddTool(upstream, &mcp.Tool{Name: "delete_all", Description: "Never expose this tool"}, func(context.Context, *mcp.CallToolRequest, Args) (*mcp.CallToolResult, Output, error) {
		t.Error("unconfigured tool executed")
		return nil, Output{}, nil
	})
	server := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upstream }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer server.Close()
	manager, e := New(Config{Version: 1, Connections: []Connection{{ID: "extension", Kind: "mcp", Endpoint: server.URL, Operations: []Operation{{Name: "find", ReadOnly: true}}}}})
	if e != nil {
		t.Fatal(e)
	}
	defer manager.Close()
	tools, e := manager.Tools(context.Background(), "extension")
	if e != nil || len(tools) != 1 || tools[0].Name != "find" {
		t.Fatal(tools, e)
	}
	result, e := manager.Call(context.Background(), "extension", "find", json.RawMessage(`{"term":"hello"}`), tools[0].SchemaHash)
	if e != nil || !result.Receipt.Complete || called != 1 {
		t.Fatal(result, e, called)
	}
	if initializations.Load() != 2 {
		t.Fatal("discovery and execution opened redundant sessions", initializations.Load())
	}
	if _, e = manager.Call(context.Background(), "extension", "delete_all", json.RawMessage(`{"term":"hello"}`), ""); e == nil {
		t.Fatal("unconfigured tool accessible")
	}
	upstream.RemoveTools("find")
	mcp.AddTool(upstream, &mcp.Tool{Name: "find", Description: "Changed operation"}, func(context.Context, *mcp.CallToolRequest, struct {
		ID int `json:"id"`
	}) (*mcp.CallToolResult, Output, error) {
		called++
		return nil, Output{}, nil
	})
	expireDiscovery(manager, "extension")
	if _, e = manager.Call(context.Background(), "extension", "find", json.RawMessage(`{"id":1}`), tools[0].SchemaHash); !errors.Is(e, ErrSchemaChanged) {
		t.Fatal("stale schema accepted", e)
	}
	if called != 1 {
		t.Fatal("schema drift reached upstream")
	}
}

// expireDiscovery ages a connection's discovered contract past its lifetime,
// as if a minute had passed since the upstream was last asked.
func expireDiscovery(m *Manager, id string) {
	r := m.connections[id]
	r.contractMu.Lock()
	defer r.contractMu.Unlock()
	if r.discovered != nil {
		aged := *r.discovered
		aged.expires = time.Now().Add(-time.Second)
		r.discovered = &aged
	}
}

func TestMCPCallsReuseADiscoveredContractUntilItExpiresOrDiffers(t *testing.T) {
	upstream := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	var lists, sessions atomic.Int32
	upstream.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case "tools/list":
				lists.Add(1)
			case "initialize", "server/discover":
				sessions.Add(1)
			}
			return next(ctx, method, req)
		}
	})
	type Args struct {
		Term string `json:"term"`
	}
	type Output struct {
		Answer string `json:"answer"`
	}
	var called atomic.Int32
	find := func(context.Context, *mcp.CallToolRequest, Args) (*mcp.CallToolResult, Output, error) {
		called.Add(1)
		return nil, Output{"found"}, nil
	}
	mcp.AddTool(upstream, &mcp.Tool{Name: "find", Description: "Find a record"}, find)
	server := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upstream }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer server.Close()
	cfg := Config{Version: 1, Connections: []Connection{{ID: "extension", Kind: "mcp", Endpoint: server.URL, Operations: []Operation{{Name: "find", ReadOnly: true}}}}}
	manager, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	tools, err := manager.Tools(t.Context(), "extension")
	if err != nil || len(tools) != 1 {
		t.Fatal(tools, err)
	}
	for range 3 {
		if _, err = manager.Call(t.Context(), "extension", "find", json.RawMessage(`{"term":"a"}`), tools[0].SchemaHash); err != nil {
			t.Fatal(err)
		}
	}
	if lists.Load() != 1 || sessions.Load() != 4 || called.Load() != 3 {
		t.Fatal("calls asked the upstream again for a contract discovered moments before", lists.Load(), sessions.Load(), called.Load())
	}
	if _, err = manager.Call(t.Context(), "extension", "find", json.RawMessage(`{"term":5}`), tools[0].SchemaHash); err == nil || sessions.Load() != 4 {
		t.Fatal("invalid arguments reached the upstream", err, sessions.Load())
	}

	// Another gateway inspected a newer contract. A caller holding it makes this
	// gateway ask the upstream again instead of refusing it as changed.
	upstream.RemoveTools("find")
	mcp.AddTool(upstream, &mcp.Tool{Name: "find", Description: "Find a record by term"}, find)
	other, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	newer, err := other.Tools(t.Context(), "extension")
	if err != nil || newer[0].SchemaHash == tools[0].SchemaHash {
		t.Fatal("the changed description kept its contract identity", err)
	}
	if _, err = manager.Call(t.Context(), "extension", "find", json.RawMessage(`{"term":"a"}`), newer[0].SchemaHash); err != nil || lists.Load() != 3 {
		t.Fatal("a newer contract than the cached one was refused", err, lists.Load())
	}

	// A failed call forgets the contract, so the next call asks again.
	upstream.RemoveTools("find")
	mcp.AddTool(upstream, &mcp.Tool{Name: "find", Description: "Find a record by term"}, func(context.Context, *mcp.CallToolRequest, Args) (*mcp.CallToolResult, Output, error) {
		return nil, Output{}, errors.New("fixture failure")
	})
	if _, err = manager.Call(t.Context(), "extension", "find", json.RawMessage(`{"term":"a"}`), newer[0].SchemaHash); err == nil {
		t.Fatal("a failed upstream call succeeded")
	}
	upstream.RemoveTools("find")
	mcp.AddTool(upstream, &mcp.Tool{Name: "find", Description: "Find a record by term"}, find)
	if _, err = manager.Call(t.Context(), "extension", "find", json.RawMessage(`{"term":"a"}`), newer[0].SchemaHash); err != nil || lists.Load() != 4 {
		t.Fatal("a call after a failure reused the contract it failed with", err, lists.Load())
	}

	// Once the contract expires, a call asks again and refuses a drifted schema
	// before it reaches the upstream.
	before := called.Load()
	upstream.RemoveTools("find")
	mcp.AddTool(upstream, &mcp.Tool{Name: "find", Description: "Find a record by term"}, func(context.Context, *mcp.CallToolRequest, struct {
		ID int `json:"id"`
	}) (*mcp.CallToolResult, Output, error) {
		called.Add(1)
		return nil, Output{}, nil
	})
	expireDiscovery(manager, "extension")
	if _, err = manager.Call(t.Context(), "extension", "find", json.RawMessage(`{"id":1}`), newer[0].SchemaHash); !errors.Is(err, ErrSchemaChanged) || lists.Load() != 5 {
		t.Fatal("an expired contract was trusted", err, lists.Load())
	}
	if called.Load() != before {
		t.Fatal("schema drift reached the upstream")
	}
}
