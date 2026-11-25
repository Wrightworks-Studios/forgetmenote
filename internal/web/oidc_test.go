package web

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/instanceconfig"
	"github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
)

type testProvider struct {
	server                         *httptest.Server
	nonce, challenge, sub, invalid string
	claims                         map[string]any
	scopes, requestedScopes        []string
	key                            *rsa.PrivateKey
	t                              *testing.T
}

func provider(t *testing.T) *testProvider {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	p := &testProvider{key: key, t: t, sub: "subject-alice", scopes: []string{"openid", "email"}}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{"issuer": p.server.URL, "authorization_endpoint": p.server.URL + "/authorize", "token_endpoint": p.server.URL + "/token", "jwks_uri": p.server.URL + "/keys", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}, "scopes_supported": p.scopes})
		case "/keys":
			json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: "one", Algorithm: "RS256", Use: "sig"}}})
		case "/token":
			r.ParseForm()
			if oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != p.challenge {
				http.Error(w, "PKCE", 400)
				return
			}
			id, secret, ok := r.BasicAuth()
			if !ok || id != "test-client" || secret != "test-secret" {
				http.Error(w, "client", 401)
				return
			}
			claims := map[string]any{"iss": p.server.URL, "sub": p.sub, "aud": "test-client", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": p.nonce, "email": "same@example.com", "preferred_username": "alice"}
			for name, value := range p.claims {
				claims[name] = value
			}
			switch p.invalid {
			case "nonce":
				claims["nonce"] = "wrong"
			case "issuer":
				claims["iss"] = "https://wrong.example"
			case "audience":
				claims["aud"] = "other"
			case "expiry":
				claims["exp"] = time.Now().Add(-time.Hour).Unix()
			case "subject":
				claims["sub"] = ""
			}
			key := p.key
			if p.invalid == "signature" {
				key, _ = rsa.GenerateKey(rand.Reader, 2048)
			}
			signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "one"))
			raw, _ := json.Marshal(claims)
			signed, _ := signer.Sign(raw)
			token, _ := signed.CompactSerialize()
			json.NewEncoder(w).Encode(map[string]any{"access_token": "ignored", "token_type": "Bearer", "id_token": token})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.server.Close)
	return p
}
func oidcRequest(app *Server, method, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, app.origin+path, strings.NewReader(body))
	if method == "POST" {
		r.Header.Set("Origin", app.origin)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	serveMCPTest(app, w, r)
	return w
}
func beginOIDC(t *testing.T, app *Server, p *testProvider, path string, cookies ...*http.Cookie) (string, *http.Cookie) {
	t.Helper()
	w := oidcRequest(app, "POST", path, "", cookies...)
	if w.Code != 303 {
		t.Fatalf("start %d %s", w.Code, w.Body.String())
	}
	u, _ := url.Parse(w.Header().Get("Location"))
	p.nonce = u.Query().Get("nonce")
	p.challenge = u.Query().Get("code_challenge")
	p.requestedScopes = strings.Fields(u.Query().Get("scope"))
	if p.nonce == "" || p.challenge == "" || u.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("missing nonce/PKCE")
	}
	return u.Query().Get("state"), w.Result().Cookies()[0]
}
func finishOIDC(app *Server, state string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	return oidcRequest(app, "GET", "/auth/oidc/callback?code=verified&state="+url.QueryEscape(state), "", cookies...)
}

func signInOIDCUser(t *testing.T, app *Server, p *testProvider) catalog.User {
	t.Helper()
	state, binding := beginOIDC(t, app, p, "/auth/oidc/login")
	w := finishOIDC(app, state, binding)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == app.canonicalSessionCookie() {
			me := oidcRequest(app, "GET", "/api/me", "", cookie)
			var user catalog.User
			if err := json.Unmarshal(me.Body.Bytes(), &user); err != nil || me.Code != http.StatusOK {
				t.Fatalf("current user: %d %s", me.Code, me.Body.String())
			}
			return user
		}
	}
	t.Fatal("no session")
	return catalog.User{}
}

