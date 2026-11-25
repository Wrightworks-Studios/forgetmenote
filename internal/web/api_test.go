package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/catalog"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAPICredentialRechecksMembershipAccountAndExpiry(t *testing.T) {
	app, _ := fixture(t)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	grant, err := issueCredential(app.catalog, app.oauth, "alice", CredentialSpec{Name: "editor", Projects: []string{"default"}, Scopes: []string{"notes:read", "notes:write"}, Audiences: []string{"api"}, Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	if w := apiCall(app, "POST", "/api/v1/actions/save_note", `{"project":"default","path":"before.md","content":"accepted","revision":""}`, grant.Token); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	if w := apiCall(app, "POST", "/api/v1/actions/save_note", `{"project":"default","path":"after.md","content":"denied","revision":""}`, grant.Token); w.Code != 403 {
		t.Fatal("role loss did not revoke writes", w.Code, w.Body.String())
	}
	if w := apiCall(app, "POST", "/api/v1/actions/read_note", `{"project":"default","path":"before.md","revision":""}`, grant.Token); w.Code != 200 {
		t.Fatal("read membership was lost", w.Code)
	}
	if err = app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "alice", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if w := apiCall(app, "GET", "/api/v1/capabilities", "", grant.Token); w.Code != 401 {
		t.Fatal("disabled account authenticated", w.Code)
	}
	if err = app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if w := apiCall(app, "GET", "/api/v1/capabilities", "", grant.Token); w.Code != 401 {
		t.Fatal("re-enabled account revived an old credential", w.Code)
	}
	grant, err = issueCredential(app.catalog, app.oauth, "alice", grant.CredentialSpec)
	if err != nil {
		t.Fatal(err)
	}
	app.oauth.Mu.Lock()
	expired := app.oauth.Credentials[digest(grant.Token)]
	expired.Expires = time.Now().Add(-time.Second).Unix()
	app.oauth.Credentials[digest(grant.Token)] = expired
	app.oauth.Mu.Unlock()
	if w := apiCall(app, "GET", "/api/v1/capabilities", "", grant.Token); w.Code != 401 {
		t.Fatal("expired credential authenticated", w.Code)
	}
}

func apiCall(app *Server, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, app.origin+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	return w
}

func TestCredentialSnapshotsCannotChangeNotebookRestrictions(t *testing.T) {
	app, _ := fixture(t)
	if err := app.catalog.Create(catalog.Bootstrap, "hidden", "Hidden"); err != nil {
		t.Fatal(err)
	}
	spec := CredentialSpec{Name: "reader", Projects: []string{"default"}, Scopes: []string{"notes:read"}, Audiences: []string{"api"}, Days: 1}
	grant, err := issueCredential(app.catalog, app.oauth, catalog.Bootstrap, spec)
	if err != nil {
		t.Fatal(err)
	}
	// Neither the submitted slices nor returned metadata may mutate authority.
	spec.Projects[0] = "hidden"
	if w := apiCall(app, "POST", "/api/v1/actions/list_documents", `{"project":"hidden"}`, grant.Token); w.Code != 403 {
		t.Fatalf("caller mutated stored notebook scope: %d %s", w.Code, w.Body)
	}
	grant.Scopes[0] = "notes:write"
	if w := apiCall(app, "POST", "/api/v1/actions/save_note", `{"project":"default","path":"forged.md","content":"no","revision":""}`, grant.Token); w.Code != 403 {
		t.Fatalf("returned credential mutated stored permissions: %d %s", w.Code, w.Body)
	}
	metadata, ok := app.oauth.Credential(digest(grant.Token))
	if !ok {
		t.Fatal("credential missing")
	}
	metadata.Projects[0] = "hidden"
	if w := apiCall(app, "POST", "/api/v1/actions/list_documents", `{"project":"default"}`, grant.Token); w.Code != 200 {
		t.Fatalf("snapshot mutated original notebook permission: %d %s", w.Code, w.Body)
	}
}

func TestAPICredentialMCPResourcesRespectNotebookScope(t *testing.T) {
	app, _ := fixture(t)
	if err := app.catalog.Create("@local-admin", "hidden", "Hidden notebook"); err != nil {
		t.Fatal(err)
	}
	grant, err := issueCredential(app.catalog, app.oauth, "@local-admin", CredentialSpec{Name: "MCP", Projects: []string{"default"}, Scopes: []string{"notes:read"}, Audiences: []string{"mcp"}, Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app)
	defer ts.Close()
	app.origin = ts.URL
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	client.AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, r mcp.Request) (mcp.Result, error) {
			if method == "server/discover" {
				return nil, &jsonrpc.Error{Code: -32601, Message: "legacy subscription fixture"}
			}
			return next(ctx, method, r)
		}
	})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: grant.Token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	list, err := session.ListResources(ctx, &mcp.ListResourcesParams{})
	if err != nil || len(list.Resources) != 1 || list.Resources[0].URI != revisionURI("default") {
		t.Fatalf("scoped resource list: %+v %v", list, err)
	}
	if _, err = session.ReadResource(ctx, &mcp.ReadResourceParams{URI: revisionURI("hidden")}); err == nil {
		t.Fatal("read outside credential")
	}
	if err = session.Subscribe(ctx, &mcp.SubscribeParams{URI: revisionURI("default")}); err != nil {
		t.Fatal(err)
	}
	if err = session.Subscribe(ctx, &mcp.SubscribeParams{URI: revisionURI("hidden")}); err == nil {
		t.Fatal("subscription outside credential")
	}
	w := apiCall(app, "GET", "/api/v1/capabilities", "", grant.Token)
	if w.Code != 401 {
		t.Fatalf("MCP-only credential accepted by API: %d", w.Code)
	}
}

