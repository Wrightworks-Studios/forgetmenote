package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/modules"
)

// A browser session manages its own API credentials through the same typed
// actions as MCP and API callers, with the session's own checks: the secret
// is answered once, the list never carries it, and only its owner revokes it.
func TestOwnCredentialsFromTheBrowser(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	writerID := addLocal(t, app, admin, "credential-writer")
	addLocal(t, app, admin, "credential-other")
	if err := app.catalog.Member(catalog.Bootstrap, "default", writerID, "edit"); err != nil {
		t.Fatal(err)
	}
	writer := passwordSession(t, app, "credential-writer")
	other := passwordSession(t, app, "credential-other")
	spec := `{"name":"Laptop script","projects":["default"],"scopes":["notes:read"],"audiences":["api"],"days":7}`

	cross := httptest.NewRequest("POST", app.origin+"/integrations/actions/create_api_credential", strings.NewReader(spec))
	cross.Header.Set("Origin", "https://evil.example")
	cross.Header.Set("Content-Type", "application/json")
	cross.AddCookie(writer)
	w := httptest.NewRecorder()
	serveMCPTest(app, w, cross)
	if w.Code != 403 {
		t.Fatalf("a cross-site page minted a credential: %d %s", w.Code, w.Body.String())
	}
	if w := localRequest(app, "/integrations/actions/create_api_credential", `{"name":"Wide","projects":["default"],"scopes":["notes:write"],"audiences":["chat"],"days":7}`, writer); w.Code != 400 {
		t.Fatalf("an unknown audience was accepted: %d %s", w.Code, w.Body.String())
	}

	w = localRequest(app, "/integrations/actions/create_api_credential", spec, writer)
	var issued issuedCredential
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &issued) != nil || !strings.HasPrefix(issued.Token, "fmn_") || issued.Subject != writerID {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if w := apiCall(app, "POST", "/api/v1/actions/list_projects", `{}`, issued.Token); w.Code != 200 {
		t.Fatalf("the new credential did not work: %d %s", w.Code, w.Body.String())
	}

	w = localRequest(app, "/integrations/actions/list_api_credentials", `{}`, writer)
	var listed []APICredential
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listed) != nil || len(listed) != 1 || listed[0].ID != issued.ID || listed[0].Name != "Laptop script" {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), issued.Token) || strings.Contains(w.Body.String(), `"token"`) {
		t.Fatal("the list disclosed the secret")
	}
	if w := localRequest(app, "/integrations/actions/list_api_credentials", `{}`, other); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("another person saw the credential: %d %s", w.Code, w.Body.String())
	}

	revoke := `{"id":"` + issued.ID + `"}`
	if w := localRequest(app, "/integrations/actions/revoke_api_credential", revoke, other); w.Code != 403 {
		t.Fatalf("another person revoked the credential: %d %s", w.Code, w.Body.String())
	}
	if w := apiCall(app, "POST", "/api/v1/actions/list_projects", `{}`, issued.Token); w.Code != 200 {
		t.Fatalf("a refused revocation stopped the credential: %d %s", w.Code, w.Body.String())
	}
	if w := localRequest(app, "/integrations/actions/revoke_api_credential", revoke, writer); w.Code != 200 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	if w := apiCall(app, "POST", "/api/v1/actions/list_projects", `{}`, issued.Token); w.Code != 401 {
		t.Fatalf("a revoked credential still worked: %d %s", w.Code, w.Body.String())
	}
}

// The Administration page imports a repository from the App's installation
// through import_repository itself: only an instance Admin, only with sharing
// consent, and only while the repositories module runs.
func TestRepositoryImportFromTheBrowser(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	addLocal(t, app, admin, "import-member")
	member := passwordSession(t, app, "import-member")
	in := `{"id":"source","name":"Source","installation":7,"repository":42,"shareWithNotebook":true,"externalProcessing":false}`
	if w := localRequest(app, "/integrations/actions/import_repository", in, member); w.Code != 403 {
		t.Fatalf("a member imported a repository: %d %s", w.Code, w.Body.String())
	}
	if w := localRequest(app, "/integrations/actions/import_repository", strings.Replace(in, `"shareWithNotebook":true`, `"shareWithNotebook":false`, 1), admin); w.Code != 403 {
		t.Fatalf("an import without sharing consent: %d %s", w.Code, w.Body.String())
	}
	// No App is configured here: the action itself answers that.
	if w := localRequest(app, "/integrations/actions/import_repository", in, admin); w.Code != 503 {
		t.Fatalf("an import without an App: %d %s", w.Code, w.Body.String())
	}

	set, err := modules.Parse([]string{"repositories"})
	if err != nil {
		t.Fatal(err)
	}
	off, _ := fixtureWithModules(t, set)
	if w := localRequest(off, "/integrations/actions/import_repository", in, recoverySession(t, off)); w.Code != 404 {
		t.Fatalf("an import with the repositories module off: %d %s", w.Code, w.Body.String())
	}
}