func TestOIDCEmailDefaultDisplayName(t *testing.T) {
	for _, test := range []struct {
		name, want string
		claims     map[string]any
		openidOnly bool
	}{
		{name: "verified email", claims: map[string]any{"email": "nikki@example.com", "email_verified": true}, want: "nikki@example.com"},
		{name: "signed email without verification flag", claims: map[string]any{"email": "nikki@example.com"}, openidOnly: true, want: "Contributor"},
		{name: "verified email with openid scope only", claims: map[string]any{"email": "nikki@example.com", "email_verified": true}, openidOnly: true, want: "nikki@example.com"},
		{name: "non-boolean verification flag", claims: map[string]any{"email": "nikki@example.com", "email_verified": "true"}, want: "Contributor"},
		{name: "missing email", claims: map[string]any{"email": ""}, want: "Contributor"},
		{name: "unverified email", claims: map[string]any{"email_verified": false}, want: "Contributor"},
		{name: "malformed claim", claims: map[string]any{"email": 123}, want: "Contributor"},
		{name: "invalid address", claims: map[string]any{"email": "not an email"}, want: "Contributor"},
		{name: "long address", claims: map[string]any{"email": strings.Repeat("a", 110) + "@example.com"}, want: "Contributor"},
		{name: "line break", claims: map[string]any{"email": "nikki@example.com\r\n"}, want: "Contributor"},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, _ := fixture(t)
			p := provider(t)
			p.claims = test.claims
			if test.openidOnly {
				p.scopes = []string{"openid"}
			}
			if err := app.ConfigureOIDC(p.server.URL, "test-client", "test-secret"); err != nil {
				t.Fatal(err)
			}
			user := signInOIDCUser(t, app, p)
			if user.DisplayName != test.want {
				t.Fatalf("display name = %q, want %q", user.DisplayName, test.want)
			}
			if got := strings.Join(p.requestedScopes, " "); got != "openid" {
				t.Fatalf("requested scopes = %q, want existing openid scope", got)
			}
			if user.Admin || len(app.catalog.List(user.ID)) != 0 {
				t.Fatal("email granted authority")
			}
		})
	}
}

func TestOIDCEmailDefaultPreservesNamesAndSeparateAccounts(t *testing.T) {
	app, _ := fixture(t)
	p := provider(t)
	p.claims = map[string]any{"email": "", "email_verified": true}
	if err := app.ConfigureOIDC(p.server.URL, "test-client", "test-secret"); err != nil {
		t.Fatal(err)
	}
	first := signInOIDCUser(t, app, p)
	p.claims["email"] = "nikki@example.com"
	user := signInOIDCUser(t, app, p)
	if user.ID != first.ID || user.DisplayName != "nikki@example.com" {
		t.Fatalf("unnamed account was not filled: %#v", user)
	}
	name := "Nikki"
	if err := app.catalog.UpdateUser(catalog.Bootstrap, catalog.UserUpdate{ID: user.ID, DisplayName: &name}); err != nil {
		t.Fatal(err)
	}
	user = signInOIDCUser(t, app, p)
	if user.ID != first.ID || user.DisplayName != name {
		t.Fatalf("login overwrote chosen name: %#v", user)
	}
	p.sub = "separate-subject"
	other := signInOIDCUser(t, app, p)
	if other.ID == user.ID || other.DisplayName != "nikki@example.com" {
		t.Fatalf("same email merged identities: %#v", other)
	}
}

