package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStandardMCPWithoutLegacyGateway(t *testing.T) {
	app, _ := fixture(t)
	cookie := recoverySession(t, app)
	page := oidcRequest(app, "GET", "/", "", cookie)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Connect your AI") {
		t.Fatalf("website unavailable: %d %s", page.Code, page.Body.String())
	}
	for _, path := range []string{"/mcp/full", "/.well-known/oauth-protected-resource/mcp/full"} {
		r := httptest.NewRequest("GET", app.origin+path, nil)
		r.Header.Set("Authorization", "Bearer "+app.token)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("legacy alias %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest("POST", app.origin+"/", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	r.Header.Set("Authorization", "Bearer "+app.token)
	r.Header.Set("Origin", app.origin)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 405 {
		t.Fatalf("root interpreted MCP: %d %s", w.Code, w.Body.String())
	}
	r = httptest.NewRequest("POST", app.origin+"/mcp", strings.NewReader(`{}`))
	r.Header.Set("Cf-Access-Jwt-Assertion", "legacy-assertion")
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), app.origin+"/.well-known/oauth-protected-resource/mcp") {
		t.Fatalf("standard MCP auth bypassed: %d %s", w.Code, w.Body.String())
	}
	if len(app.automations.Definitions.List()) != 0 {
		t.Fatal("fresh instance installed organization jobs")
	}
}
