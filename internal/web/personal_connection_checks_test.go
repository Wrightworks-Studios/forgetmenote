package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
)

// personalConnectFixture signs in a person whose assistant may read their
// mail. Google's OAuth and API requests go to google, which runs inside each
// request before the fake Google answers it.
func personalConnectFixture(t *testing.T, google func(path string)) (app *Server, user string, session *http.Cookie) {
	t.Helper()
	app, _ = fixture(t)
	user = addLocal(t, app, recoverySession(t, app), "personal-owner")
	session = passwordSession(t, app, "personal-owner")
	if _, err := app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.catalog.SaveAssistant(user, catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, TimeZone: "UTC", Features: []string{"email"}}); err != nil {
		t.Fatal(err)
	}
	personalProviderFixture(t, app, "google", func(w http.ResponseWriter, r *http.Request) {
		google(r.URL.Path)
		switch r.URL.Path {
		case "/token":
			fmt.Fprint(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600,"scope":"openid email profile https://www.googleapis.com/auth/gmail.modify"}`)
		case "/v1/userinfo":
			fmt.Fprint(w, `{"sub":"google-owner","email":"owner@example.test","email_verified":true}`)
		default:
			t.Errorf("unexpected Google path %s", r.URL.Path)
			fmt.Fprint(w, `{}`)
		}
	})
	return app, user, session
}

// passwordSession signs in a local user and returns their session cookie.
func passwordSession(t *testing.T, app *Server, username string) *http.Cookie {
	t.Helper()
	w := oidcRequest(app, "POST", "/auth/password/login", url.Values{"username": {username}, "password": {"test-password-long"}}.Encode())
	if w.Code != 303 || len(w.Result().Cookies()) == 0 {
		t.Fatalf("sign in: %d %s", w.Code, w.Body.String())
	}
	return w.Result().Cookies()[0]
}

// beginGoogleConnect starts connecting Google in a session. It returns the
// OAuth state and the session's binding cookie for this flow.
func beginGoogleConnect(t *testing.T, app *Server, session *http.Cookie) (string, *http.Cookie) {
	t.Helper()
	w := oidcRequest(app, "POST", "/account/connections/google/connect", "", session)
	if w.Code != 303 {
		t.Fatalf("connect: %d %s", w.Code, w.Body.String())
	}
	target, err := url.Parse(w.Header().Get("Location"))
	if err != nil || target.Query().Get("state") == "" {
		t.Fatalf("invalid redirect %q", w.Header().Get("Location"))
	}
	for _, c := range w.Result().Cookies() {
		if strings.Contains(c.Name, "fmn-personal-google") {
			return target.Query().Get("state"), c
		}
	}
	t.Fatal("connect set no binding cookie")
	return "", nil
}

// googleCallback is Google's redirect back to the instance after consent.
func googleCallback(app *Server, state string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	return oidcRequest(app, "GET", "/account/connections/google/callback?code=synthetic-code&state="+url.QueryEscape(state), "", cookies...)
}

// googleConnected reports whether the person has a connected Google account.
func googleConnected(t *testing.T, app *Server, user string) bool {
	t.Helper()
	account, err := app.catalog.Account(user)
	if err != nil {
		t.Fatal(err)
	}
	connections, err := app.personalConnections.List(account.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range connections {
		if c.Provider == "google" && c.State == "connected" {
			return true
		}
	}
	return false
}

// A connect flow finishes only in the browser session that started it. The
// callback needs that session's binding cookie: without one, or with the
// cookie of another session's flow, nothing is connected.
func TestPersonalConnectionCallbackNeedsItsSessionsBindingCookie(t *testing.T) {
	app, user, first := personalConnectFixture(t, func(string) {})
	second := passwordSession(t, app, "personal-owner")

	state, _ := beginGoogleConnect(t, app, first)
	if w := googleCallback(app, state, first); w.Code < 400 || googleConnected(t, app, user) {
		t.Fatalf("a callback without its binding cookie connected Google: %d %s", w.Code, w.Body.String())
	}

	_, firstBinding := beginGoogleConnect(t, app, first)
	state, _ = beginGoogleConnect(t, app, second)
	if w := googleCallback(app, state, first, firstBinding); w.Code < 400 || googleConnected(t, app, user) {
		t.Fatalf("a callback with another session's binding cookie connected Google: %d %s", w.Code, w.Body.String())
	}

	state, binding := beginGoogleConnect(t, app, second)
	if w := googleCallback(app, state, second, binding); w.Code != 303 || !googleConnected(t, app, user) {
		t.Fatalf("a callback with its own binding cookie did not connect Google: %d %s", w.Code, w.Body.String())
	}
}

// Finishing a connection rechecks the person's session after Google answers.
// If the person's sessions end during the token exchange, nothing is
// connected.
func TestPersonalConnectionCallbackStopsWhenTheSessionsEnd(t *testing.T) {
	var app *Server
	var user string
	var revoke atomic.Bool
	app, user, session := personalConnectFixture(t, func(path string) {
		if path == "/token" && revoke.CompareAndSwap(true, false) {
			if err := app.catalog.Revoke(catalog.Bootstrap, user); err != nil {
				t.Error(err)
			}
		}
	})
	state, binding := beginGoogleConnect(t, app, session)
	revoke.Store(true)
	w := googleCallback(app, state, session, binding)
	if revoke.Load() {
		t.Fatal("the callback never exchanged its code")
	}
	if w.Code < 400 || googleConnected(t, app, user) {
		t.Fatalf("connected Google after the person's sessions ended: %d %s", w.Code, w.Body.String())
	}
}

// The connection routes serve only the person's own browser. Neither an API
// credential nor an OAuth access token can start, finish or remove a
// connection, whatever its scopes. Authentication refuses them before the
// route runs: these paths accept only a browser session, the trusted proxy or
// the recovery bearer, and the action API never routes to them.
func TestPersonalConnectionRoutesRefuseAPICredentials(t *testing.T) {
	app, user, session := personalConnectFixture(t, func(string) {})
	state, binding := beginGoogleConnect(t, app, session)
	if w := googleCallback(app, state, session, binding); w.Code != 303 || !googleConnected(t, app, user) {
		t.Fatalf("fixture: connect Google: %d %s", w.Code, w.Body.String())
	}
	account, err := app.catalog.Account(user)
	if err != nil {
		t.Fatal(err)
	}
	connected, err := app.personalConnections.List(account.ID)
	if err != nil || len(connected) != 1 {
		t.Fatal("fixture: the Google connection", connected, err)
	}
	grant, err := issueCredential(app.catalog, app.oauth, user, CredentialSpec{Name: "everything", AllProjects: true, Scopes: []string{"notes:read", "notes:write", "notes:manage", "data:read"}, Audiences: []string{"api", "mcp"}, Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	const oauthToken = "synthetic-oauth-access"
	app.oauth.Mu.Lock()
	app.oauth.Access[digest(oauthToken)] = withOAuthFamily(accessGrant{Subject: user, Client: "synthetic-client", Scope: "notes:read notes:write notes:manage data:read", Resource: app.origin + "/mcp", Expires: time.Now().Add(time.Hour).Unix(), Epoch: app.catalog.Epoch(user)})
	app.oauth.Mu.Unlock()
	for _, credential := range []struct{ name, token, method, path, body string }{
		{"API credential", grant.Token, "GET", "/api/v1/capabilities", ""},
		{"OAuth access token", oauthToken, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}}`},
	} {
		if w := bearerRequest(app, credential.token, credential.method, credential.path, credential.body); w.Code != 200 {
			t.Fatalf("fixture: the %s does not authenticate on %s: %d %s", credential.name, credential.path, w.Code, w.Body.String())
		}
		refuseBearerConnectionChanges(t, app, session, credential.name, credential.token)
	}
	current, err := app.personalConnections.List(account.ID)
	if err != nil || len(current) != 1 || current[0].State != "connected" || current[0].Generation != connected[0].Generation {
		t.Fatalf("a bearer token changed the Google connection: %v %v", current, err)
	}
}