func TestOIDCLoginProvisioningAndFailures(t *testing.T) {
	app, _ := fixture(t)
	p := provider(t)
	if e := app.ConfigureOIDC(p.server.URL, "test-client", "test-secret"); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{"nonce", "issuer", "audience", "expiry", "signature", "subject"} {
		t.Run(bad, func(t *testing.T) {
			p.invalid = bad
			state, binding := beginOIDC(t, app, p, "/auth/oidc/login")
			w := finishOIDC(app, state, binding)
			if w.Code != 401 {
				t.Fatalf("accepted bad %s: %d %s", bad, w.Code, w.Body.String())
			}
		})
	}
	p.invalid = ""
	state, binding := beginOIDC(t, app, p, "/auth/oidc/login")
	if w := finishOIDC(app, state); w.Code != 403 {
		t.Fatal("missing binding accepted")
	}
	state, binding = beginOIDC(t, app, p, "/auth/oidc/login")
	w := finishOIDC(app, state, binding)
	if w.Code != 303 {
		t.Fatalf("login %d %s", w.Code, w.Body.String())
	}
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == app.canonicalSessionCookie() {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session")
	}
	me := oidcRequest(app, "GET", "/api/me", "", session)
	var user catalog.User
	json.Unmarshal(me.Body.Bytes(), &user)
	if user.ID == "" || user.ID == "alice" || user.Admin {
		t.Fatal(me.Body.String())
	}
	if w := oidcRequest(app, "GET", "/p/default/api/notes", "", session); w.Code != 403 {
		t.Fatal("login granted project access")
	}
	if w := finishOIDC(app, state, binding); w.Code != 403 {
		t.Fatal("callback replay accepted")
	}
	state, binding = beginOIDC(t, app, p, "/auth/oidc/login")
	w = finishOIDC(app, state, binding)
	if w.Code != 303 {
		t.Fatal("repeat login failed")
	}
	app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: user.ID, Disabled: true})
	if w := oidcRequest(app, "GET", "/api/me", "", session); w.Code != 401 {
		t.Fatal("disabled session accepted")
	}
	state, binding = beginOIDC(t, app, p, "/auth/oidc/login")
	if w := finishOIDC(app, state, binding); w.Code != 403 {
		t.Fatal("disabled login accepted", w.Code)
	}
}

