package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/client"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/workspace"
)

func TestOrganizationGroupAccessAndRevocation(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	id := addLocal(t, app, admin, "group-reader")
	w := localRequest(app, "/api/organization/action/set_group", fmt.Sprintf(`{"id":"engineering","name":"Engineering","members":[%q],"revision":0}`, id), admin)
	if w.Code != 200 {
		t.Fatalf("create group: %d %s", w.Code, w.Body.String())
	}
	var group catalog.Group
	if err := json.Unmarshal(w.Body.Bytes(), &group); err != nil {
		t.Fatal(err)
	}
	w = localRequest(app, "/api/organization/action/set_project_group", `{"project":"default","group":"engineering","role":"edit"}`, admin)
	if w.Code != 200 {
		t.Fatalf("share with group: %d %s", w.Code, w.Body.String())
	}
	login := oidcRequest(app, "POST", "/auth/password/login", url.Values{"username": {"group-reader"}, "password": {"test-password-long"}}.Encode())
	if login.Code != 303 {
		t.Fatal(login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	if w = oidcRequest(app, "GET", "/p/default/api/notes", "", cookie); w.Code != 200 {
		t.Fatalf("inherited access: %d %s", w.Code, w.Body.String())
	}
	if w = localRequest(app, "/api/organization/action/set_group", fmt.Sprintf(`{"id":"engineering","name":"Engineering","members":[],"revision":%d}`, group.Revision), admin); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = oidcRequest(app, "GET", "/p/default/api/notes", "", cookie); w.Code != 403 {
		t.Fatalf("revoked group read: %d", w.Code)
	}
}

func TestOrganizationInvitationCreatesAccountOnce(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	w := localRequest(app, "/api/organization/action/create_invitation", `{"name":"New colleague","projects":{"default":"read"},"groups":[]}`, admin)
	if w.Code != 200 {
		t.Fatalf("invite: %d %s", w.Code, w.Body.String())
	}
	var invitation struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &invitation); err != nil || invitation.Token == "" {
		t.Fatal("no token", err)
	}
	body := fmt.Sprintf(`{"token":%q,"username":"new-colleague","password":"a-long-test-password"}`, invitation.Token)
	w = localRequest(app, "/auth/invitation/accept", body)
	if w.Code != 200 {
		t.Fatalf("accept: %d %s", w.Code, w.Body.String())
	}
	if w = localRequest(app, "/auth/invitation/accept", body); w.Code != 403 {
		t.Fatalf("replay: %d %s", w.Code, w.Body.String())
	}
}

func TestOrganizationInvitationAcceptsFirstTrustedProxyVisit(t *testing.T) {
	app, _ := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	invite, err := app.catalog.CreateInvitation(catalog.Bootstrap, catalog.InvitationSpec{Name: "New proxy person", Projects: map[string]string{"default": "read"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"token":%q}`, invite.Token)
	r := httptest.NewRequest("POST", app.origin+"/auth/invitation/accept", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Remote-User", "new-proxy-person")
	r.Header.Set("Origin", app.origin)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unverified proxy identity accepted: %d %s", w.Code, w.Body.String())
	}
	w = proxyCall(app, "new-proxy-person", "POST", "/auth/invitation/accept", body)
	if w.Code != http.StatusOK {
		t.Fatalf("first trusted visit: %d %s", w.Code, w.Body.String())
	}
	if _, err = app.catalog.Access("new-proxy-person", "default", "read"); err != nil {
		t.Fatal("invited proxy user cannot read notebook", err)
	}
	if _, err = app.catalog.Access("new-proxy-person", "default", "comment"); !errors.Is(err, workspace.ErrForbidden) || app.catalog.IsAdmin("new-proxy-person") {
		t.Fatal("invitation did not preserve its read-only grant")
	}
	if w = proxyCall(app, "new-proxy-person", "POST", "/auth/invitation/accept", body); w.Code != http.StatusForbidden {
		t.Fatalf("proxy invitation replay: %d %s", w.Code, w.Body.String())
	}
}

func TestOrganizationServiceCredentialsAndAdminBoundaries(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	steward := addLocal(t, app, admin, "steward")
	request := func(action, body string) *httptest.ResponseRecorder {
		return localRequest(app, "/api/organization/action/"+action, body, admin)
	}
	w := request("create_service_account", fmt.Sprintf(`{"name":"Build bot","steward":%q}`, steward))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var bot catalog.User
	json.Unmarshal(w.Body.Bytes(), &bot)
	if w = localRequest(app, "/api/members", fmt.Sprintf(`{"project":"default","user":%q,"role":"edit"}`, bot.ID), admin); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	body := fmt.Sprintf(`{"user":%q,"name":"Build token","projects":["default"],"scopes":["notes:read","notes:write","notes:manage"],"audiences":["api","mcp"],"days":1}`, bot.ID)
	w = request("issue_service_credential", body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var credential issuedCredential
	json.Unmarshal(w.Body.Bytes(), &credential)
	for name, body := range map[string]string{
		"organization_status":    `{}`,
		"create_service_account": fmt.Sprintf(`{"name":"Unapproved bot","steward":%q}`, steward),
		"create_api_credential":  `{"name":"Unapproved token","projects":["default"],"scopes":["notes:read"],"audiences":["api"],"days":1}`,
		"set_user_access":        fmt.Sprintf(`{"id":%q,"admin":true}`, bot.ID),
	} {
		w = apiCall(app, "POST", "/api/v1/actions/"+name, body, credential.Token)
		if w.Code != 403 {
			t.Fatal("expected permission denial", name, w.Code, w.Body.String())
		}
	}
	if w = request("issue_service_credential", fmt.Sprintf(`{"user":%q,"name":"Unbounded token","allProjects":true,"projects":[],"scopes":["notes:read"],"audiences":["api"],"days":1}`, bot.ID)); w.Code != 403 {
		t.Fatal("service credential must have explicit notebooks", w.Code, w.Body.String())
	}
	w = apiCall(app, "POST", "/api/v1/actions/list_projects", `{}`, credential.Token)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"default"`) {
		t.Fatal("bot cannot list granted notebook", w.Code, w.Body.String())
	}
	if err := app.catalog.Create(catalog.Bootstrap, "other", "Other"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "other", bot.ID, "read"); err != nil {
		t.Fatal(err)
	}
	w = apiCall(app, "POST", "/api/v1/actions/list_documents", `{"project":"other"}`, credential.Token)
	if w.Code != 403 {
		t.Fatal("token escaped notebook restriction", w.Code, w.Body.String())
	}
	server := httptest.NewServer(app)
	defer server.Close()
	app.origin = server.URL
	ctx := t.Context()
	mc := mcp.NewClient(&mcp.Implementation{Name: "automation-test", Version: "1"}, nil)
	session, err := mc.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: credential.Token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_projects", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatal("bot MCP failed", err, result)
	}
	w = localRequest(app, "/api/users", fmt.Sprintf(`{"id":%q,"disabled":true}`, bot.ID), admin)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = apiCall(app, "POST", "/api/v1/actions/list_projects", `{}`, credential.Token); w.Code != 401 {
		t.Fatal("disabled bot token accepted", w.Code)
	}
	// The unexpired credential remains invalid after re-enabling the account.
	w = localRequest(app, "/api/users", fmt.Sprintf(`{"id":%q,"disabled":false}`, bot.ID), admin)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = apiCall(app, "POST", "/api/v1/actions/list_projects", `{}`, credential.Token); w.Code != 401 {
		t.Fatal("old bot token revived", w.Code)
	}
}

func TestOrganizationCLIUsesSharedActions(t *testing.T) {
	app, _ := fixture(t)
	server := httptest.NewServer(app)
	defer server.Close()
	app.origin = server.URL
	t.Setenv("FORGETMENOTE_SERVER", server.URL)
	t.Setenv("FORGETMENOTE_TOKEN", testToken)
	t.Setenv("FORGETMENOTE_CONFIG_DIR", t.TempDir())
	var out, diagnostics bytes.Buffer
	if err := client.Run(t.Context(), []string{"organization", "status", "--json"}, strings.NewReader(""), &out, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"organization"`) {
		t.Fatal(out.String())
	}
	var status map[string]any
	if err := json.Unmarshal(out.Bytes(), &status); err != nil || status["chatConfigured"] != false {
		t.Fatal("unconfigured chat reported as configured", out.String(), err)
	}
	out.Reset()
	if err := client.Run(t.Context(), []string{"organization", "group", "--file", "-", "--json"}, strings.NewReader(`{"id":"cli-team","name":"CLI team","members":[],"revision":0}`), &out, &diagnostics); err != nil {
		t.Fatal(err)
	}
	org, err := app.catalog.Organization(catalog.Bootstrap)
	if err != nil || len(org.Groups) != 1 || org.Groups[0].ID != "cli-team" {
		t.Fatal(org, err)
	}
	out.Reset()
	if err := client.Run(t.Context(), []string{"organization", "invite", "--file", "-", "--json"}, strings.NewReader(`{"name":"CLI colleague","groups":["cli-team"],"projects":{"default":"read"}}`), &out, &diagnostics); err != nil {
		t.Fatal(err)
	}
	var invitation struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(out.Bytes(), &invitation); err != nil || invitation.Token == "" {
		t.Fatal("CLI invitation missing", out.String(), err)
	}
	if _, err = app.catalog.AcceptInvitation(invitation.Token, "", "cli-person", "cli-person-password", true); err != nil {
		t.Fatal(err)
	}
}

func TestOrganizationInvitationsHaveIndependentRateLimits(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	for i := range 11 {
		w := localRequest(app, "/api/organization/action/create_invitation", `{"name":"Colleague","projects":{},"groups":[]}`, admin)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var invite struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &invite); err != nil {
			t.Fatal(err)
		}
		w = localRequest(app, "/auth/invitation/inspect", fmt.Sprintf(`{"token":%q}`, invite.Token))
		if w.Code != 200 {
			t.Fatalf("invitation %d was throttled by unrelated invites: %d %s", i, w.Code, w.Body.String())
		}
	}
}

func TestOrganizationServiceCredentialIssuanceRechecksRevocation(t *testing.T) {
	app, _ := fixture(t)
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "issuer", Admin: true}); err != nil {
		t.Fatal(err)
	}
	bot, err := app.catalog.CreateService("issuer", "Bot", "issuer")
	if err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member("issuer", "default", bot.ID, "read"); err != nil {
		t.Fatal(err)
	}
	app.oauth.Mu.Lock()
	locked := true
	defer func() {
		if locked {
			app.oauth.Mu.Unlock()
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, e := issueCredentialFor(app.catalog, app.oauth, "issuer", bot.ID, CredentialSpec{Name: "Bot", Projects: []string{"default"}, Scopes: []string{"notes:read"}, Audiences: []string{"api"}, Days: 1})
		done <- e
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		org, e := app.catalog.Organization(catalog.Bootstrap)
		if e != nil {
			t.Fatal(e)
		}
		if len(org.Activity) > 0 && org.Activity[0].Action == "credential.issue" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("issuance did not reach credential store")
		}
		time.Sleep(time.Millisecond)
	}
	if err = app.catalog.Revoke(catalog.Bootstrap, "issuer"); err != nil {
		t.Fatal(err)
	}
	app.oauth.Mu.Unlock()
	locked = false
	if err = <-done; !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("revoked issuer minted a service credential", err)
	}
	app.oauth.Mu.Lock()
	defer app.oauth.Mu.Unlock()
	if len(app.oauth.Credentials) != 0 {
		t.Fatal("revoked issuance left a credential")
	}
}
