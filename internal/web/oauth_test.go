package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestOAuthCodeBoundToPKCEAndSingleUse(t *testing.T) {
	app, _ := fixture(t)
	if app.oauth == nil {
		t.Fatal("OAuth is not initialized")
	}
	reg := call(app, "POST", "/oauth/register", `{"client_name":"Test client","redirect_uris":["https://client.example/callback"],"token_endpoint_auth_method":"none"}`, "")
	if reg.Code != 201 {
		t.Fatalf("registration: %d %s", reg.Code, reg.Body.String())
	}
	var client map[string]any
	json.Unmarshal(reg.Body.Bytes(), &client)
	id := client["client_id"].(string)
	verifier := strings.Repeat("v", 43)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	request := grantRequest{Client: id, Redirect: "https://client.example/callback", Challenge: challenge, Scope: "notes:read", Resource: app.origin + "/mcp", Subject: "@local-admin", Expires: time.Now().Add(time.Minute).Unix()}
	code, err := app.issueCode(request)
	if err != nil {
		t.Fatal(err)
	}
	exchange := func(v string) *httptest.ResponseRecorder {
		form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {id}, "redirect_uri": {request.Redirect}, "code_verifier": {v}, "resource": {request.Resource}}
		r := httptest.NewRequest("POST", app.origin+"/oauth/token", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		serveMCPTest(app, w, r)
		return w
	}
	if w := exchange("wrong"); w.Code != 400 {
		t.Fatalf("bad verifier accepted: %d", w.Code)
	}
	w := exchange(verifier)
	if w.Code != 200 {
		t.Fatalf("exchange: %d %s", w.Code, w.Body.String())
	}
	var tokens map[string]any
	json.Unmarshal(w.Body.Bytes(), &tokens)
	if w := exchange(verifier); w.Code != 400 {
		t.Fatalf("code reused: %d", w.Code)
	}
	// Read-only OAuth grants must fail on the MCP write tool, even though the caller is authenticated.
	payload := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"save_note","arguments":{"project":"default","path":"notes/denied.md","content":"no","revision":""}}}`
	r := httptest.NewRequest("POST", app.origin+"/mcp", strings.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("Authorization", "Bearer "+tokens["access_token"].(string))
	w = httptest.NewRecorder()
	serveMCPTest(app, w, r)
	if !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatalf("write not denied: %d %s", w.Code, w.Body.String())
	}
}
func TestOAuthRejectsUnsafeRedirectAndProtectsConsent(t *testing.T) {
	app, _ := fixture(t)
	for _, uri := range []string{"javascript:alert(1)", "http://remote.example/callback", "https://client.example/cb#fragment"} {
		raw, _ := json.Marshal(map[string]any{"redirect_uris": []string{uri}})
		w := call(app, "POST", "/oauth/register", string(raw), "")
		if w.Code != 400 {
			t.Fatalf("accepted %s: %d", uri, w.Code)
		}
	}
	w := call(app, "GET", "/oauth/authorize?client_id=unknown", "", "")
	if w.Code != 303 {
		t.Fatalf("consent not gated: %d", w.Code)
	}
}

func TestOAuthRefreshRotationPersistenceAndUserRevocation(t *testing.T) {
	app, store := fixture(t)
	if _, err := store.Save(context.Background(), "notes/no.md", []byte("visible"), "", "test"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member("@local-admin", "default", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	reg := call(app, "POST", "/oauth/register", `{"redirect_uris":["https://client.example/cb"]}`, "")
	var client map[string]any
	json.Unmarshal(reg.Body.Bytes(), &client)
	verifier := strings.Repeat("x", 43)
	sum := sha256.Sum256([]byte(verifier))
	code, err := app.issueCode(grantRequest{Client: client["client_id"].(string), Redirect: "https://client.example/cb", Challenge: base64.RawURLEncoding.EncodeToString(sum[:]), Scope: "notes:read notes:write", Resource: app.origin + "/mcp", Subject: "alice", Expires: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	exchange := func(form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", app.origin+"/oauth/token", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		serveMCPTest(app, w, r)
		return w
	}
	w := exchange(url.Values{"grant_type": {"authorization_code"}, "client_id": {client["client_id"].(string)}, "code": {code}, "redirect_uri": {"https://client.example/cb"}, "code_verifier": {verifier}})
	if w.Code != 200 {
		t.Fatalf("exchange: %s", w.Body.String())
	}
	var tokens map[string]any
	json.Unmarshal(w.Body.Bytes(), &tokens)
	// Simulate an application restart: grants load from disk, not process-local memory.
	app.oauth, err = reopenOAuth(app)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {client["client_id"].(string)}, "refresh_token": {tokens["refresh_token"].(string)}}
	w = exchange(form)
	if w.Code != 200 {
		t.Fatalf("refresh after restart: %s", w.Body.String())
	}
	var refreshed map[string]any
	json.Unmarshal(w.Body.Bytes(), &refreshed)
	callTool := func(name string) *httptest.ResponseRecorder {
		args := map[string]any{"project": "default", "path": "notes/no.md", "revision": ""}
		if name == "save_note" {
			args["content"] = "no"
		}
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		return call(app, "POST", "/mcp", string(raw), refreshed["access_token"].(string))
	}
	w = callTool("save_note")
	if !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatalf("OAuth grant exceeded user's Read role: %s", w.Body.String())
	}
	w = callTool("read_note")
	if w.Code != 200 || strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatalf("authorized read failed: %s", w.Body.String())
	}
	if err = app.catalog.Member("@local-admin", "default", "alice", ""); err != nil {
		t.Fatal(err)
	}
	w = callTool("read_note")
	if !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatal("OAuth retained revoked project access")
	}
	used := app.oauth.Used[digest(form.Get("refresh_token"))]
	used.UsedAt = time.Now().Add(-31 * time.Second).Unix()
	app.oauth.Used[digest(form.Get("refresh_token"))] = used
	if w = exchange(form); w.Code != 400 {
		t.Fatal("refresh token replay accepted")
	}
}

func TestConsentRequiresTheSameAuthenticatedUser(t *testing.T) {
	app, _ := fixture(t)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	reg := call(app, "POST", "/oauth/register", `{"client_name":"Review client","redirect_uris":["https://client.example/cb"]}`, "")
	var client map[string]any
	json.Unmarshal(reg.Body.Bytes(), &client)
	sum := sha256.Sum256([]byte(strings.Repeat("v", 43)))
	query := url.Values{"client_id": {client["client_id"].(string)}, "redirect_uri": {"https://client.example/cb"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "state": {"test-state"}}
	w := proxyCall(app, "alice", "GET", "/oauth/authorize?"+query.Encode(), "")
	if w.Code != 200 {
		t.Fatalf("consent: %s", w.Body.String())
	}
	policy := w.Header().Get("Content-Security-Policy")
	if strings.Contains(policy, "form-action") || !strings.Contains(policy, "default-src 'self'") || !strings.Contains(policy, "frame-ancestors 'none'") {
		t.Fatalf("consent must permit callback redirect chains and retain other CSP directives: %q", policy)
	}
	fields := url.Values{}
	for _, key := range []string{"request", "signature"} {
		match := regexp.MustCompile(`name="` + key + `" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
		if len(match) != 2 {
			t.Fatal("missing consent field")
		}
		fields.Set(key, html.UnescapeString(match[1]))
	}
	submit := func(user string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", app.origin+"/oauth/authorize", strings.NewReader(fields.Encode()))
		r.RemoteAddr = "127.0.0.1:12"
		r.Header.Set("Remote-User", user)
		r.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
		r.Header.Set("Origin", app.origin)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		serveMCPTest(app, w, r)
		return w
	}
	if w = submit("bob"); w.Code != 400 {
		t.Fatal("another user reused the consent")
	}
	w = submit("alice")
	if w.Code != 303 {
		t.Fatalf("consent submission: %s", w.Body.String())
	}
	if w.Header().Get("Content-Security-Policy") != policy {
		t.Fatal("consent submission changed the consent policy")
	}
	redirect, _ := url.Parse(w.Header().Get("Location"))
	if redirect.Host != "client.example" || redirect.Query().Get("state") != "test-state" || redirect.Query().Get("code") == "" {
		t.Fatal("invalid consent redirect")
	}
}

