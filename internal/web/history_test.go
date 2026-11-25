package web

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/knowledge"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestHistoricalSearchHTTPAndMCPReadPermissions(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	old, err := store.Save(ctx, "archive.md", []byte("# Earlier\nVanished cobalt evidence.\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(ctx, old.Path, []byte("# Today\nCopper evidence.\n"), old.Revision, ""); err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member("@local-admin", "default", "reader", "read"); err != nil {
		t.Fatal(err)
	}
	if err = app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	query := `{"scope":"history","mode":"text","text":"Vanished cobalt"}`
	check := func(raw []byte) {
		t.Helper()
		var result knowledge.HybridResult
		if err := json.Unmarshal(raw, &result); err != nil || len(result.Matches) != 1 || result.Matches[0].Revision != old.Revision || result.Scope != "history" {
			t.Fatalf("history response: %s %v", raw, err)
		}
	}
	w := proxyCall(app, "reader", "POST", "/p/default/api/knowledge/hybrid-search", query)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	check(w.Body.Bytes())
	if w = proxyCall(app, "reader", "GET", "/p/default/api/knowledge/passages?scope=history", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w = proxyCall(app, "reader", "POST", "/p/default/api/knowledge/embeddings", `{"scope":"history"}`); w.Code != 403 {
		t.Fatal("reader wrote embeddings", w.Code)
	}
	if w = proxyCall(app, "outsider", "POST", "/p/default/api/knowledge/hybrid-search", query); w.Code != 403 {
		t.Fatal("outsider read history", w.Code)
	}
	if w = proxyCall(app, "reader", "POST", "/p/default/api/knowledge/hybrid-search", `{"scope":"unknown","mode":"text","text":"evidence"}`); w.Code == 200 {
		t.Fatal("invalid scope accepted")
	}
	ts := httptest.NewServer(app)
	defer ts.Close()
	app.origin = ts.URL
	app.oauth.Access[digest("history-read-token")] = withOAuthFamily(accessGrant{Subject: "reader", Client: "history-fixture", Scope: "notes:read", Resource: ts.URL + "/mcp", Expires: time.Now().Add(time.Hour).Unix()})
	client := mcp.NewClient(&mcp.Implementation{Name: "history-fixture", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: "history-read-token"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := func(name string, args map[string]any, wantError bool) *mcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || result.IsError != wantError {
			t.Fatalf("%s: %+v %v", name, result, err)
		}
		return result
	}
	args := map[string]any{"project": "default", "query": map[string]any{"scope": "history", "mode": "text", "text": "Vanished cobalt"}}
	result := call("hybrid_search", args, false)
	raw, _ := json.Marshal(result.StructuredContent)
	check(raw)
	call("get_indexing_passages", map[string]any{"project": "default", "scope": "history"}, false)
	call("read_note", map[string]any{"project": "default", "path": old.Path, "revision": old.Revision}, false)
	call("submit_embeddings", map[string]any{"project": "default", "submission": map[string]any{"scope": "history"}}, true)
	if err = app.catalog.Member("@local-admin", "default", "reader", ""); err != nil {
		t.Fatal(err)
	}
	call("hybrid_search", args, true)
	call("get_indexing_passages", map[string]any{"project": "default", "scope": "history"}, true)
	if w = proxyCall(app, "reader", "POST", "/p/default/api/knowledge/hybrid-search", query); w.Code != 403 {
		t.Fatal("revoked reader read history", w.Code)
	}
}
