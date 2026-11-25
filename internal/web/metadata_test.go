package web

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/catalog"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPropertiesAndFilteringRespectProjectPermissions(t *testing.T) {
	app, store := fixture(t)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	app.catalog.Member(catalog.Bootstrap, "default", "reader", "read")
	app.catalog.Member(catalog.Bootstrap, "default", "writer", "edit")
	doc, err := store.Save(context.Background(), "a.md", []byte("# Original"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"path": "a.md", "revision": doc.Revision, "metadata": map[string]any{"title": "Readable title", "kind": "research", "status": "draft", "tags": []string{"science"}}})
	w := proxyCall(app, "reader", "POST", "/p/default/api/metadata", string(body))
	if w.Code != 403 {
		t.Fatalf("reader properties: %d %s", w.Code, w.Body)
	}
	w = proxyCall(app, "writer", "POST", "/p/default/api/metadata", string(body))
	if w.Code != 200 {
		t.Fatalf("writer properties: %d %s", w.Code, w.Body)
	}
	w = proxyCall(app, "reader", "GET", "/p/default/api/notes?tag=science&status=draft", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Readable title") {
		t.Fatalf("filter: %d %s", w.Code, w.Body)
	}
	w = proxyCall(app, "reader", "GET", "/p/default/api/note?path=a.md", "")
	var note noteOutput
	json.Unmarshal(w.Body.Bytes(), &note)
	if strings.Contains(note.HTML, "tags") || !strings.Contains(note.HTML, "Original") {
		t.Fatal("front matter leaked into preview", note.HTML)
	}
}

func TestMCPPropertiesAndMovesUseWorkspace(t *testing.T) {
	app, store := fixture(t)
	ts := httptest.NewServer(app)
	defer ts.Close()
	app.origin = ts.URL
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: tokenTransport{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	doc, err := store.Save(context.Background(), "a.md", []byte("Synthetic astronomy"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "set_note_properties", Arguments: map[string]any{"project": "default", "path": "a.md", "revision": doc.Revision, "metadata": map[string]any{"title": "Research", "kind": "note", "status": "draft", "tags": []string{"science"}}}})
	if err != nil || result.IsError {
		t.Fatalf("properties: %+v %v", result, err)
	}
	result, err = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "search_notes", Arguments: map[string]any{"project": "default", "query": "astronomy", "tag": "science"}})
	if err != nil || result.IsError {
		t.Fatalf("search: %+v %v", result, err)
	}
	raw, _ := json.Marshal(result)
	if !strings.Contains(string(raw), "a.md") {
		t.Fatal("MCP search omitted note", string(raw))
	}
	state, _ := store.Status(context.Background())
	result, err = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "move_notes", Arguments: map[string]any{"project": "default", "from": "a.md", "to": "notes/research.md", "revision": state.Revision}})
	if err != nil || result.IsError {
		t.Fatalf("move: %+v %v", result, err)
	}
	if _, err = store.Read(context.Background(), "notes/research.md", ""); err != nil {
		t.Fatal(err)
	}
}