func TestOAuthManagementConsentIsExplicitAndSurvivesExchange(t *testing.T) {
	for _, resource := range []string{"/mcp", apiPrefix} {
		t.Run(resource, func(t *testing.T) {
			app, _ := fixture(t)
			app.ConfigureHeadless(true)
			app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
			reg := call(app, "POST", "/oauth/register", `{"client_name":"Management client","redirect_uris":["https://client.example/cb"]}`, "")
			var client map[string]any
			json.Unmarshal(reg.Body.Bytes(), &client)
			verifier := strings.Repeat("m", 43)
			sum := sha256.Sum256([]byte(verifier))
			query := url.Values{"client_id": {client["client_id"].(string)}, "redirect_uri": {"https://client.example/cb"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}}
			query.Set("resource", app.origin+resource)
			// Existing/default consent is not silently expanded.
			w := proxyCall(app, "alice", "GET", "/oauth/authorize?"+query.Encode(), "")
			if w.Code != 200 || strings.Contains(w.Body.String(), "Manage projects and access") {
				t.Fatal(w.Code, w.Body)
			}
			query.Set("scope", "notes:read notes:write notes:manage")
			w = proxyCall(app, "alice", "GET", "/oauth/authorize?"+query.Encode(), "")
			if w.Code != 200 || !strings.Contains(w.Body.String(), "Manage projects and access") {
				t.Fatal(w.Code, w.Body)
			}
			fields := url.Values{}
			for _, key := range []string{"request", "signature"} {
				m := regexp.MustCompile(`name="` + key + `" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
				if len(m) != 2 {
					t.Fatal("missing consent field")
				}
				fields.Set(key, html.UnescapeString(m[1]))
			}
			r := httptest.NewRequest("POST", app.origin+"/oauth/authorize", strings.NewReader(fields.Encode()))
			r.RemoteAddr = "127.0.0.1:12"
			r.Header.Set("Remote-User", "alice")
			r.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
			r.Header.Set("Origin", app.origin)
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w = httptest.NewRecorder()
			serveMCPTest(app, w, r)
			if w.Code != 303 {
				t.Fatal(w.Code, w.Body)
			}
			redirect, _ := url.Parse(w.Header().Get("Location"))
			exchange := func(values url.Values) map[string]any {
				t.Helper()
				r := httptest.NewRequest("POST", app.origin+"/oauth/token", strings.NewReader(values.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				w := httptest.NewRecorder()
				serveMCPTest(app, w, r)
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body)
				}
				var token map[string]any
				json.Unmarshal(w.Body.Bytes(), &token)
				return token
			}
			granted := exchange(url.Values{"grant_type": {"authorization_code"}, "code": {redirect.Query().Get("code")}, "client_id": {client["client_id"].(string)}, "redirect_uri": {"https://client.example/cb"}, "code_verifier": {verifier}})
			if granted["scope"] != query.Get("scope") {
				t.Fatal(granted["scope"])
			}
			refreshed := exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {granted["refresh_token"].(string)}, "client_id": {client["client_id"].(string)}})
			if refreshed["scope"] != query.Get("scope") {
				t.Fatal(refreshed["scope"])
			}
			if resource == apiPrefix {
				w := apiCall(app, "GET", apiPrefix+"/capabilities", "", refreshed["access_token"].(string))
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				w = apiCall(app, "POST", "/mcp", `{}`, refreshed["access_token"].(string))
				if w.Code != 401 {
					t.Fatal("API audience accepted at MCP", w.Code)
				}
			}
			query.Set("scope", "notes:superadmin")
			if w := proxyCall(app, "alice", "GET", "/oauth/authorize?"+query.Encode(), ""); w.Code != 400 {
				t.Fatal("unknown scope accepted")
			}
		})
	}
}
