package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A configured connection with many operations, each with its own schemas, as
// an operator's HTTP gateway has.
func BenchmarkConfiguredHTTPCall(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answer":"found"}`))
	}))
	defer upstream.Close()
	properties := []string{}
	for i := range 16 {
		properties = append(properties, fmt.Sprintf(`"field%d":{"type":"string","description":"A filter on field %d of the record"}`, i, i))
	}
	input := `{"type":"object","properties":{` + strings.Join(properties, ",") + `},"additionalProperties":false}`
	output := `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`
	ops := []Operation{}
	for i := range 64 {
		ops = append(ops, Operation{Name: fmt.Sprintf("op%d", i), Description: "Read records", ReadOnly: true, Method: "GET", Path: "/records", InputSchema: json.RawMessage(input), OutputSchema: json.RawMessage(output)})
	}
	m, err := New(Config{Version: 1, Connections: []Connection{{ID: "gateway", Kind: "http", Endpoint: upstream.URL, Operations: ops}}})
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	args := json.RawMessage(`{"field3":"x"}`)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := m.Call(context.Background(), "gateway", "op7", args, ""); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMCPCall(b *testing.B) {
	upstream := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	type Args struct {
		Term string `json:"term"`
	}
	type Output struct {
		Answer string `json:"answer"`
	}
	mcp.AddTool(upstream, &mcp.Tool{Name: "find", Description: "Find a record"}, func(context.Context, *mcp.CallToolRequest, Args) (*mcp.CallToolResult, Output, error) {
		return nil, Output{"found"}, nil
	})
	var requests atomic.Int64
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upstream }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	m, err := New(Config{Version: 1, Connections: []Connection{{ID: "extension", Kind: "mcp", Endpoint: server.URL, Operations: []Operation{{Name: "find", ReadOnly: true}}}}})
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	args := json.RawMessage(`{"term":"hello"}`)
	requests.Store(0)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := m.Call(context.Background(), "extension", "find", args, ""); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(requests.Load())/float64(b.N), "upstream-requests/op")
}
