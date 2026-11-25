package web

import (
	"strings"
	"testing"
)

func TestHeadlessRetainsClientsAndAuthentication(t *testing.T) {
	app, _ := fixture(t)
	app.ConfigureHeadless(true)
	root := apiCall(app, "GET", "/", "", "")
	if root.Code != 200 || !strings.Contains(root.Body.String(), `"headless":true`) {
		t.Fatalf("headless root: %d %.100s", root.Code, root.Body.String())
	}
	for _, path := range []string{"/static/app.mjs", "/static/editor.mjs", "/custom.css", "/manifest.webmanifest"} {
		w := apiCall(app, "GET", path, "", testToken)
		if w.Code != 404 {
			t.Fatalf("UI route %s: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/healthz", "/readyz", "/login", "/.well-known/oauth-protected-resource/api/v1", "/api/v1/capabilities"} {
		w := apiCall(app, "GET", path, "", testToken)
		if w.Code != 200 {
			t.Fatalf("headless client route %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	w := apiCall(app, "POST", "/api/v1/actions/save_note", `{"project":"default","path":"headless.md","content":"headless","revision":""}`, testToken)
	if w.Code != 200 {
		t.Fatalf("headless save: %d %s", w.Code, w.Body.String())
	}
}
