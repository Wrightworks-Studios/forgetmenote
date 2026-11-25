package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/connectors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
)

func TestPersonalAssistantProvisioningAndIsolation(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	alice := addLocal(t, app, admin, "assistant-alice")
	bob := addLocal(t, app, admin, "assistant-bob")
	login := oidcRequest(app, "POST", "/auth/password/login", url.Values{"username": {"assistant-alice"}, "password": {"test-password-long"}}.Encode())
	if login.Code != http.StatusSeeOther {
		t.Fatalf("login: %d %s", login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	request := func(body string) (string, uint64) {
		t.Helper()
		w := localRequest(app, "/api/assistant/action/save_assistant_settings", body, cookie)
		if w.Code != 200 {
			t.Fatalf("enable personal assistant: %d %s", w.Code, w.Body.String())
		}
		var out struct {
			Project  string `json:"project"`
			Revision uint64 `json:"revision"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Project == "" {
			t.Fatalf("missing assistant binding: %s %v", w.Body.String(), err)
		}
		return out.Project, out.Revision
	}
	w := localRequest(app, "/api/organization/action/set_assistant_policy", `{"revision":0,"enabled":true}`, admin)
	if w.Code != 200 {
		t.Fatalf("enable assistant policy: %d %s", w.Code, w.Body.String())
	}
	project, revision := request(`{"revision":0,"enabled":true,"externalProcessing":true,"timeZone":"America/Denver","features":["email","slack","calendar"]}`)
	if app.catalog.CanCreateProjects(alice) || app.catalog.IsAdmin(alice) {
		t.Fatal("assistant setup granted unrelated privileges")
	}
	store, err := app.catalog.Access(alice, project, "owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"AGENTS.md", "SOUL.md", "CONTEXT.md", "commitments.md"} {
		if note, err := store.Read(t.Context(), path, ""); err != nil || note.Revision == "" {
			t.Fatalf("missing committed %s: %v", path, err)
		}
	}
	if _, err = app.catalog.Access(bob, project, "read"); err == nil {
		t.Fatal("another employee can read the assistant notebook")
	}
	if err = app.catalog.Member(alice, project, bob, "read"); err == nil {
		t.Fatal("personal assistant notebook was shared")
	}
	if _, err = app.catalog.SetGroup(catalog.Bootstrap, catalog.Group{ID: "assistant-team", Name: "Team", Members: []string{bob}}); err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.SetProjectGroup(alice, project, "assistant-team", "read"); err == nil {
		t.Fatal("personal assistant notebook inherited group access")
	}
	w = localRequest(app, "/api/assistant/action/assistant_context", `{}`, cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "AGENTS.md") {
		t.Fatalf("assistant context: %d %s", w.Code, w.Body.String())
	}
	w = localRequest(app, "/api/assistant/action/save_assistant_settings", `{"revision":0,"enabled":false}`, cookie)
	if w.Code != 409 {
		t.Fatalf("stale settings overwrite: %d %s", w.Code, w.Body.String())
	}
	second, _ := request(fmt.Sprintf(`{"revision":%d,"enabled":false,"timeZone":"America/Denver"}`, revision))
	if second != project {
		t.Fatal("disabling changed the assistant notebook")
	}
}

func TestPersonalConnectionsRequireOwnerSessionAndConsent(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	secret := filepath.Join(dir, "client")
	os.WriteFile(key, bytes.Repeat([]byte{7}, 32), 0600)
	os.WriteFile(secret, []byte("synthetic-client-secret"), 0600)
	config := filepath.Join(dir, "personal.json")
	raw, _ := json.Marshal(connectors.PersonalConfig{Version: 1, KeyFile: key, Google: &connectors.OAuthApplication{ClientID: "synthetic", SecretFile: secret}})
	os.WriteFile(config, raw, 0600)
	if err := app.ConfigurePersonalConnections(config); err != nil {
		t.Fatal(err)
	}
	id := addLocal(t, app, admin, "personal-owner")
	login := oidcRequest(app, "POST", "/auth/password/login", url.Values{"username": {"personal-owner"}, "password": {"test-password-long"}}.Encode())
	cookie := login.Result().Cookies()[0]
	w := oidcRequest(app, "POST", "/account/connections/google/connect", "", cookie)
	if w.Code != 403 {
		t.Fatalf("connect without consent: %d %s", w.Code, w.Body.String())
	}
	app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true})
	settings, err := app.catalog.SaveAssistant(id, catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, TimeZone: "UTC", Features: []string{"email"}})
	if err != nil {
		t.Fatal(err)
	}
	w = oidcRequest(app, "POST", "/account/connections/google/connect", "", cookie)
	if w.Code != 303 {
		t.Fatalf("connect: %d %s", w.Code, w.Body.String())
	}
	bindingCookies := w.Result().Cookies()
	target, err := url.Parse(w.Header().Get("Location"))
	if err != nil || target.Host != "accounts.google.com" || target.Query().Get("state") == "" || target.Query().Get("code_challenge") == "" {
		t.Fatalf("invalid redirect %s", target)
	}
	if strings.Contains(w.Body.String(), "synthetic-client-secret") {
		t.Fatal("secret leaked")
	}
	w = localRequest(app, "/api/assistant/action/assistant_status", `{}`, cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"providers":["google"]`) {
		t.Fatalf("connection status: %d %s", w.Code, w.Body.String())
	}
	settings.Enabled = false
	app.catalog.SaveAssistant(id, settings)
	w = oidcRequest(app, "GET", "/account/connections/google/callback?code=unused&state="+target.Query().Get("state"), "", append([]*http.Cookie{cookie}, bindingCookies...)...)
	if w.Code != 403 {
		t.Fatalf("disabled callback accepted: %d %s", w.Code, w.Body.String())
	}
}