// bearerRequest sends a same-origin request with a bearer token.
func bearerRequest(app *Server, token, method, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, app.origin+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Origin", app.origin)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	serveMCPTest(app, w, r)
	return w
}

// refuseBearerConnectionChanges checks that a bearer token cannot start,
// finish or remove a Google connection.
func refuseBearerConnectionChanges(t *testing.T, app *Server, session *http.Cookie, name, token string) {
	t.Helper()
	bearer := func(method, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		return bearerRequest(app, token, method, path, "", cookies...)
	}
	if w := bearer("POST", "/account/connections/google/connect"); w.Code < 400 {
		t.Fatalf("an %s started a connection: %d %s", name, w.Code, w.Header().Get("Location"))
	}
	state, binding := beginGoogleConnect(t, app, session)
	if w := bearer("GET", "/account/connections/google/callback?code=synthetic-code&state="+url.QueryEscape(state), binding); w.Code < 400 {
		t.Fatalf("an %s finished a connection: %d %s", name, w.Code, w.Body.String())
	}
	if w := bearer("POST", "/account/connections/google/disconnect"); w.Code < 400 {
		t.Fatalf("an %s removed a connection: %d %s", name, w.Code, w.Body.String())
	}
}

// personalCall rechecks the person's assistant settings, the instance
// assistant policy and the person's session when the provider answers. A
// change to any of them during the call discards the answer, even one that
// leaves the person's consent in place.
func TestPersonalCallDiscardsAnAnswerAfterAChange(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, app *Server)
	}{
		{"settings", func(t *testing.T, app *Server) {
			settings, err := app.catalog.Assistant("alice")
			if err != nil {
				t.Error(err)
				return
			}
			settings.Notifications = !settings.Notifications
			if _, err = app.catalog.SaveAssistant("alice", settings); err != nil {
				t.Error(err)
			}
		}},
		{"policy", func(t *testing.T, app *Server) {
			policy := app.catalog.AssistantPolicy()
			policy.IntervalSeconds = 600
			if _, err := app.catalog.SetAssistantPolicy(catalog.Bootstrap, policy); err != nil {
				t.Error(err)
			}
		}},
		{"session", func(t *testing.T, app *Server) {
			if err := app.catalog.Revoke(catalog.Bootstrap, "alice"); err != nil {
				t.Error(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, _ := fixture(t)
			if _, err := app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true}); err != nil {
				t.Fatal(err)
			}
			if _, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, TimeZone: "UTC", Features: []string{"email"}}); err != nil {
				t.Fatal(err)
			}
			var change atomic.Bool
			connectPersonalFixture(t, app, "alice", func(r *http.Request) string {
				switch r.URL.Path {
				case "/token":
					return `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600,"scope":"openid email profile https://www.googleapis.com/auth/gmail.modify"}`
				case "/v1/userinfo":
					return `{"sub":"google-alice","email":"alice@example.test","email_verified":true}`
				case "/gmail/v1/users/me/profile":
					if change.CompareAndSwap(true, false) {
						tc.change(t, app)
					}
					return `{"emailAddress":"alice@example.test","historyId":"100"}`
				default:
					t.Errorf("unexpected Google path %s", r.URL.Path)
					return `{}`
				}
			})
			account, err := app.catalog.Account("alice")
			if err != nil {
				t.Fatal(err)
			}
			connections, err := app.personalConnections.List(account.ID)
			if err != nil || len(connections) != 1 {
				t.Fatal("fixture: alice's Google connection", connections, err)
			}
			ctx := context.WithValue(t.Context(), identityKey{}, "alice")
			status := connectors.PersonalOperation{Provider: "google", Generation: connections[0].Generation, Operation: "email.status", Arguments: map[string]any{}}
			if _, err = app.personalCall(ctx, status); err != nil {
				t.Fatal("fixture: the call failed without a change", err)
			}
			change.Store(true)
			_, err = app.personalCall(ctx, status)
			if change.Load() {
				t.Fatal("the call never reached Google")
			}
			if err == nil {
				t.Fatalf("returned Google's answer after a %s change during the call", tc.name)
			}
		})
	}
}