func TestAPIUsesSharedSaveAndConflicts(t *testing.T) {
	app, store := fixture(t)
	saved := apiCall(app, "POST", "/api/v1/actions/save_note", `{"project":"default","path":"notes/api.md","content":"# API","revision":""}`, testToken)
	if saved.Code != 200 {
		t.Fatalf("save: %d %s", saved.Code, saved.Body.String())
	}
	var doc noteOutput
	if err := json.Unmarshal(saved.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	got, err := store.Read(context.Background(), "notes/api.md", "")
	if err != nil || string(got.Content) != "# API" {
		t.Fatalf("shared storage: %s %v", got.Content, err)
	}
	stale := apiCall(app, "POST", "/api/v1/actions/save_note", `{"project":"default","path":"notes/api.md","content":"overwrite","revision":""}`, testToken)
	if stale.Code != 409 || !strings.Contains(stale.Body.String(), `"code":"conflict"`) {
		t.Fatalf("conflict: %d %s", stale.Code, stale.Body.String())
	}
	schemas := apiCall(app, "GET", "/api/v1/actions", "", testToken)
	if schemas.Code != 200 || !strings.Contains(schemas.Body.String(), `"save_note"`) || !strings.Contains(schemas.Body.String(), `"outputSchema"`) {
		t.Fatalf("schemas: %d %s", schemas.Code, schemas.Body.String())
	}
	private := apiCall(app, "GET", "/api/v1/capabilities", "", "")
	if private.Code != 401 {
		t.Fatalf("private API: %d", private.Code)
	}
}

func TestAPIReadCredentialDiscoveryMatchesCapabilities(t *testing.T) {
	app, _ := fixture(t)
	grant, err := issueCredential(app.catalog, app.oauth, "@local-admin", CredentialSpec{Name: "reader", Projects: []string{"default"}, Scopes: []string{"notes:read"}, Audiences: []string{"api"}, Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	w := apiCall(app, "GET", "/api/v1/capabilities", "", grant.Token)
	var result struct {
		Projects []struct{ Capabilities map[string]bool }
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Projects) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(result.Projects[0].Capabilities) == 0 {
		t.Fatal("API capability discovery omitted notebook capabilities")
	}
	for name, allowed := range result.Projects[0].Capabilities {
		if allowed != (name == "read") {
			t.Fatalf("read credential advertises %s=%v", name, allowed)
		}
	}
	w = apiCall(app, "GET", "/api/v1/actions", "", grant.Token)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"name":"save_note"`) || strings.Contains(w.Body.String(), `"name":"refresh_repository_source"`) || strings.Contains(w.Body.String(), `"name":"managed_backup_status"`) {
		t.Fatal("discovery exceeds scope", w.Code)
	}
	for i := 0; i < cap(app.rpcReadWork); i++ {
		app.rpcReadWork <- struct{}{}
	}
	w = apiCall(app, "POST", "/api/v1/actions/list_projects", `{}`, grant.Token)
	if w.Code != 429 || !strings.Contains(w.Body.String(), `"code":"capacity"`) {
		t.Fatal("capacity error is not structured", w.Code, w.Body.String())
	}
}

func TestCredentialCanRevokeItselfWithoutManagementScope(t *testing.T) {
	app, _ := fixture(t)
	grant, err := issueCredential(app.catalog, app.oauth, "@local-admin", CredentialSpec{Name: "read", Projects: []string{"default"}, Scopes: []string{"notes:read"}, Audiences: []string{"api"}, Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", app.origin+"/oauth/revoke", strings.NewReader("token="+grant.Token))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = apiCall(app, "GET", "/api/v1/capabilities", "", grant.Token); w.Code != 401 {
		t.Fatalf("credential survives logout: %d", w.Code)
	}
}

func TestAPICredentialNotebookScopesRevocationAndAudience(t *testing.T) {
	app, _ := fixture(t)
	if err := app.catalog.Create("@local-admin", "hidden", "Hidden notebook"); err != nil {
		t.Fatal(err)
	}
	issue := apiCall(app, "POST", "/api/v1/actions/create_api_credential", `{"name":"test client","projects":["default"],"scopes":["notes:read","notes:write"],"audiences":["api"],"days":30}`, testToken)
	if issue.Code != 200 {
		t.Fatalf("issue: %d %s", issue.Code, issue.Body.String())
	}
	var issued struct {
		Token string `json:"token"`
		ID    string `json:"id"`
	}
	if err := json.Unmarshal(issue.Body.Bytes(), &issued); err != nil || issued.Token == "" {
		t.Fatalf("credential response: %s %v", issue.Body.String(), err)
	}
	list := apiCall(app, "POST", "/api/v1/actions/list_projects", `{}`, issued.Token)
	if list.Code != 200 || strings.Contains(list.Body.String(), "Hidden") || !strings.Contains(list.Body.String(), "default") {
		t.Fatalf("list scope: %d %s", list.Code, list.Body.String())
	}
	for _, action := range []string{"read_note", "save_note"} {
		args := `{"project":"hidden","path":"secret.md","revision":""}`
		if action == "save_note" {
			args = `{"project":"hidden","path":"secret.md","content":"bad","revision":""}`
		}
		w := apiCall(app, "POST", "/api/v1/actions/"+action, args, issued.Token)
		if w.Code != 403 {
			t.Fatalf("%s outside grant: %d %s", action, w.Code, w.Body.String())
		}
	}
	mcp := apiCall(app, "POST", "/mcp", `{}`, issued.Token)
	if mcp.Code != 401 {
		t.Fatalf("API token accepted by MCP: %d", mcp.Code)
	}
	w := apiCall(app, "POST", "/api/v1/actions/create_project", `{"id":"escaped","name":"Escaped"}`, issued.Token)
	if w.Code != 403 {
		t.Fatalf("credential escalated: %d %s", w.Code, w.Body.String())
	}
	w = apiCall(app, "POST", "/api/v1/actions/revoke_api_credential", `{"id":"`+issued.ID+`"}`, testToken)
	if w.Code != 200 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	w = apiCall(app, "GET", "/api/v1/capabilities", "", issued.Token)
	if w.Code != 401 {
		t.Fatalf("revoked token: %d", w.Code)
	}
}
