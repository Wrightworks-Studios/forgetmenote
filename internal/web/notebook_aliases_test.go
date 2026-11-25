package web

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"strings"
	"testing"
)

func TestNotebookAliasesShareHTTPMCPAuthorityAndRevisions(t *testing.T) {
	app, _ := fixture(t)
	canonical := apiCall(app, "GET", "/api/notebooks", "", testToken)
	legacy := apiCall(app, "GET", "/api/projects", "", testToken)
	if canonical.Code != 200 || canonical.Body.String() != legacy.Body.String() {
		t.Fatalf("notebook list alias: %d %s", canonical.Code, canonical.Body)
	}
	if w := apiCall(app, "POST", "/api/notebooks", `{"id":"aliased","name":"Aliased notebook"}`, testToken); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w := apiCall(app, "POST", "/notebooks/aliased/api/note", `{"path":"Alias.md","content":"# Same storage","revision":""}`, testToken); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	canonical = apiCall(app, "GET", "/notebooks/aliased/api/note?path=Alias.md", "", testToken)
	legacy = apiCall(app, "GET", "/p/aliased/api/note?path=Alias.md", "", testToken)
	if canonical.Code != 200 || canonical.Body.String() != legacy.Body.String() {
		t.Fatal("aliases did not read the same revision", canonical.Code, canonical.Body)
	}
	if w := apiCall(app, "POST", "/notebooks/aliased/api/note", `{"path":"Alias.md","content":"stale","revision":"stale"}`, testToken); w.Code != 409 {
		t.Fatal("alias bypassed revision check", w.Code, w.Body)
	}
	client := actionClient(t, app, catalog.Bootstrap, "notes:read notes:write notes:manage")
	tools, err := client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	for _, name := range []string{"list_notebooks", "notebook_status", "create_notebook", "import_notebook_bundle", "export_notebook_backup", "set_notebook_member", "notebook_instructions", "set_notebook_instructions", "read_notebook_changes", "set_notebook_group"} {
		if !names[name] {
			t.Fatalf("missing notebook tool %s", name)
		}
	}
	a := actionCall(t, client, "list_notebooks", map[string]any{}, false)
	b := actionCall(t, client, "list_projects", map[string]any{}, false)
	if string(a) != string(b) {
		t.Fatal("MCP notebook alias changed visibility")
	}
	actionCall(t, client, "create_notebook", map[string]any{"id": "mcp-aliased", "name": "MCP aliased"}, false)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alias-reader", "read"); err != nil {
		t.Fatal(err)
	}
	grant, err := issueCredential(app.catalog, app.oauth, "alias-reader", CredentialSpec{Name: "limited alias", Projects: []string{"default"}, Scopes: []string{"notes:read"}, Audiences: []string{"api"}, Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	w := apiCall(app, "POST", "/api/v1/actions/list_notebooks", `{}`, grant.Token)
	if w.Code != 200 || strings.Contains(w.Body.String(), "mcp-aliased") {
		t.Fatal("alias exposed ungranted notebooks", w.Code, w.Body)
	}
	var response json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	w = apiCall(app, "POST", "/api/v1/actions/notebook_status", `{"project":"aliased"}`, grant.Token)
	if w.Code != 403 {
		t.Fatal("alias bypassed notebook restriction", w.Code, w.Body)
	}
	w = apiCall(app, "POST", "/api/v1/actions/create_notebook", `{"id":"forbidden-alias","name":"Denied"}`, grant.Token)
	if w.Code != 403 {
		t.Fatal("alias bypassed management consent", w.Code, w.Body)
	}
}

func TestNotebookInstructionAliasSharesChatMutationPolicy(t *testing.T) {
	for _, name := range []string{"set_project_instructions", "set_notebook_instructions"} {
		t.Run(name, func(t *testing.T) {
			app, store := fixture(t)
			run := chat.Run{Options: chat.Options{Mode: "edit", Corpus: &chat.Corpus{Version: 1, Archives: "exclude", Drafts: "exclude"}}}
			content, err := chatContentScope(run)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(t.Context(), identityKey{}, catalog.Bootstrap)
			ctx = context.WithValue(ctx, chatScopeKey{}, &chatRunScope{Owner: catalog.Bootstrap, Project: "default", Run: run, Content: content})
			var revision string
			if d, err := store.Read(ctx, "AGENTS.md", ""); err == nil {
				revision = d.Revision
			}
			raw, _ := json.Marshal(map[string]any{"content": "# Notebook instructions\nUse citations.\n", "revision": revision})
			if !allowedAction("edit", name, raw, nil, true) {
				t.Fatal("approved alias excluded by policy")
			}
			if _, err := app.dispatchAction(ctx, "default", name, raw); err != nil {
				t.Fatalf("approved %s did not share the instruction control: %v", name, err)
			}
		})
	}
}
