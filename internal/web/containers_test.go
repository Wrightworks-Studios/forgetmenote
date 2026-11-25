package web

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/catalog"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"strings"
	"testing"
)

func TestContainerHTTPCommitAndCollection(t *testing.T) {
	app, store := fixture(t)
	original := "---\nstatus: draft\ncustom: keep\n---\n# Human text\nExact body.\n"
	doc, err := store.Save(context.Background(), "folder/note.md", []byte(original), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"path": doc.Path, "revision": doc.Revision, "container": "archive"})
	response := call(app, "POST", "/api/container", string(body), testToken)
	if response.Code != 200 {
		t.Fatalf("classify note: %d %s", response.Code, response.Body.String())
	}
	var saved noteOutput
	if err := json.Unmarshal(response.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Path != doc.Path || !strings.Contains(saved.Content, "fmn-container: archive") || !strings.Contains(saved.Content, "status: draft\ncustom: keep") || !strings.HasSuffix(saved.Content, "# Human text\nExact body.\n") {
		t.Fatalf("classification changed identity/body: %+v", saved)
	}
	current, err := store.Read(context.Background(), doc.Path, "")
	if err != nil || string(current.Content) != saved.Content || current.Revision == doc.Revision {
		t.Fatalf("not committed: %+v %v", current, err)
	}
	old, err := store.Read(context.Background(), doc.Path, doc.Revision)
	if err != nil || string(old.Content) != original {
		t.Fatalf("history changed: %+v %v", old, err)
	}
	for collection, present := range map[string]bool{"notes": false, "drafts": false, "archives": true, "all": true} {
		response = call(app, "GET", "/api/notes?collection="+collection, "", testToken)
		if response.Code != 200 || strings.Contains(response.Body.String(), doc.Path) != present {
			t.Fatalf("collection %s: %d %s", collection, response.Code, response.Body.String())
		}
	}
	response = call(app, "GET", "/api/browse?collection=notes", "", testToken)
	if response.Code != 200 || strings.Contains(response.Body.String(), `"path":"folder"`) {
		t.Fatalf("empty presentation folder: %d %s", response.Code, response.Body.String())
	}
}

func TestContainerHTTPAndMCPAuthority(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	for user, role := range map[string]string{"reader": "read", "commenter": "comment", "editor": "edit"} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", user, role); err != nil {
			t.Fatal(err)
		}
	}
	doc, err := store.Save(ctx, "folder/note.md", []byte("# Original"), "", "author")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"path": doc.Path, "revision": doc.Revision, "container": "draft"})
	for _, user := range []string{"reader", "commenter"} {
		if r := proxyCall(app, user, "POST", "/p/default/api/container", string(body)); r.Code != 403 {
			t.Fatalf("%s can classify: %d %s", user, r.Code, r.Body)
		}
	}
	if r := proxyCall(app, "editor", "POST", "/p/default/api/container", string(body)); r.Code != 200 {
		t.Fatalf("editor cannot classify: %d %s", r.Code, r.Body)
	}
	client := actionClient(t, app, "editor", "notes:read notes:write")
	doc, _ = store.Read(ctx, doc.Path, "")
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "set_note_container", Arguments: map[string]any{"project": "default", "path": doc.Path, "revision": doc.Revision, "container": "archive"}})
	if err != nil || result.IsError {
		t.Fatalf("MCP classify: %+v %v", result, err)
	}
	result, err = client.CallTool(ctx, &mcp.CallToolParams{Name: "browse_folder", Arguments: map[string]any{"project": "default", "path": "folder", "collection": "archives"}})
	if err != nil || result.IsError {
		t.Fatalf("MCP collection: %+v %v", result, err)
	}
	raw, _ := json.Marshal(result.StructuredContent)
	if !strings.Contains(string(raw), doc.Path) {
		t.Fatalf("MCP omitted archive: %s", raw)
	}
	doc, _ = store.Read(ctx, doc.Path, "")
	lock, err := store.SetLock(ctx, "folder", true, doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	actionCall(t, client, "set_note_container", map[string]any{"project": "default", "path": doc.Path, "revision": lock.Revision, "container": ""}, true)
	if err = app.catalog.Member(catalog.Bootstrap, "default", "editor", ""); err != nil {
		t.Fatal(err)
	}
	actionCall(t, client, "note_collections", map[string]any{"project": "default"}, true)
}

func TestContainerHTTPMalformedContinuationPreservesCommittedBytes(t *testing.T) {
	for name, separator := range map[string]string{"blank": "\n", "comment": "# separator\n"} {
		t.Run(name, func(t *testing.T) {
			app, store := fixture(t)
			ctx := context.Background()
			doc, err := store.Save(ctx, "malformed.md", []byte("# Original\n"), "", "editor")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]string{"path": doc.Path, "revision": doc.Revision, "content": "---\nfmn-container: archive\n" + separator + "  nested: value\n---\nBody\n"})
			response := call(app, "POST", "/api/note", string(body), testToken)
			if response.Code < 400 {
				t.Fatalf("HTTP committed malformed classification: %d %s", response.Code, response.Body)
			}
			current, err := store.Read(ctx, doc.Path, "")
			if err != nil || string(current.Content) != string(doc.Content) {
				t.Fatalf("changed bytes: %+v %v", current, err)
			}
			state, err := store.Status(ctx)
			if err != nil || state.Revision != doc.Revision || state.Dirty {
				t.Fatalf("changed Git: %+v %v", state, err)
			}
		})
	}
}
