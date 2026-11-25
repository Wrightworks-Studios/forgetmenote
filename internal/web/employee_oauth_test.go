package web

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"forgetmenote/plugins"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

func TestEmployeeCompanionOAuthPermissions(t *testing.T) {
	app, store := fixture(t)
	ts := httptest.NewServer(app)
	defer ts.Close()
	app.origin = ts.URL
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	admin := recoverySession(t, app)
	user := addLocal(t, app, admin, "employee")
	other := addLocal(t, app, admin, "colleague")
	g, err := app.catalog.SetGroup(catalog.Bootstrap, catalog.Group{ID: "employees", Name: "Employees", Members: []string{user}})
	must(err)
	must(app.catalog.SetProjectGroup(catalog.Bootstrap, "default", g.ID, "read"))
	_, err = store.Save(context.Background(), "Knowledge.md", []byte("Synthetic company evidence"), "", "test")
	must(err)
	must(app.catalog.Create(catalog.Bootstrap, "profiles", "Shared profiles"))
	profile, err := app.catalog.Access(catalog.Bootstrap, "profiles", "edit")
	must(err)
	_, err = profile.Save(context.Background(), "Profile.md", []byte("Use the permitted metrics read operation."), "", "test")
	must(err)
	must(app.catalog.SetProjectGroup(catalog.Bootstrap, "profiles", g.ID, "read"))
	must(app.catalog.ProvisionWorkNotebook(catalog.Bootstrap, user, "work", "My work"))
	must(app.catalog.ProvisionWorkNotebook(catalog.Bootstrap, other, "other-work", "Other private work"))
	must(app.catalog.CreateKind(catalog.Bootstrap, "source", "Source", "repository"))
	must(app.catalog.Member(catalog.Bootstrap, "source", user, "owner"))
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"value":7}`))
	}))
	defer upstream.Close()
	manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "metrics", Kind: "http", Endpoint: upstream.URL, Operations: []connectors.Operation{{Name: "read", Method: "GET", Path: "/", ReadOnly: true}, {Name: "hidden", Method: "GET", Path: "/", ReadOnly: true}, {Name: "write", Method: "POST", Path: "/"}}}}})
	must(err)
	defer manager.Close()
	app.dataConnections = manager
	_, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "metrics", Groups: map[string][]string{g.ID: {"read", "write"}}})
	must(err)
	w := oidcRequest(app, "POST", "/auth/password/login", url.Values{"username": {"employee"}, "password": {"test-password-long"}}.Encode())
	if w.Code != 303 {
		t.Fatalf("login %d %s", w.Code, w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	page := oidcRequest(app, "GET", "/account/connect", "", cookie)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "My work") || strings.Contains(page.Body.String(), "Other private work") {
		t.Fatal("incorrect access page", page.Code, page.Body.String())
	}
	if w = localRequest(app, "/api/organization/action/provision_work_notebook", `{"id":"forbidden","name":"Forbidden","user":"`+user+`"}`, cookie); w.Code != 403 {
		t.Fatal("employee reached admin provisioning", w.Code, w.Body.String())
	}
	// Real DCR, browser consent, S256 PKCE and token exchange; no injected grant.
	w = localRequest(app, "/oauth/register", `{"client_name":"Employee companion","redirect_uris":["http://127.0.0.1:54321/callback"],"token_endpoint_auth_method":"none"}`)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var registration struct {
		ID string `json:"client_id"`
	}
	must(json.Unmarshal(w.Body.Bytes(), &registration))
	verifier := oauth2.GenerateVerifier()
	q := url.Values{"client_id": {registration.ID}, "redirect_uri": {"http://127.0.0.1:54321/callback"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {oauth2.S256ChallengeFromVerifier(verifier)}, "resource": {app.origin + "/mcp"}, "scope": {plugins.EmployeeScopes}}
	w = oidcRequest(app, "GET", "/oauth/authorize?"+q.Encode(), "", cookie)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	fields := url.Values{}
	for _, key := range []string{"request", "signature"} {
		m := regexp.MustCompile(`name="` + key + `" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
		if len(m) != 2 {
			t.Fatal("missing consent", w.Body.String())
		}
		fields.Set(key, html.UnescapeString(m[1]))
	}
	w = oidcRequest(app, "POST", "/oauth/authorize", fields.Encode(), cookie)
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	callback, err := url.Parse(w.Header().Get("Location"))
	must(err)
	w = oidcRequest(app, "POST", "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {registration.ID}, "redirect_uri": q["redirect_uri"], "code": {callback.Query().Get("code")}, "code_verifier": {verifier}, "resource": q["resource"]}.Encode())
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var tokens struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Scope   string `json:"scope"`
	}
	must(json.Unmarshal(w.Body.Bytes(), &tokens))
	if tokens.Scope != plugins.EmployeeScopes {
		t.Fatal("unexpected employee scope", tokens.Scope)
	}
	// Asking for broader scopes during refresh cannot change the original consent.
	w = oidcRequest(app, "POST", "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {registration.ID}, "refresh_token": {tokens.Refresh}, "scope": {"notes:read notes:write notes:manage data:read data:write"}}.Encode())
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	must(json.Unmarshal(w.Body.Bytes(), &tokens))
	if tokens.Scope != plugins.EmployeeScopes {
		t.Fatal("refresh expanded scope", tokens.Scope)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "employee-proof", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: app.origin + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: tokens.Access}}}, nil)
	must(err)
	defer session.Close()
	if !strings.Contains(session.InitializeResult().Instructions, plugins.Instructions()) {
		t.Fatal("direct MCP clients did not receive the canonical workflow")
	}
	callTool := func(name string, args any, denied bool) json.RawMessage {
		t.Helper()
		return actionCall(t, session, name, args, denied)
	}
	cap := callTool("capabilities", map[string]any{}, false)
	if !strings.Contains(string(cap), app.origin) || !strings.Contains(string(cap), `"dataReadConsent":true`) {
		t.Fatal("missing identity/consent", string(cap))
	}
	projects := callTool("list_projects", map[string]any{}, false)
	if strings.Contains(string(projects), "other-work") {
		t.Fatal("private notebook discovered")
	}
	for _, book := range []struct{ project, path string }{{"default", "Knowledge.md"}, {"profiles", "Profile.md"}} {
		callTool("read_note", map[string]any{"project": book.project, "path": book.path}, false)
		callTool("save_note", map[string]any{"project": book.project, "path": book.path, "content": "forbidden", "revision": ""}, true)
	}
	callTool("read_note", map[string]any{"project": "other-work", "path": "Any.md"}, true)
	callTool("save_note", map[string]any{"project": "source", "path": "README.md", "content": "forbidden", "revision": ""}, true)
	args := map[string]any{"project": "work", "path": "Analysis.md", "content": "First analysis", "revision": ""}
	callTool("save_note", args, false)
	var note struct{ Revision string }
	must(json.Unmarshal(callTool("read_note", map[string]any{"project": "work", "path": "Analysis.md"}, false), &note))
	args["revision"] = note.Revision
	args["content"] = "Updated analysis"
	callTool("save_note", args, false)
	args["content"] = "Stale analysis"
	callTool("save_note", args, true)
	work, err := app.catalog.Access(user, "work", "edit")
	must(err)
	locks, err := work.Locks(context.Background(), "Analysis.md")
	must(err)
	_, err = work.SetLock(context.Background(), "Analysis.md", true, locks.Revision, "test")
	must(err)
	must(json.Unmarshal(callTool("read_note", map[string]any{"project": "work", "path": "Analysis.md"}, false), &note))
	args["revision"] = note.Revision
	callTool("save_note", args, true)
	connections := callTool("list_data_connections", map[string]any{}, false)
	if !strings.Contains(string(connections), "metrics") {
		t.Fatal(string(connections))
	}
	ops := callTool("list_data_operations", map[string]any{"connection": "metrics"}, false)
	if strings.Contains(string(ops), `"name":"hidden"`) || strings.Contains(string(ops), `"name":"write"`) {
		t.Fatal("ungranted operations discovered", string(ops))
	}
	result := callTool("call_data_operation", map[string]any{"connection": "metrics", "operation": "read", "arguments": map[string]any{}}, false)
	if !strings.Contains(string(result), `"value":7`) || !strings.Contains(string(result), `"complete":true`) {
		t.Fatal("missing result/receipt", string(result))
	}
	for _, op := range []string{"hidden", "write"} {
		callTool("call_data_operation", map[string]any{"connection": "metrics", "operation": op, "arguments": map[string]any{}}, true)
	}
	callTool("provision_work_notebook", map[string]any{"user": user, "id": "denied", "name": "Denied"}, true)
	callTool("organization_status", map[string]any{}, true)
	g.Members = nil
	_, err = app.catalog.SetGroup(catalog.Bootstrap, g)
	must(err)
	callTool("read_note", map[string]any{"project": "default", "path": "Knowledge.md"}, true)
	callTool("call_data_operation", map[string]any{"connection": "metrics", "operation": "read", "arguments": map[string]any{}}, true)
	if requests.Load() != 1 {
		t.Fatal("unpermitted upstream requests", requests.Load())
	}
	must(app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: user, Disabled: true}))
	_, err = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "capabilities", Arguments: map[string]any{}})
	if err == nil {
		t.Fatal("disabled account reused issued token")
	}
}
