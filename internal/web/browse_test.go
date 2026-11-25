package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCoversAndFolderBrowse(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	for path, content := range map[string]string{"assets/cover.png": "image fixture", "wiki/index.md": "# Overview\nWelcome.", "wiki/one.md": "# First note\nA **useful** preview.", "wiki/nested/two.md": "# Nested", "wiki-other/no.md": "# Other"} {
		state, _ := store.Status(ctx)
		if _, err := store.Save(ctx, path, []byte(content), state.Revision, "test"); err != nil {
			t.Fatal(err)
		}
	}
	doc, _ := store.Read(ctx, "wiki/one.md", "")
	body, _ := json.Marshal(map[string]any{"path": doc.Path, "revision": doc.Revision, "metadata": map[string]any{"title": "Card title", "cover": "assets/cover.png"}})
	res := call(app, "POST", "/api/metadata", string(body), testToken)
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"cover":"assets/cover.png"`) {
		t.Fatalf("cover save: %d %s", res.Code, res.Body.String())
	}
	doc, _ = store.Read(ctx, doc.Path, "")
	// Old clients omit cover; they must preserve it when editing other fields.
	body, _ = json.Marshal(map[string]any{"path": doc.Path, "revision": doc.Revision, "metadata": map[string]any{"title": "Retitled"}})
	res = call(app, "POST", "/api/metadata", string(body), testToken)
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"cover":"assets/cover.png"`) {
		t.Fatalf("legacy client lost cover: %s", res.Body.String())
	}
	res = call(app, "GET", "/api/browse?path=wiki", "", testToken)
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"excerpt":"A useful preview."`) || !strings.Contains(res.Body.String(), `"kind":"folder"`) || strings.Contains(res.Body.String(), "wiki-other") || strings.Contains(res.Body.String(), "nested/two.md") {
		t.Fatalf("browse: %d %s", res.Code, res.Body.String())
	}
	if r := call(app, "GET", "/api/browse?path=../", "", testToken); r.Code == 200 {
		t.Fatal("unsafe folder accepted")
	}
	if r := call(app, "GET", "/api/browse?path=wiki", "", ""); r.Code != http.StatusUnauthorized {
		t.Fatal("anonymous browse", r.Code)
	}
	client := actionClient(t, app, catalog.Bootstrap, "notes:read notes:write")
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "browse_folder", Arguments: map[string]any{"project": "default", "path": "wiki"}})
	if err != nil || result.IsError {
		t.Fatalf("MCP browse: %v %v", result, err)
	}
	raw, _ := json.Marshal(result.StructuredContent)
	if !strings.Contains(string(raw), "assets/cover.png") {
		t.Fatalf("MCP cover: %s", raw)
	}
	doc, _ = store.Read(ctx, doc.Path, "")
	result, err = client.CallTool(ctx, &mcp.CallToolParams{Name: "set_note_properties", Arguments: map[string]any{"project": "default", "path": doc.Path, "revision": doc.Revision, "metadata": map[string]any{"cover": "", "title": "Retitled", "kind": "", "status": "", "tags": []string{}}}})
	if err != nil || result.IsError {
		raw, _ := json.Marshal(result)
		t.Fatalf("MCP clear: %s %v", raw, err)
	}
	cleared, _ := store.Read(ctx, doc.Path, "")
	if strings.Contains(string(cleared.Content), "assets/cover.png") {
		t.Fatal("cover was not cleared")
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "reader", "read"); err != nil {
		t.Fatal(err)
	}
	reader := actionClient(t, app, "reader", "notes:read notes:write")
	actionCall(t, reader, "browse_folder", map[string]any{"project": "default", "path": "wiki"}, false)
	actionCall(t, reader, "set_note_properties", map[string]any{"project": "default", "path": doc.Path, "revision": doc.Revision, "metadata": map[string]any{"cover": "assets/cover.png", "title": "", "kind": "", "status": "", "tags": []string{}}}, true)
	app.catalog.Member(catalog.Bootstrap, "default", "reader", "")
	actionCall(t, reader, "browse_folder", map[string]any{"project": "default", "path": "wiki"}, true)
	for _, cover := range []string{"https://outside.test/image.png", "assets/../private.png", "assets/a.svg", "assets/missing.png"} {
		doc, _ = store.Read(ctx, doc.Path, "")
		body, _ = json.Marshal(map[string]any{"path": doc.Path, "revision": doc.Revision, "metadata": map[string]any{"cover": cover}})
		if r := call(app, "POST", "/api/metadata", string(body), testToken); r.Code == 200 {
			t.Fatalf("unsafe/missing cover accepted: %s", cover)
		}
	}
}