func TestOIDCDefaultGroupGrantsOnlyNewAccounts(t *testing.T) {
	app, _ := fixture(t)
	p := provider(t)
	g, err := app.catalog.SetGroup(catalog.Bootstrap, catalog.Group{ID: "employees", Name: "Employees"})
	if err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.SetProjectGroup(catalog.Bootstrap, "default", g.ID, "read"); err != nil {
		t.Fatal(err)
	}
	if err = app.ConfigureOIDC(p.server.URL, "test-client", "test-secret", g.ID); err != nil {
		t.Fatal(err)
	}
	login := func() (*http.Cookie, catalog.User) {
		t.Helper()
		state, binding := beginOIDC(t, app, p, "/auth/oidc/login")
		w := finishOIDC(app, state, binding)
		if w.Code != 303 {
			t.Fatalf("login: %d %s", w.Code, w.Body.String())
		}
		for _, cookie := range w.Result().Cookies() {
			if cookie.Name == app.canonicalSessionCookie() {
				var user catalog.User
				json.Unmarshal(oidcRequest(app, "GET", "/api/me", "", cookie).Body.Bytes(), &user)
				return cookie, user
			}
		}
		t.Fatal("no session")
		return nil, catalog.User{}
	}
	session, user := login()
	if user.Admin {
		t.Fatal("provisioned administrator")
	}
	if w := oidcRequest(app, "GET", "/p/default/api/notes", "", session); w.Code != 200 {
		t.Fatalf("new employee cannot read shared notebook: %d %s", w.Code, w.Body.String())
	}
	if _, err = app.catalog.Access(user.ID, "default", "edit"); err == nil {
		t.Fatal("reader gained edit access")
	}
	org, err := app.catalog.Organization(catalog.Bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range org.Groups {
		if group.ID == g.ID {
			g = group
		}
	}
	if len(g.Members) != 1 || g.Members[0] != user.ID {
		t.Fatalf("wrong members: %v", g.Members)
	}
	g.Members = nil
	if _, err = app.catalog.SetGroup(catalog.Bootstrap, g); err != nil {
		t.Fatal(err)
	}
	session, _ = login()
	if w := oidcRequest(app, "GET", "/p/default/api/notes", "", session); w.Code != 403 {
		t.Fatal("login undid explicit access revocation", w.Code)
	}
	// A deleted/misspelled default group cannot silently create a stranded identity.
	p.sub = "another-subject"
	if err = app.ConfigureOIDC(p.server.URL, "test-client", "test-secret", "missing"); err != nil {
		t.Fatal(err)
	}
	state, binding := beginOIDC(t, app, p, "/auth/oidc/login")
	if w := finishOIDC(app, state, binding); w.Code == 303 {
		t.Fatal("accepted missing default group")
	}
}

func TestOIDCExplicitLinkPreservesAccountAndRequiresSameSession(t *testing.T) {
	app, _ := fixture(t)
	p := provider(t)
	app.ConfigureOIDC(p.server.URL, "test-client", "test-secret")
	app.catalog.Member(catalog.Bootstrap, "default", "legacy", "owner")
	old, _ := app.catalog.Account("legacy")
	app.catalog.LinkGitHub("legacy", catalog.GitHubAccount{ID: 123, Login: "legacy", Created: time.Now()}, old.Generation)
	existing := httptest.NewRecorder()
	app.createSession(existing, "legacy")
	cookie := existing.Result().Cookies()[0]
	state, binding := beginOIDC(t, app, p, "/account/oidc/link", cookie)
	if w := finishOIDC(app, state, binding); w.Code != 403 {
		t.Fatal("linked without original session")
	}
	state, binding = beginOIDC(t, app, p, "/account/oidc/link", cookie)
	if w := finishOIDC(app, state, binding, cookie); w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	id, e := app.catalog.ResolveOIDC(p.server.URL, p.sub, "", "")
	if e != nil || id != "legacy" {
		t.Fatal(id, e)
	}
	linked, _ := app.catalog.Account(id)
	if old.ID != linked.ID || linked.GitHub == nil {
		t.Fatal("lost existing identity/GitHub")
	}
	if _, e = app.catalog.Access(id, "default", "owner"); e != nil {
		t.Fatal("lost permissions")
	}
	// A fresh provider subject with the same username/email is a separate account.
	p.sub = "someone-else"
	state, binding = beginOIDC(t, app, p, "/auth/oidc/login")
	if w := finishOIDC(app, state, binding); w.Code != 303 {
		t.Fatal(w.Code)
	}
	other, _ := app.catalog.ResolveOIDC(p.server.URL, p.sub, "", "")
	if other == id {
		t.Fatal("merged by email")
	}
}
func TestOIDCSessionsPersistLogoutAndRevokeMCP(t *testing.T) {
	app, _ := fixture(t)
	p := provider(t)
	app.ConfigureOIDC(p.server.URL, "test-client", "test-secret")
	state, binding := beginOIDC(t, app, p, "/auth/oidc/login")
	w := finishOIDC(app, state, binding)
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == app.canonicalSessionCookie() {
			cookie = c
		}
	}
	id, _ := app.catalog.ResolveOIDC(p.server.URL, p.sub, "", "")
	// Restart the HTTP service using persisted state after releasing its store ownership.
	app.Close()
	again, e := New(app.catalog, app.db, app.data, app.origin, app.token)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(again.Close)
	app = again
	if w := oidcRequest(app, "GET", "/api/me", "", cookie); w.Code != 200 {
		t.Fatal("session not persisted")
	}
	app.oauth.Access[digest("access")] = withOAuthFamily(accessGrant{Subject: id, Resource: app.origin + "/mcp", Expires: time.Now().Add(time.Hour).Unix()})
	app.oauth.Refresh[digest("refresh")] = withOAuthFamily(accessGrant{Subject: id, Client: "client", Resource: app.origin + "/mcp", Expires: time.Now().Add(time.Hour).Unix()})
	if e = app.catalog.Revoke("other", id); e == nil {
		t.Fatal("unauthorized revocation")
	}
	app.catalog.Revoke(catalog.Bootstrap, id)
	if w := oidcRequest(app, "GET", "/api/me", "", cookie); w.Code != 401 {
		t.Fatal("revoked browser session accepted")
	}
	req := httptest.NewRequest("GET", app.origin+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer access")
	if app.validAccessToken(req) {
		t.Fatal("revoked MCP accepted")
	}
	w = oidcRequest(app, "POST", "/oauth/token", "grant_type=refresh_token&refresh_token=refresh&client_id=client")
	if w.Code != 400 {
		t.Fatal("revoked refresh accepted")
	}
	rec := httptest.NewRecorder()
	app.createSession(rec, id)
	cookie = rec.Result().Cookies()[0]
	if w = oidcRequest(app, "POST", "/logout", "", cookie); w.Code != 303 {
		t.Fatal(w.Code)
	}
	if w = oidcRequest(app, "GET", "/api/me", "", cookie); w.Code != 401 {
		t.Fatal("logged out cookie accepted")
	}
	// Recovery remains available regardless of OIDC availability.
	if w = oidcRequest(app, "POST", "/login", "token="+app.token); w.Code != 303 {
		t.Fatal("recovery failed")
	}
}

func TestMCPConsentThroughOIDC(t *testing.T) {
	app, _ := fixture(t)
	p := provider(t)
	app.ConfigureOIDC(p.server.URL, "test-client", "test-secret")
	reg := call(app, "POST", "/oauth/register", `{"client_name":"OIDC test","redirect_uris":["https://client.example/cb"]}`, "")
	var client map[string]any
	json.Unmarshal(reg.Body.Bytes(), &client)
	verifier := oauth2.GenerateVerifier()
	query := url.Values{"client_id": {client["client_id"].(string)}, "redirect_uri": {"https://client.example/cb"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {oauth2.S256ChallengeFromVerifier(verifier)}}
	target := "/oauth/authorize?" + query.Encode()
	if w := oidcRequest(app, "GET", target, ""); w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/login?next=") {
		t.Fatal("MCP login continuation missing")
	}
	start := oidcRequest(app, "POST", "/auth/oidc/login", url.Values{"next": {target}}.Encode())
	u, _ := url.Parse(start.Header().Get("Location"))
	p.nonce = u.Query().Get("nonce")
	p.challenge = u.Query().Get("code_challenge")
	w := finishOIDC(app, u.Query().Get("state"), start.Result().Cookies()[0])
	if w.Code != 303 || w.Header().Get("Location") != target {
		t.Fatal("OIDC lost consent continuation", w.Code)
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == app.canonicalSessionCookie() {
			cookie = c
		}
	}
	w = oidcRequest(app, "GET", target, "", cookie)
	if w.Code != 200 {
		t.Fatal("consent failed", w.Code)
	}
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
		t.Fatal("consent submission failed", w.Code)
	}
	callback, _ := url.Parse(w.Header().Get("Location"))
	w = oidcRequest(app, "POST", "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {callback.Query().Get("code")}, "client_id": {client["client_id"].(string)}, "redirect_uri": {"https://client.example/cb"}, "code_verifier": {verifier}}.Encode())
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var tokens map[string]string
	json.Unmarshal(w.Body.Bytes(), &tokens)
	request := httptest.NewRequest("POST", app.origin+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_note","arguments":{"project":"default","path":"notes/private.md"}}}`))
	request.Header.Set("Authorization", "Bearer "+tokens["access_token"])
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	result := httptest.NewRecorder()
	serveMCPTest(app, result, request)
	if !strings.Contains(result.Body.String(), `"isError":true`) {
		t.Fatal("MCP login granted project access", result.Body.String())
	}
}

func TestOIDCPendingStateAndRecoveryRotation(t *testing.T) {
	app, _ := fixture(t)
	p := provider(t)
	app.ConfigureOIDC(p.server.URL, "test-client", "test-secret")
	state, binding := beginOIDC(t, app, p, "/auth/oidc/login")
	if w := finishOIDC(app, "wrong", binding); w.Code != 403 {
		t.Fatal("invalid state accepted")
	}
	expireFlow(t, app, "oidc/login", digest(state))
	if w := finishOIDC(app, state, binding); w.Code != 403 {
		t.Fatal("expired flow accepted")
	}
	state, binding = beginOIDC(t, app, p, "/auth/oidc/login")
	if w := oidcRequest(app, "GET", "/auth/oidc/callback?error=access_denied&state="+state, "", binding); w.Code != 401 {
		t.Fatal("denial accepted")
	}
	rec := httptest.NewRecorder()
	if err := app.createSession(rec, catalog.Bootstrap); err != nil {
		t.Fatal(err)
	}
	cookie := rec.Result().Cookies()[0]
	app.token = strings.Repeat("r", 40)
	if w := oidcRequest(app, "GET", "/api/me", "", cookie); w.Code != 401 {
		t.Fatal("key rotation retained recovery session")
	}
	p.server.Close()
	if w := oidcRequest(app, "POST", "/login", "token="+app.token); w.Code != 303 {
		t.Fatal("provider outage blocked recovery")
	}
}

func TestOIDCLinkFromTrustedProxySurvivesPublicCallback(t *testing.T) {
	app, _ := fixture(t)
	p := provider(t)
	app.ConfigureOIDC(p.server.URL, "test-client", "test-secret")
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	app.catalog.Member(catalog.Bootstrap, "default", "legacy", "owner")
	w := proxyCall(app, "legacy", "POST", "/account/oidc/link", `{}`)
	if w.Code != 303 {
		t.Fatal("start", w.Code, w.Body.String())
	}
	u, _ := url.Parse(w.Header().Get("Location"))
	p.nonce = u.Query().Get("nonce")
	p.challenge = u.Query().Get("code_challenge")
	// The provider callback is public at the proxy, so it has no Remote-User header.
	w = finishOIDC(app, u.Query().Get("state"), w.Result().Cookies()...)
	if w.Code != 303 {
		t.Fatal("proxy identity lost at public callback", w.Code, w.Body.String())
	}
	id, e := app.catalog.ResolveOIDC(p.server.URL, p.sub, "", "")
	if e != nil || id != "legacy" {
		t.Fatal(id, e)
	}
}

func TestOIDCInitiatingPagesPermitOnlyVerifiedAuthorizationOrigin(t *testing.T) {
	app, _ := fixture(t)
	p := provider(t)
	if err := app.ConfigureOIDC(p.server.URL, "test-client", "test-secret"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/login", "/"} {
		auth := ""
		if path == "/" {
			auth = testToken
		}
		w := call(app, "GET", path, "", auth)
		if w.Code != 200 {
			t.Fatal(path, w.Code)
		}
		policy := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(policy, "form-action 'self' "+p.server.URL) {
			t.Fatalf("%s blocks the provider redirect at its initiating page: %s", path, policy)
		}
		if strings.Contains(policy, "form-action *") {
			t.Fatal("unrestricted form destinations")
		}
	}
}
func TestLoginPageRemainsAvailableDuringOIDCOutage(t *testing.T) {
	app, _ := fixture(t)
	p := provider(t)
	app.ConfigureOIDC(p.server.URL, "test-client", "test-secret")
	p.server.Close()
	w := call(app, "GET", "/login", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Recovery administrator") {
		t.Fatal("recovery page unavailable", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "form-action 'self'") {
		t.Fatal("lost form protection")
	}
}

func TestOIDCSignInKeepsVerifiedEmailAndHostedDomain(t *testing.T) {
	app, _ := fixture(t)
	p := provider(t)
	p.claims = map[string]any{"email": "Nikki@Example.com", "email_verified": true, "hd": "example.com"}
	if err := app.ConfigureOIDC(p.server.URL, "test-client", "test-secret"); err != nil {
		t.Fatal(err)
	}
	user := signInOIDCUser(t, app, p)
	if got := app.catalog.ExternalIdentities(user.ID); len(got) != 1 || got[0].Email != "nikki@example.com" || got[0].HostedDomain != "example.com" {
		t.Fatalf("sign-in did not keep the verified address: %+v", got)
	}
	p.claims = map[string]any{"email": "nikki@example.com", "email_verified": false, "hd": "example.com"}
	signInOIDCUser(t, app, p)
	if got := app.catalog.ExternalIdentities(user.ID); got[0].Email != "" || got[0].HostedDomain != "" {
		t.Fatalf("an unverified sign-in kept the address: %+v", got)
	}
}

func TestOIDCRequestsEmailScopeOnlyWhenConfigured(t *testing.T) {
	app, _ := fixture(t)
	p := provider(t)
	p.claims = map[string]any{"email": "nikki@example.com", "email_verified": true, "hd": "example.com"}
	secret := filepath.Join(t.TempDir(), "oidc-secret")
	if err := os.WriteFile(secret, []byte("test-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	config := instanceconfig.Config{Version: 1, Identity: instanceconfig.Identity{OIDC: &instanceconfig.OIDC{Issuer: p.server.URL, ClientID: "test-client", SecretFile: secret, RequestEmail: true}}}
	if err := app.ConfigureInstance(instanceconfig.Resolved{Config: config}); err != nil {
		t.Fatal(err)
	}
	user := signInOIDCUser(t, app, p)
	if got := strings.Join(p.requestedScopes, " "); got != "openid email" {
		t.Fatalf("requested scopes = %q, want openid email", got)
	}
	if got := app.catalog.ExternalIdentities(user.ID); len(got) != 1 || got[0].Email != "nikki@example.com" {
		t.Fatalf("sign-in did not keep the verified address: %+v", got)
	}
}
