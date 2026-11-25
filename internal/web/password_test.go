package web

import (
	"encoding/json"
	"forgetmenote/internal/catalog"
	"golang.org/x/oauth2"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func localRequest(app *Server, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", app.origin+path, strings.NewReader(body))
	r.Header.Set("Origin", app.origin)
	r.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	serveMCPTest(app, w, r)
	return w
}
func recoverySession(t *testing.T, app *Server) *http.Cookie {
	t.Helper()
	w := oidcRequest(app, "POST", "/login", url.Values{"token": {app.token}}.Encode())
	if w.Code != 303 {
		t.Fatal(w.Code)
	}
	return w.Result().Cookies()[0]
}
func addLocal(t *testing.T, app *Server, cookie *http.Cookie, username string) string {
	t.Helper()
	w := localRequest(app, "/api/local-users", `{"username":"`+username+`","password":"test-password-long"}`, cookie)
	if w.Code != 200 {
		t.Fatalf("create local: %d %s", w.Code, w.Body.String())
	}
	var u struct{ ID string }
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil || u.ID == "" {
		t.Fatal("missing user ID", err)
	}
	return u.ID
}
func TestLocalPasswordLoginAndExternalPrecedence(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	id := addLocal(t, app, admin, "alice")
	w := oidcRequest(app, "POST", "/auth/password/login", url.Values{"username": {"alice"}, "password": {"test-password-long"}}.Encode())
	if w.Code != 303 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	me := oidcRequest(app, "GET", "/api/me", "", cookie)
	if !strings.Contains(me.Body.String(), id) {
		t.Fatal(me.Body.String())
	}
	if w := oidcRequest(app, "GET", "/p/default/api/notes", "", cookie); w.Code != 403 {
		t.Fatal("new account gained project access", w.Code)
	}
	for _, name := range []string{"alice", "missing"} {
		w = oidcRequest(app, "POST", "/auth/password/login", url.Values{"username": {name}, "password": {"incorrect-password"}}.Encode())
		if w.Code != 401 || !strings.Contains(w.Body.String(), "Invalid username or password") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := localRequest(app, "/api/local-users", `{"username":"mallory","password":"test-password-long"}`, cookie); w.Code != 403 {
		t.Fatal("nonadmin created user", w.Code)
	}
	if err := app.ConfigureOIDC("https://unavailable.example", "client", "secret"); err != nil {
		t.Fatal(err)
	}
	w = oidcRequest(app, "POST", "/auth/password/login", url.Values{"username": {"alice"}, "password": {"test-password-long"}}.Encode())
	if w.Code != 403 {
		t.Fatal("local sign-in survived external auth", w.Code)
	}
	if w = oidcRequest(app, "GET", "/login", ""); strings.Contains(w.Body.String(), `action="/auth/password/login"`) {
		t.Fatal("local login visible")
	}
}

func TestLocalPasswordMCPConsentAndResetRevocation(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	id := addLocal(t, app, admin, "alice")
	reg := call(app, "POST", "/oauth/register", `{"client_name":"Local test","redirect_uris":["https://client.example/cb"]}`, "")
	var client map[string]any
	json.Unmarshal(reg.Body.Bytes(), &client)
	verifier := oauth2.GenerateVerifier()
	target := "/oauth/authorize?" + url.Values{"client_id": {client["client_id"].(string)}, "redirect_uri": {"https://client.example/cb"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {oauth2.S256ChallengeFromVerifier(verifier)}}.Encode()
	w := oidcRequest(app, "POST", "/auth/password/login", url.Values{"username": {"alice"}, "password": {"test-password-long"}, "next": {target}}.Encode())
	if w.Code != 303 || w.Header().Get("Location") != target {
		t.Fatal("lost MCP continuation", w.Code)
	}
	cookie := w.Result().Cookies()[0]
	w = oidcRequest(app, "GET", target, "", cookie)
	fields := url.Values{}
	for _, key := range []string{"request", "signature"} {
		match := regexp.MustCompile(`name="` + key + `" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
		if len(match) != 2 {
			t.Fatal("missing consent")
		}
		fields.Set(key, html.UnescapeString(match[1]))
	}
	w = oidcRequest(app, "POST", "/oauth/authorize", fields.Encode(), cookie)
	if w.Code != 303 {
		t.Fatal(w.Code)
	}
	callback, _ := url.Parse(w.Header().Get("Location"))
	w = oidcRequest(app, "POST", "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {callback.Query().Get("code")}, "client_id": {client["client_id"].(string)}, "redirect_uri": {"https://client.example/cb"}, "code_verifier": {verifier}}.Encode())
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var tokens map[string]any
	json.Unmarshal(w.Body.Bytes(), &tokens)
	bearer := tokens["access_token"].(string)
	payload := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_note","arguments":{"project":"default","path":"private.md"}}}`
	if w = call(app, "POST", "/mcp", payload, bearer); !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatal("MCP bypassed permissions", w.Body.String())
	}
	if w = localRequest(app, "/account/password", `{"user":"`+id+`","password":"replacement-password"}`, admin); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = oidcRequest(app, "GET", "/api/me", "", cookie); w.Code != 401 {
		t.Fatal("reset kept browser session", w.Code)
	}
	if w = call(app, "POST", "/mcp", payload, bearer); w.Code != 401 {
		t.Fatal("reset kept MCP access", w.Code)
	}
	w = oidcRequest(app, "POST", "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tokens["refresh_token"].(string)}, "client_id": {client["client_id"].(string)}}.Encode())
	if w.Code != 400 {
		t.Fatal("reset kept refresh token", w.Code)
	}
}
func TestLocalLoginThrottlesAndRejectsCrossOrigin(t *testing.T) {
	app, _ := fixture(t)
	body := url.Values{"username": {"missing"}, "password": {"incorrect"}}.Encode()
	for i := 0; i < 10; i++ {
		if w := oidcRequest(app, "POST", "/auth/password/login", body); w.Code != 401 {
			t.Fatal(i, w.Code)
		}
	}
	if w := oidcRequest(app, "POST", "/auth/password/login", body); w.Code != 429 {
		t.Fatal("missing throttle", w.Code)
	}
	r := httptest.NewRequest("POST", app.origin+"/auth/password/login", strings.NewReader(body))
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	serveMCPTest(app, w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin login", w.Code)
	}
	if err := app.ConfigureProxy(strings.Repeat("s", 32), []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	if w := oidcRequest(app, "POST", "/auth/password/login", body); w.Code != 403 {
		t.Fatal("proxy did not override local", w.Code)
	}
}

func TestInitialAdministratorSetup(t *testing.T) {
	app, _ := fixture(t)
	body := `{"username":"first-admin","password":"initial-test-password"}`
	if w := localRequest(app, "/account/setup", body); w.Code != 401 {
		t.Fatalf("unauthenticated setup: %d", w.Code)
	}
	recovery := recoverySession(t, app)
	me := oidcRequest(app, "GET", "/api/me", "", recovery)
	if !strings.Contains(me.Body.String(), `"setupAvailable":true`) {
		t.Fatal("fresh recovery login did not offer setup", me.Body.String())
	}
	w := localRequest(app, "/account/setup", body, recovery)
	if w.Code != 200 {
		t.Fatalf("setup: %d %s", w.Code, w.Body.String())
	}
	var created struct{ ID string }
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || !app.catalog.IsAdmin(created.ID) {
		t.Fatal("setup did not create administrator", err)
	}
	if len(w.Result().Cookies()) == 0 {
		t.Fatal("setup did not sign in personal administrator")
	}
	personal := w.Result().Cookies()[0]
	me = oidcRequest(app, "GET", "/api/me", "", personal)
	if !strings.Contains(me.Body.String(), created.ID) || !strings.Contains(me.Body.String(), `"admin":true`) {
		t.Fatal("wrong session identity", me.Body.String())
	}
	if w = localRequest(app, "/account/setup", body, personal); w.Code != 403 {
		t.Fatal("personal user can run setup", w.Code)
	}
	if w = localRequest(app, "/account/setup", body, recovery); w.Code != 403 {
		t.Fatal("setup can be repeated", w.Code)
	}
	if err := app.catalog.SetUser("@local-admin", catalog.User{ID: created.ID, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if w = localRequest(app, "/account/setup", body, recovery); w.Code != 403 {
		t.Fatal("disabling administrator reopens setup", w.Code)
	}
	if w = oidcRequest(app, "POST", "/login", url.Values{"token": {app.token}}.Encode()); w.Code != 303 {
		t.Fatal("recovery login lost", w.Code)
	}
}

func TestInitialSetupRespectsAuthenticationBoundaries(t *testing.T) {
	for _, provider := range []string{"oidc", "proxy", "existing-local"} {
		t.Run(provider, func(t *testing.T) {
			app, _ := fixture(t)
			cookie := recoverySession(t, app)
			switch provider {
			case "oidc":
				if err := app.ConfigureOIDC("https://unavailable.example", "client", "secret"); err != nil {
					t.Fatal(err)
				}
			case "proxy":
				if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
					t.Fatal(err)
				}
			case "existing-local":
				addLocal(t, app, cookie, "existing")
			}
			w := localRequest(app, "/account/setup", `{"username":"first-admin","password":"initial-test-password"}`, cookie)
			if w.Code != 403 {
				t.Fatalf("setup available with %s: %d", provider, w.Code)
			}
		})
	}
	app, _ := fixture(t)
	cookie := recoverySession(t, app)
	r := httptest.NewRequest("POST", app.origin+"/account/setup", strings.NewReader(`{"username":"attacker","password":"initial-test-password"}`))
	r.AddCookie(cookie)
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	serveMCPTest(app, w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin setup accepted", w.Code)
	}
}
