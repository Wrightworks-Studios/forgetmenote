package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func actionClient(t *testing.T, app *Server, user, scope string) *mcp.ClientSession {
	t.Helper()
	ts := httptest.NewServer(app)
	t.Cleanup(ts.Close)
	app.origin = ts.URL
	token := "actions-" + digest(user+scope)
	app.oauth.Mu.Lock()
	app.oauth.Access[digest(token)] = withOAuthFamily(accessGrant{Subject: user, Client: "actions-client", Scope: scope, Resource: ts.URL + "/mcp", Expires: time.Now().Add(time.Hour).Unix(), Epoch: app.catalog.Epoch(user)})
	app.oauth.Refresh[digest(token+"-refresh")] = app.oauth.Access[digest(token)]
	app.oauth.Mu.Unlock()
	client := mcp.NewClient(&mcp.Implementation{Name: "actions", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func actionCall(t *testing.T, client *mcp.ClientSession, name string, args any, wantError bool) json.RawMessage {
	t.Helper()
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(name, err)
	}
	if result.IsError != wantError {
		raw, _ := json.Marshal(result)
		t.Fatalf("%s: %s", name, raw)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestMCPDiscoverySchemasAreObjects(t *testing.T) {
	app, _ := fixture(t)
	w := call(app, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, testToken)
	var response struct {
		Result struct {
			Tools []struct {
				Name         string          `json:"name"`
				InputSchema  json.RawMessage `json:"inputSchema"`
				OutputSchema json.RawMessage `json:"outputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != 200 || len(response.Result.Tools) == 0 {
		t.Fatalf("tools/list: HTTP %d %s (%v)", w.Code, w.Body.String(), err)
	}
	for _, tool := range response.Result.Tools {
		for field, raw := range map[string]json.RawMessage{"inputSchema": tool.InputSchema, "outputSchema": tool.OutputSchema} {
			if field == "outputSchema" && len(raw) == 0 {
				continue
			}
			var schema map[string]json.RawMessage
			if err := json.Unmarshal(raw, &schema); err != nil || schema == nil {
				t.Errorf("%s %s must be a JSON object, got %s (%v)", tool.Name, field, raw, err)
			}
			var properties map[string]json.RawMessage
			if json.Unmarshal(schema["properties"], &properties) == nil {
				for name, property := range properties {
					if bytes.Equal(property, []byte("true")) || bytes.Equal(property, []byte("false")) {
						t.Errorf("%s %s property %s must use an object schema", tool.Name, field, name)
					}
				}
			}
		}
	}
}

func TestMCPSchemaPreservesBooleanData(t *testing.T) {
	var schema jsonschema.Schema
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"data":true,"nested":{"type":"object","properties":{"value":true}}},"default":{"data":true,"count":9007199254740993},"examples":[{"data":false}]}`), &schema); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(mcpToolSchema(&schema, false))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Default    map[string]json.RawMessage `json:"default"`
		Examples   []map[string]bool          `json:"examples"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if string(result.Properties["data"]) != "{}" || string(result.Default["data"]) != "true" || string(result.Default["count"]) != "9007199254740993" || len(result.Examples) != 1 || result.Examples[0]["data"] {
		t.Fatalf("schema conversion changed data or left a boolean schema: %s", raw)
	}
	var nested struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(result.Properties["nested"], &nested) != nil || string(nested.Properties["value"]) != "{}" {
		t.Fatalf("nested boolean schema was not converted: %s", raw)
	}
}

// A client checks each result against the output schema it listed when it
// connected. One that listed remember before a release added reviewReason, and
// items[].similar before that, still accepts results carrying them. Arguments
// stay closed, so a misspelled one is refused.
func TestMCPOutputSchemasAcceptFieldsAddedLater(t *testing.T) {
	app, _ := fixture(t)
	client := actionClient(t, app, catalog.Bootstrap, "notes:read notes:write")
	tools, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var closed func(path string, value any) []string
	closed = func(path string, value any) (found []string) {
		switch v := value.(type) {
		case map[string]any:
			if extra, ok := v["additionalProperties"]; ok && (extra == false || mcpClosedSchema(extra)) {
				found = append(found, path)
			}
			for key, child := range v {
				found = append(found, closed(path+"/"+key, child)...)
			}
		case []any:
			for i, child := range v {
				found = append(found, closed(path+"/"+strconv.Itoa(i), child)...)
			}
		}
		return found
	}
	var remember map[string]any
	for _, tool := range tools.Tools {
		var output map[string]any
		raw, _ := json.Marshal(tool.OutputSchema)
		if err := json.Unmarshal(raw, &output); err != nil {
			t.Fatal(tool.Name, err)
		}
		if paths := closed("", output); len(paths) > 0 {
			t.Errorf("%s output schema refuses new fields at %v", tool.Name, paths)
		}
		if tool.Name == "remember" {
			remember = output
			if raw, _ := json.Marshal(tool.InputSchema); !bytes.Contains(raw, []byte(`"additionalProperties":{"not":{}}`)) {
				t.Errorf("remember accepts unknown arguments: %s", raw)
			}
		}
	}
	if remember == nil {
		t.Fatal("remember is not listed")
	}
	delete(remember["properties"].(map[string]any), "reviewReason")
	items := remember["properties"].(map[string]any)["items"].(map[string]any)["items"].(map[string]any)
	delete(items["properties"].(map[string]any), "similar")
	raw, _ := json.Marshal(remember)
	var earlier jsonschema.Schema
	if err := json.Unmarshal(raw, &earlier); err != nil {
		t.Fatal(err)
	}
	resolved, err := earlier.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]any{"notebook": "assistant-x", "path": "memory.md", "revision": "r1", "items": []any{map[string]any{"id": "m1", "similar": "m0"}}, "review": "failed", "reviewReason": "capacity"}
	if err := resolved.Validate(result); err != nil {
		t.Fatalf("an earlier remember schema refuses a current result: %v", err)
	}
}

func TestMCPLegacyClientToolSchemasAndArrayResults(t *testing.T) {
	app, _ := fixture(t)
	request := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", app.origin+"/mcp", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+testToken)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("MCP-Protocol-Version", "2025-11-25")
		w := httptest.NewRecorder()
		serveMCPTest(app, w, r)
		return w
	}
	w := request(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	var response struct {
		Result struct {
			Tools []struct {
				Name         string         `json:"name"`
				OutputSchema map[string]any `json:"outputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != 200 || len(response.Result.Tools) == 0 {
		t.Fatalf("tools/list: HTTP %d %s (%v)", w.Code, w.Body.String(), err)
	}
	for _, tool := range response.Result.Tools {
		if tool.OutputSchema != nil && tool.OutputSchema["type"] != "object" {
			t.Errorf("%s advertises a non-object output schema to a legacy client", tool.Name)
		}
	}
	w = request(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_projects","arguments":{}}}`)
	var result struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Structured json.RawMessage `json:"structuredContent"`
			IsError    bool            `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || result.Result.IsError {
		t.Fatalf("list_projects: HTTP %d %s (%v)", w.Code, w.Body.String(), err)
	}
	if len(result.Result.Structured) != 0 {
		t.Fatalf("legacy array result must use the JSON text fallback: %s", result.Result.Structured)
	}
	var projects []struct {
		ID string `json:"id"`
	}
	if len(result.Result.Content) == 0 || json.Unmarshal([]byte(result.Result.Content[0].Text), &projects) != nil || len(projects) == 0 || projects[0].ID != "default" {
		t.Fatalf("missing project array in JSON text fallback: %s", w.Body.String())
	}
	client := actionClient(t, app, "@local-admin", "notes:read")
	tools, err := client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range tools.Tools {
		if tool.Name == "list_projects" {
			found = true
			raw, _ := json.Marshal(tool.OutputSchema)
			var schema struct {
				Type []string `json:"type"`
			}
			if json.Unmarshal(raw, &schema) != nil || len(schema.Type) != 2 || schema.Type[0] != "null" || schema.Type[1] != "array" {
				t.Fatalf("current client lost its array schema after legacy discovery: %s", raw)
			}
		}
	}
	if !found {
		t.Fatal("current client is missing list_projects")
	}
	if raw := actionCall(t, client, "list_projects", map[string]any{}, false); json.Unmarshal(raw, &projects) != nil || len(projects) == 0 {
		t.Fatalf("current client lost its structured array: %s", raw)
	}
}

func TestMCPActionParityInventory(t *testing.T) {
	app, _ := fixture(t)
	client := actionClient(t, app, "@local-admin", "notes:read notes:write notes:manage data:read data:write")
	result, err := client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]bool{}
	for _, tool := range result.Tools {
		tools[tool.Name] = true
	}
	for _, name := range []string{"capabilities", "set_note_lock", "upload_attachment", "list_attachments", "read_attachment", "resolve_comment", "delete_comment", "set_project_instructions", "set_semantic_config", "restore_note", "preview_markdown", "export_project_backup", "administration", "create_project", "set_project_member", "set_user_access"} {
		if !tools[name] {
			t.Errorf("browser action has no MCP equivalent: %s", name)
		}
	}
	for name := range assistantActions {
		// Account-page-only actions keep the shared registration but are not
		// offered to MCP clients, which they always refuse.
		if _, registered := app.actions[name]; !registered || !tools[name] && !accountUIActions[name] {
			t.Errorf("assistant action has no shared MCP registration: %s", name)
		}
	}
	// Every HTTP API action must map to a registered tool or a deliberate interactive exception.
	parity := map[string]string{
		"POST /api/assistant/action/{action}":    "assistant_status",
		"POST /api/organization/action/{action}": "organization_status",
		"GET /api/authority/status":              "notebook_authority_status", "POST /api/authority/preview": "preview_notebook_authority_change", "POST /api/authority/apply": "apply_notebook_authority_change", "POST /api/authority/cancel": "cancel_notebook_authority_change",
		"GET /api/managed-backup": "managed_backup_status", "POST /api/managed-backup/control": "control_managed_backup",
		"POST /api/managed-backup/connect": "interactive repository setup", "GET /api/managed-backup/choices": "interactive repository setup", "POST /api/managed-backup/select": "interactive repository setup", "POST /api/managed-backup/create": "interactive repository setup",
		"POST /api/retrieval":               "search_repository_sources",
		"POST /api/repository/index-config": "configure_repository_index",
		"GET /api/repository/map":           "repository_source_map",
		"POST /api/notebook-links/resolve":  "resolve_notebook_link", "GET /api/notebook-links/targets": "find_notebook_link_targets",
		"GET /api/repository": "repository_source_status", "GET /api/repository/tree": "repository_source_tree", "GET /api/repository/file": "repository_source_file", "GET /api/repository/changes": "repository_source_changes", "POST /api/repository/refresh": "refresh_repository_source", "POST /api/repository/capture": "retain_repository_citation",
		// Repository sharing consent and connection management require Owner access.
		"POST /api/repository/connect": "interactive repository setup", "GET /api/repository/choices": "interactive repository setup", "POST /api/repository/select": "interactive repository setup", "POST /api/repository/control": "interactive repository setup",
		"POST /api/judgments/jobs": "interactive personal review", "GET /api/judgments/jobs": "interactive personal review", "GET /api/judgments/jobs/{job}": "interactive personal review",
		"POST /api/judgments/jobs/{job}/continue": "interactive personal review", "POST /api/judgments/jobs/{job}/cancel": "interactive personal review",
		// The former /api/jev paths serve the same judgments routes for one release.
		"POST /api/jev/jobs": "interactive personal review", "GET /api/jev/jobs": "interactive personal review", "GET /api/jev/jobs/{job}": "interactive personal review",
		"POST /api/jev/jobs/{job}/continue": "interactive personal review", "POST /api/jev/jobs/{job}/cancel": "interactive personal review",
		"GET /api/notifications": "interactive personal review", "POST /api/notifications/read": "interactive personal review",
		"GET /api/knowledge/retrieval-settings": "get_retrieval_settings", "POST /api/knowledge/retrieval-settings": "set_retrieval_settings",
		"POST /api/wiki":      "set_notebook_wiki",
		"POST /api/container": "set_note_container", "GET /api/collections": "note_collections",
		// MCP clients move a folder's notes with set_note_container or one edit group.
		"POST /api/folder-container": "set_note_container",
		"GET /api/trash":             "list_trash", "POST /api/trash": "trash_notes", "POST /api/trash/restore": "restore_trash", "POST /api/trash/empty": "empty_trash",
		// Agents only prepare a purge; running it and its receipts are a person's, in the browser.
		"POST /api/purge/prepare": "prepare_purge", "POST /api/purge/execute": "interactive purge confirmation", "GET /api/purge": "interactive purge confirmation",
		"GET /api/live": "read_live_note", "POST /api/live": "patch_live_note", "POST /api/live/patch": "patch_live_note", "POST /api/live/checkpoint": "checkpoint_live_note",
		"POST /api/projects/import": "import_project_bundle",
		"GET /api/document":         "read_document", "GET /api/map": "notebook_map", "GET /api/documents": "list_documents",
		"GET /api/document/asset":   "read_document",
		"POST /api/writing/outline": "document_outline", "POST /api/writing/terms": "find_term_mentions",
		"POST /api/projects/repository/connect": "interactive repository setup", "GET /api/projects/repository/choices": "interactive repository setup", "POST /api/projects/repository/finish": "interactive repository setup",
		"POST /api/sources/jobs": "prepare_source", "GET /api/sources/jobs/{job}": "source_job", "POST /api/sources/jobs/{job}/cancel": "cancel_source_job", "POST /api/sources/jobs/{job}/resume": "resume_source_job",
		"GET /api/people": "find_workspace_people", "GET /api/sources/formats": "source_formats", "POST /api/sources/preview": "archive_source", "POST /api/sources/import": "archive_source", "GET /api/sources/verify": "verify_source",
		// Private account consent and capture/review orchestration are explicit
		// browser interactions. Their note operations still use the shared
		// workspace methods; an MCP client cannot silently enable remote use.
		"GET /api/judgments/settings": "interactive personal review", "POST /api/judgments/settings": "interactive personal review",
		"GET /api/jev/settings": "interactive personal review", "POST /api/jev/settings": "interactive personal review",
		// Organizer grants must originate in the browser and remain bound to the
		// selected policy; ordinary MCP proposal confirmation remains unchanged.
		"GET /api/judgments/organization": "interactive personal review", "POST /api/judgments/organization": "interactive personal review",
		"POST /api/judgments/jobs/{job}/organization/{action}": "interactive personal review",
		"GET /api/jev/organization":                            "interactive personal review", "POST /api/jev/organization": "interactive personal review",
		"POST /api/jev/jobs/{job}/organization/{action}": "interactive personal review",
		"GET /api/chat/{chat}/runs/{run}/citations":      "interactive personal review", "POST /api/chat/{chat}/runs/{run}/citations": "interactive personal review",
		"GET /api/quicknotes": "interactive private capture", "POST /api/quicknotes": "interactive private capture",
		"GET /api/quicknotes/settings": "interactive private capture", "POST /api/quicknotes/settings": "interactive private capture",
		"GET /api/quicknotes/destinations": "interactive private capture", "POST /api/quicknotes/file": "interactive private capture",
		"POST /api/quicknotes/undo": "interactive private capture", "POST /api/quicknotes/recover": "interactive private capture", "POST /api/quicknotes/sort": "interactive private capture",
		"POST /api/local-users":     "interactive password provisioning",
		"GET /api/chat/connections": "interactive private chat", "GET /api/chat": "interactive private chat", "POST /api/chat": "interactive private chat", "GET /api/chat/{chat}": "interactive private chat", "GET /api/chat/{chat}/events": "interactive private chat", "POST /api/chat/{chat}/turns": "interactive private chat", "POST /api/chat/{chat}/runs/{run}/cancel": "interactive private chat", "POST /api/chat/{chat}/runs/{run}/resume": "interactive private chat", "POST /api/chat/{chat}/save-note": "interactive private chat", "POST /api/chat/{chat}/actions/{action}": "interactive private chat",
		"GET /api/me": "capabilities", "GET /api/projects": "list_projects", "POST /api/projects": "create_project",
		"GET /api/admin": "administration", "POST /api/users": "set_user_access", "POST /api/members": "set_project_member",
		"POST /api/move": "move_notes", "POST /api/duplicate": "duplicate_notes", "POST /api/metadata": "set_note_properties",
		"GET /api/locks": "note_locks", "POST /api/locks": "set_note_lock",
		"GET /api/comments": "list_comments", "POST /api/comments": "add_comment", "POST /api/comments/resolve": "resolve_comment", "POST /api/comments/delete": "delete_comment",
		"GET /api/browse": "browse_folder", "GET /api/notes": "search_notes", "GET /api/note": "read_note", "POST /api/note": "save_note", "GET /api/history": "note_history",
		// A view runs only in the reader's browser; MCP clients read its source in the note.
		"GET /api/view": "interactive sandboxed view", "POST /api/view/preview": "interactive sandboxed view",
		"POST /api/preview": "preview_markdown", "GET /api/status": "project_status", "POST /api/upload": "upload_attachment", "GET /api/backup": "export_project_backup",
		"GET /api/knowledge/status": "knowledge_status", "GET /api/knowledge/snapshot": "read_project_changes", "GET /api/knowledge/changes": "read_project_changes",
		"GET /api/knowledge/passages": "get_indexing_passages", "POST /api/knowledge/embeddings": "submit_embeddings", "POST /api/knowledge/search": "semantic_search", "POST /api/knowledge/hybrid-search": "hybrid_search", "POST /api/knowledge/config": "set_semantic_config",
		"GET /api/knowledge/evidence": "get_note_sources", "GET /api/knowledge/instructions": "project_instructions", "POST /api/knowledge/instructions": "set_project_instructions",
		"GET /api/knowledge/review": "review_inbox", "POST /api/knowledge/findings": "file_review_finding", "POST /api/knowledge/findings/decide": "review_decision",
		"POST /api/knowledge/groups": "submit_change_group", "POST /api/knowledge/proposals": "submit_change_group", "POST /api/knowledge/proposals/decide": "review_decision",
		"GET /api/knowledge/checkpoint": "consumer_checkpoint", "POST /api/knowledge/complete": "complete_workflow",
	}
	files, _ := filepath.Glob("*.go")
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			route, _ := strconv.Unquote(literal.Value)
			if !strings.HasPrefix(route, "GET /api/") && !strings.HasPrefix(route, "POST /api/") {
				return true
			}
			name, ok := parity[route]
			if !ok {
				t.Errorf("new HTTP action needs MCP parity or explicit exception: %s", route)
			} else if name != "interactive password provisioning" && name != "interactive private chat" && name != "interactive personal review" && name != "interactive private capture" && name != "interactive repository setup" && name != "interactive sandboxed view" && name != "interactive purge confirmation" && !tools[name] {
				t.Errorf("%s maps to missing tool %s", route, name)
			}
			return true
		})
	}

}

func TestMCPContentActionsAndGitParity(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner")
	client := actionClient(t, app, "alice", "notes:read notes:write")
	original, err := store.Save(ctx, "note.md", []byte("# Original\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	changed, err := store.Save(ctx, "note.md", []byte("# Changed\n"), original.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	actionCall(t, client, "restore_note", map[string]any{"project": "default", "path": "note.md", "sourceRevision": original.Revision, "revision": original.Revision}, true)
	actionCall(t, client, "restore_note", map[string]any{"project": "default", "path": "note.md", "sourceRevision": original.Revision, "revision": changed.Revision}, false)
	got, _ := store.Read(ctx, "note.md", "")
	if !bytes.Equal(got.Content, original.Content) {
		t.Fatal("restore changed wording")
	}
	history, _ := store.History(ctx, "note.md")
	if len(history) != 3 || history[0].Source != "mcp" || history[0].Client != "actions-client" {
		t.Fatal(history)
	}
	state, _ := store.Status(ctx)
	actionCall(t, client, "set_note_lock", map[string]any{"project": "default", "path": "note.md", "locked": true, "revision": state.Revision}, false)
	actionCall(t, client, "restore_note", map[string]any{"project": "default", "path": "note.md", "sourceRevision": changed.Revision, "revision": got.Revision}, true)
	state, _ = store.Status(ctx)
	actionCall(t, client, "set_note_lock", map[string]any{"project": "default", "path": "note.md", "locked": false, "revision": state.Revision}, false)
	actionCall(t, client, "set_project_instructions", map[string]any{"project": "default", "content": "Use citations.", "revision": ""}, false)
	actionCall(t, client, "set_project_instructions", map[string]any{"project": "default", "content": "stale", "revision": ""}, true)
	instructions, _ := store.Read(ctx, "AGENTS.md", "")
	if string(instructions.Content) != "Use citations." {
		t.Fatal(instructions)
	}
	initialDiscussion, _ := store.Comments(ctx, "note.md")
	comment, err := store.AddComment(ctx, "note.md", "Check evidence.", initialDiscussion.Revision, "bob", workspace.CommentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"project": "default", "path": "note.md", "id": comment.Comments[0].ID, "revision": comment.Revision}
	app.catalog.Member(catalog.Bootstrap, "default", "alice", "comment")
	actionCall(t, client, "resolve_comment", args, true)
	own, _ := store.AddComment(ctx, "note.md", "My question.", comment.Revision, "alice", workspace.CommentOptions{})
	actionCall(t, client, "resolve_comment", map[string]any{"project": "default", "path": "note.md", "id": own.Comments[1].ID, "revision": own.Revision}, false)
	app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit")
	discussion, _ := store.Comments(ctx, "note.md")
	args["revision"] = discussion.Revision
	actionCall(t, client, "resolve_comment", args, false)
	for _, name := range []string{"set_note_lock", "set_project_instructions"} {
		if name == "set_note_lock" {
			actionCall(t, client, name, map[string]any{"project": "default", "path": "note.md", "locked": true, "revision": state.Revision}, true)
		} else {
			actionCall(t, client, name, map[string]any{"project": "default", "content": "No authority", "revision": instructions.Revision}, true)
		}
	}
	html := actionCall(t, client, "preview_markdown", map[string]any{"project": "default", "content": "# Hello\n<script>alert(1)</script>"}, false)
	var rendered map[string]string
	json.Unmarshal(html, &rendered)
	if !strings.Contains(rendered["html"], "Hello") || strings.Contains(rendered["html"], "<script>") {
		t.Fatal(rendered)
	}
}

func TestMCPMediaAndCompleteBackup(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit")
	client := actionClient(t, app, "alice", "notes:read notes:write")
	data := bytes.Repeat([]byte{0, 1, 2, 255, 128}, 400)
	args := map[string]any{"project": "default", "path": "assets/test.png", "data": base64.StdEncoding.EncodeToString(data), "revision": ""}
	actionCall(t, client, "upload_attachment", args, false)
	actionCall(t, client, "upload_attachment", args, true)
	for _, path := range []string{"../outside.png", "assets/../../outside.png", "note.md", "assets/code.exe"} {
		args["path"] = path
		actionCall(t, client, "upload_attachment", args, true)
	}
	args["path"] = "assets/bad.png"
	args["data"] = "not-base64"
	actionCall(t, client, "upload_attachment", args, true)
	entries := actionCall(t, client, "list_attachments", map[string]any{"project": "default"}, false)
	if !strings.Contains(string(entries), "assets/test.png") {
		t.Fatal(string(entries))
	}
	raw := actionCall(t, client, "read_attachment", map[string]any{"project": "default", "path": "assets/test.png", "limit": 700}, false)
	var first binaryChunk
	json.Unmarshal(raw, &first)
	assembled, _ := base64.StdEncoding.DecodeString(first.Data)
	for offset := first.NextOffset; offset < first.Total; {
		raw = actionCall(t, client, "read_attachment", map[string]any{"project": "default", "path": "assets/test.png", "revision": first.Revision, "offset": offset, "limit": 700}, false)
		var part binaryChunk
		json.Unmarshal(raw, &part)
		decoded, _ := base64.StdEncoding.DecodeString(part.Data)
		assembled = append(assembled, decoded...)
		offset = part.NextOffset
	}
	if !bytes.Equal(data, assembled) || workspace.ContentHash(assembled) != first.SHA256 {
		t.Fatal("media corrupted")
	}
	actionCall(t, client, "read_attachment", map[string]any{"project": "default", "path": "assets/test.png", "offset": 1}, true)
	actionCall(t, client, "read_attachment", map[string]any{"project": "secret", "path": "assets/test.png"}, true)
	actionCall(t, client, "read_attachment", map[string]any{"project": "default", "path": "assets/../AGENTS.md"}, true)
	// Revocation applies to new tools on this already connected client.
	app.catalog.Member(catalog.Bootstrap, "default", "alice", "read")
	args["path"] = "assets/new.png"
	args["data"] = base64.StdEncoding.EncodeToString(data)
	actionCall(t, client, "upload_attachment", args, true)
	history, _ := store.History(ctx, "assets/test.png")
	if history[0].Source != "mcp" {
		t.Fatal(history)
	}
	raw = actionCall(t, client, "export_project_backup", map[string]any{"project": "default", "limit": 100}, false)
	var bundle binaryChunk
	json.Unmarshal(raw, &bundle)
	contents, _ := base64.StdEncoding.DecodeString(bundle.Data)
	for offset := bundle.NextOffset; offset < bundle.Total; {
		raw = actionCall(t, client, "export_project_backup", map[string]any{"project": "default", "offset": offset, "limit": 262144, "sha256": bundle.SHA256}, false)
		var part binaryChunk
		json.Unmarshal(raw, &part)
		decoded, _ := base64.StdEncoding.DecodeString(part.Data)
		contents = append(contents, decoded...)
		offset = part.NextOffset
	}
	if workspace.ContentHash(contents) != bundle.SHA256 {
		t.Fatal("backup checksum mismatch")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "project.bundle")
	os.WriteFile(path, contents, 0600)
	if output, err := exec.Command("git", "clone", path, filepath.Join(dir, "restored")).CombinedOutput(); err != nil {
		t.Fatal(string(output), err)
	}
	restored, err := os.ReadFile(filepath.Join(dir, "restored", "assets", "test.png"))
	if err != nil || !bytes.Equal(restored, data) {
		t.Fatal(err)
	}
	if _, err = store.Save(ctx, "later.md", []byte("Changed after export"), "", ""); err != nil {
		t.Fatal(err)
	}
	actionCall(t, client, "export_project_backup", map[string]any{"project": "default", "offset": 100, "sha256": bundle.SHA256}, true)
	files, _ := filepath.Glob(filepath.Join(app.data, "cache", "backups", "*"))
	if len(files) != 0 {
		t.Fatal("temporary backup leaked", files)
	}
}

func TestMCPSemanticFullConfiguration(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner")
	client := actionClient(t, app, "alice", "notes:read notes:write")
	config := knowledge.Config{Enabled: true, Model: "fixture-v1", Dimensions: 3, PassageRunes: 256, Splitter: "markdown-heading-v1", Endpoint: true}
	actionCall(t, client, "set_semantic_config", map[string]any{"project": "default", "config": config}, false)
	config.Enabled = false
	actionCall(t, client, "set_semantic_config", map[string]any{"project": "default", "config": config}, false)
	config.Model = "replacement"
	actionCall(t, client, "set_semantic_config", map[string]any{"project": "default", "config": config}, true)
	actionCall(t, client, "set_semantic_config", map[string]any{"project": "default", "config": config, "rebuild": true}, false)
	config.Enabled = true
	config.Endpoint = false
	actionCall(t, client, "set_semantic_config", map[string]any{"project": "default", "config": config}, false)
	e, releaseEngine, _ := app.acquireEngine(t.Context(), store)
	if releaseEngine != nil {
		t.Cleanup(releaseEngine)
	}

	state, _ := e.Pending(ctx, 1)
	if !state.Status.Config.Enabled || state.Status.Config.Endpoint || state.Status.Config.Model != "replacement" {
		t.Fatal(state)
	}
	app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit")
	actionCall(t, client, "set_semantic_config", map[string]any{"project": "default", "config": config}, true)
}

func TestMCPManagementRequiresConsentAndAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, user, scope     string
		admin, owner, allowed bool
	}{
		{"admin legacy", "alice", "notes:read notes:write", true, false, false},
		{"admin manage", "alice", "notes:read notes:manage", true, false, true},
		{"owner manage", "alice", "notes:read notes:manage", false, true, true},
		{"editor manage", "alice", "notes:read notes:write notes:manage", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, _ := fixture(t)
			role := "edit"
			if tc.owner {
				role = "owner"
			}
			app.catalog.Member(catalog.Bootstrap, "default", tc.user, role)
			if tc.admin {
				app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: tc.user, Admin: true})
			}
			client := actionClient(t, app, tc.user, tc.scope)
			actionCall(t, client, "administration", map[string]any{}, !tc.allowed)
			actionCall(t, client, "create_project", map[string]any{"id": "new-project", "name": "New project"}, !(tc.admin && tc.allowed))
			actionCall(t, client, "set_project_member", map[string]any{"project": "default", "user": "bob", "role": "read"}, !tc.allowed)
			actionCall(t, client, "set_user_access", map[string]any{"id": "bob", "admin": true, "disabled": false}, !(tc.admin && tc.allowed))
			caps := actionCall(t, client, "capabilities", map[string]any{}, false)
			var v map[string]any
			json.Unmarshal(caps, &v)
			if v["canAdministerInstance"] != (tc.admin && tc.allowed) {
				t.Fatal(v)
			}
			if tc.admin && tc.allowed {
				epoch := app.catalog.Epoch("bob")
				actionCall(t, client, "set_user_access", map[string]any{"id": "bob", "admin": false, "disabled": true}, false)
				if app.catalog.ValidSession("bob", epoch) {
					t.Fatal("disabled user retained sessions")
				}
				actionCall(t, client, "set_user_access", map[string]any{"id": "bob", "admin": false, "disabled": false}, false)
				if app.catalog.ValidSession("bob", epoch) {
					t.Fatal("re-enable revived revoked sessions")
				}
				epoch = app.catalog.Epoch("alice")
				actionCall(t, client, "set_user_access", map[string]any{"id": "alice", "admin": false, "disabled": false}, true)
				if !app.catalog.IsAdmin("alice") || app.catalog.Epoch("alice") != epoch {
					t.Fatal("rejected last-Admin demotion changed access")
				}
				actionCall(t, client, "set_user_access", map[string]any{"id": "bob", "admin": true, "disabled": false}, false)
				actionCall(t, client, "set_user_access", map[string]any{"id": "alice", "admin": false, "disabled": false}, false)
				if app.catalog.ValidSession("alice", epoch) {
					t.Fatal("administrator demotion retained the previous session")
				}
				_, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_project", Arguments: map[string]any{"id": "after-demotion", "name": "Denied"}})
				if err == nil || !strings.Contains(err.Error(), http.StatusText(http.StatusUnauthorized)) {
					t.Fatalf("demoted administrator token was not revoked: %v", err)
				}
				client = actionClient(t, app, tc.user, tc.scope)
				actionCall(t, client, "capabilities", map[string]any{}, false)
				actionCall(t, client, "create_project", map[string]any{"id": "after-demotion", "name": "Denied"}, true)
			}
			if tc.owner {
				app.catalog.Create(catalog.Bootstrap, "other", "Other")
				app.catalog.Member(catalog.Bootstrap, "other", "outsider", "read")
				actionCall(t, client, "set_project_member", map[string]any{"project": "other", "user": "alice", "role": "owner"}, true)
				raw := actionCall(t, client, "administration", map[string]any{}, false)
				if strings.Contains(string(raw), "Other") || strings.Contains(string(raw), `"id":"outsider"`) {
					t.Fatal("owner sees unrelated users/projects", string(raw))
				}
			}
		})
	}
}
