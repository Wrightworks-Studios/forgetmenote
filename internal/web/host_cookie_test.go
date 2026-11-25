package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg/pgtest"
)

func httpsFixture(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	projects, err := catalog.Open(dir, nil, pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { projects.Close() })
	app, err := New(projects, pgtest.Org(t), dir, "https://notes.example", testToken)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app
}

func cookieNamed(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// A sibling subdomain can set Domain cookies for the parent site, but never a
// __Host- cookie, so HTTPS sessions use that prefix.
func TestHTTPSSessionCookieUsesHostPrefix(t *testing.T) {
	app := httpsFixture(t)
	login := oidcRequest(app, "POST", "/login", url.Values{"token": {app.token}}.Encode())
	if login.Code != 303 {
		t.Fatalf("login: %d %s", login.Code, login.Body)
	}
	cookies := login.Result().Cookies()
	session := cookieNamed(cookies, "__Host-forgetmenote")
	if session == nil || !session.Secure || session.Path != "/" || session.Domain != "" || !session.HttpOnly {
		t.Fatalf("HTTPS session cookie is not a __Host- cookie: %#v", cookies)
	}
	if cookieNamed(cookies, "forgetmenote") != nil {
		t.Fatal("HTTPS login still issued the unprefixed session cookie")
	}
	if cookieNamed(cookies, "gitnotes") != nil {
		t.Fatal("HTTPS login still issued the retired gitnotes alias")
	}
	if w := oidcRequest(app, "GET", "/api/me", "", session); w.Code != 200 {
		t.Fatalf("__Host- session rejected: %d", w.Code)
	}

	// Sessions issued before the prefix keep working and move to it.
	earlier := &http.Cookie{Name: "forgetmenote", Value: session.Value}
	renewed := oidcRequest(app, "GET", "/api/me", "", earlier)
	if renewed.Code != 200 {
		t.Fatalf("existing unprefixed session logged out: %d", renewed.Code)
	}
	if moved := cookieNamed(renewed.Result().Cookies(), "__Host-forgetmenote"); moved == nil || moved.Value != session.Value || moved.MaxAge <= 0 || !moved.Secure || moved.Path != "/" {
		t.Fatal("existing session was not moved to the __Host- name")
	}
	planted := &http.Cookie{Name: "forgetmenote", Value: "planted-by-sibling"}
	if w := oidcRequest(app, "GET", "/api/me", "", session, planted); w.Code != 401 {
		t.Fatalf("conflicting planted cookie was ignored: %d", w.Code)
	}

	out := oidcRequest(app, "POST", "/logout", "", session)
	for _, name := range []string{"__Host-forgetmenote", "forgetmenote", "gitnotes"} {
		if c := cookieNamed(out.Result().Cookies(), name); c == nil || c.MaxAge >= 0 {
			t.Fatalf("logout did not expire %s", name)
		}
	}
	if w := oidcRequest(app, "GET", "/api/me", "", session); w.Code != 401 {
		t.Fatalf("logged-out session accepted: %d", w.Code)
	}
}

func TestHTTPSOIDCBindingCookieUsesHostPrefix(t *testing.T) {
	app := httpsFixture(t)
	p := provider(t)
	if err := app.ConfigureOIDC(p.server.URL, "test-client", "test-secret"); err != nil {
		t.Fatal(err)
	}
	state, binding := beginOIDC(t, app, p, "/auth/oidc/login")
	if binding.Name != "__Host-gitnotes-oidc" || !binding.Secure || binding.Path != "/" || binding.Domain != "" {
		t.Fatalf("OIDC binding cookie is not a __Host- cookie: %#v", binding)
	}
	if w := finishOIDC(app, state, &http.Cookie{Name: "gitnotes-oidc", Value: binding.Value}); w.Code != 403 {
		t.Fatalf("unprefixed binding cookie accepted on HTTPS: %d", w.Code)
	}
	state, binding = beginOIDC(t, app, p, "/auth/oidc/login")
	w := finishOIDC(app, state, binding)
	if w.Code != 303 {
		t.Fatalf("callback: %d %s", w.Code, w.Body)
	}
	cleared := cookieNamed(w.Result().Cookies(), "__Host-gitnotes-oidc")
	if cleared == nil || cleared.MaxAge >= 0 || cleared.Path != "/" {
		t.Fatal("binding cookie was not cleared at its own path")
	}
	if cookieNamed(w.Result().Cookies(), "__Host-forgetmenote") == nil {
		t.Fatal("OIDC sign-in did not issue the __Host- session")
	}
}

// Plain-HTTP loopback development keeps the existing cookie names.
func TestHTTPDevelopmentKeepsCookieNames(t *testing.T) {
	app, _ := fixture(t)
	w := httptest.NewRecorder()
	if err := app.createSession(w, catalog.Bootstrap); err != nil {
		t.Fatal(err)
	}
	cookies := w.Result().Cookies()
	if cookieNamed(cookies, "forgetmenote") == nil || cookieNamed(cookies, "__Host-forgetmenote") != nil {
		t.Fatalf("HTTP session cookie names changed: %#v", cookies)
	}
	p := provider(t)
	if err := app.ConfigureOIDC(p.server.URL, "test-client", "test-secret"); err != nil {
		t.Fatal(err)
	}
	_, binding := beginOIDC(t, app, p, "/auth/oidc/login")
	if binding.Name != "gitnotes-oidc" || binding.Path != "/auth/oidc/callback" {
		t.Fatalf("HTTP binding cookie changed: %#v", binding)
	}
}
