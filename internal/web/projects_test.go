package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
)

const proxyTestSecret = "synthetic-proxy-secret-0123456789abcdef"

func proxyCall(app *Server, user, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, app.origin+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Remote-User", user)
	r.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
	r.Header.Set("Origin", app.origin)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	return w
}
func TestAutheliaIdentityAndProjectPermissionMatrix(t *testing.T) {
	app, _ := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	for user, role := range map[string]string{"reader": "read", "reviewer": "comment", "writer": "edit"} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", user, role); err != nil {
			t.Fatal(err)
		}
	}
	saved := proxyCall(app, "writer", "POST", "/p/default/api/note", `{"path":"notes/a.md","content":"Author text","revision":""}`)
	if saved.Code != 200 {
		t.Fatalf("writer: %d %s", saved.Code, saved.Body.String())
	}
	for _, row := range []struct {
		user, method, path, body string
		code                     int
	}{
		{"reader", "GET", "/p/default/api/note?path=notes/a.md", "", 200},
		{"reviewer", "POST", "/p/default/api/note", `{"path":"notes/a.md","content":"no","revision":""}`, 403},
		{"reader", "POST", "/p/default/api/comments", `{}`, 403},
		{"writer", "POST", "/api/projects", `{"id":"unauthorized","name":"No"}`, 403},
		{"writer", "POST", "/api/users", `{"id":"writer","admin":true}`, 403},
		{"stranger", "GET", "/p/default/api/backup", "", 403},
		{"stranger", "GET", "/p/default/assets/assets/image.png", "", 403},
	} {
		w := proxyCall(app, row.user, row.method, row.path, row.body)
		if w.Code != row.code {
			t.Errorf("%s %s: %d %s", row.user, row.path, w.Code, w.Body.String())
		}
	}
	comments := proxyCall(app, "reviewer", "GET", "/p/default/api/comments?path=notes/a.md", "")
	var discussion map[string]any
	json.Unmarshal(comments.Body.Bytes(), &discussion)
	body, _ := json.Marshal(map[string]any{"path": "notes/a.md", "text": "A question", "revision": discussion["revision"]})
	w := proxyCall(app, "reviewer", "POST", "/p/default/api/comments", string(body))
	if w.Code != 200 {
		t.Fatalf("commenter cannot comment: %d %s", w.Code, w.Body.String())
	}
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "writer", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if w = proxyCall(app, "writer", "GET", "/api/projects", ""); w.Code != 401 {
		t.Fatal("disabled SSO user authenticated")
	}
}
func TestForgedIdentityHeadersFailClosed(t *testing.T) {
	app, _ := fixture(t)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	for _, pair := range [][2]string{{"127.0.0.1:10", "wrong"}, {"192.0.2.1:10", proxyTestSecret}} {
		r := httptest.NewRequest("GET", app.origin+"/api/projects", nil)
		r.RemoteAddr = pair[0]
		r.Header.Set("Remote-User", "admin")
		r.Header.Set("X-ForgetMeNote-Proxy-Secret", pair[1])
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("forged identity accepted: %d", w.Code)
		}
	}
}

func TestOwnerHTTPAuthorityIsProjectScoped(t *testing.T) {
	app, store := fixture(t)
	lockManuscript(t, store)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "owner", "owner"); err != nil {
		t.Fatal(err)
	}
	app.catalog.Member(catalog.Bootstrap, "default", "editor", "edit")
	app.catalog.Create(catalog.Bootstrap, "other", "Other")
	state, err := store.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"path": "manuscript", "locked": false, "revision": state.Revision})
	for _, row := range []struct {
		user, method, path, body string
		code                     int
	}{
		{"editor", "POST", "/p/default/api/locks", string(raw), 403},
		{"owner", "POST", "/p/other/api/locks", string(raw), 403},
		{"owner", "POST", "/p/default/api/locks", string(raw), 200},
		{"owner", "POST", "/api/members", `{"project":"default","user":"reader","role":"read"}`, 200},
		{"owner", "POST", "/api/users", `{"id":"owner","admin":true}`, 403},
		{"owner", "GET", "/p/default/api/history", "", 200},
		{"owner", "GET", "/p/other/api/history", "", 403},
	} {
		w := proxyCall(app, row.user, row.method, row.path, row.body)
		if w.Code != row.code {
			t.Errorf("%s %s: %d %s", row.user, row.path, w.Code, w.Body.String())
		}
	}
	w := proxyCall(app, "owner", "GET", "/api/admin", "")
	var data struct {
		Users    []catalog.User
		Projects []catalog.Project
	}
	if err = json.Unmarshal(w.Body.Bytes(), &data); err != nil || len(data.Users) != 3 || len(data.Projects) != 1 || data.Projects[0].ID != "default" {
		t.Fatalf("owner visibility: %s %v", w.Body.String(), err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", "owner", "edit"); err != nil {
		t.Fatal(err)
	}
	if w = proxyCall(app, "owner", "GET", "/api/admin", ""); w.Code != 403 {
		t.Fatal("owner revocation did not take effect")
	}
}

func TestRemoteOperationsAreNotExposed(t *testing.T) {
	app, _ := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "admin", Admin: true}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/push", "/p/default/api/push", "/api/remote", "/p/default/api/remote"} {
		w := proxyCall(app, "admin", "POST", path, `{"url":"git@example.com:synthetic/backup.git"}`)
		if w.Code != 404 {
			t.Errorf("retired endpoint %s returned %d, want 404", path, w.Code)
		}
	}
	w := proxyCall(app, "admin", "GET", "/", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Download complete Git bundle") {
		t.Fatal("local project export must remain available")
	}
	for _, control := range []string{`id="remote-form"`, `id="push"`} {
		if strings.Contains(w.Body.String(), control) {
			t.Errorf("retired control remains: %s", control)
		}
	}
}

// The project gate historically authorizes before returning 404 for an unsupported
// method. These statuses, including unusual read-only POSTs, are caller-visible.
func TestProjectRouteMethodCompatibility(t *testing.T) {
	app, _ := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	roles := []string{"read", "comment", "edit", "owner"}
	for _, role := range roles {
		if err := app.catalog.Member(catalog.Bootstrap, "default", role, role); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		method, path, body string
		codes              [4]int
	}{
		{"HEAD", "/api/status", "", [4]int{200, 200, 200, 200}},
		{"POST", "/api/preview", `{"content":"# Preview"}`, [4]int{200, 200, 200, 200}},
		{"POST", "/api/knowledge/search", "invalid json", [4]int{400, 400, 400, 400}},
		{"POST", "/api/knowledge/hybrid-search", "invalid json", [4]int{400, 400, 400, 400}},
		{"POST", "/api/comments", "invalid json", [4]int{403, 400, 400, 400}},
		{"POST", "/api/knowledge/findings", "invalid json", [4]int{403, 400, 400, 400}},
		{"POST", "/api/knowledge/proposals", "invalid json", [4]int{403, 400, 400, 400}},
		{"POST", "/api/note", "invalid json", [4]int{403, 403, 400, 400}},
		{"POST", "/api/knowledge/config", "invalid json", [4]int{403, 403, 403, 400}},
		{"POST", "/api/knowledge/retrieval-settings", "invalid json", [4]int{403, 403, 403, 400}},
		{"POST", "/api/trash", "invalid json", [4]int{403, 403, 403, 400}},
		{"POST", "/api/locks", "invalid json", [4]int{403, 403, 403, 400}},
		{"PUT", "/api/note", "", [4]int{403, 403, 404, 404}},
		{"POST", "/api/status", "", [4]int{403, 403, 404, 404}},
		{"PUT", "/api/comments/resolve", "", [4]int{403, 404, 404, 404}},
		{"PUT", "/api/preview", "", [4]int{404, 404, 404, 404}},
		{"PUT", "/api/knowledge/search", "", [4]int{404, 404, 404, 404}},
		{"PUT", "/api/knowledge/config", "", [4]int{403, 403, 403, 404}},
		{"HEAD", "/api/knowledge/config", "", [4]int{404, 404, 404, 404}},
		{"PUT", "/api/knowledge/proposals/decide", "", [4]int{403, 404, 404, 404}},
		{"HEAD", "/api/move", "", [4]int{403, 403, 403, 404}},
		{"GET", "/api/duplicate", "", [4]int{403, 403, 403, 404}},
		{"PUT", "/api/trash/unknown", "", [4]int{403, 403, 403, 404}},
		{"POST", "/api/trash-unknown", "", [4]int{403, 403, 403, 404}},
		{"PUT", "/api/chat/unknown/extra", "", [4]int{404, 404, 404, 404}},
		{"POST", "/api/unknown", "", [4]int{403, 403, 404, 404}},
		{"GET", "/api/unknown", "", [4]int{404, 404, 404, 404}},
	} {
		for i, role := range roles {
			t.Run(role+"/"+row.method+row.path, func(t *testing.T) {
				got := proxyCall(app, role, row.method, "/p/default"+row.path, row.body)
				if got.Code != row.codes[i] {
					t.Fatalf("status %d, want %d: %s", got.Code, row.codes[i], got.Body.String())
				}
			})
		}
	}
	for _, row := range []struct {
		method, path string
		code         int
	}{
		{"GET", "/api/status", 400}, {"HEAD", "/api/status", 400}, {"POST", "/api/preview", 400},
		{"POST", "/api/status", 404}, {"GET", "/p/absent/api/status", 403}, {"GET", "/p/default", 404},
	} {
		if got := proxyCall(app, "owner", row.method, row.path, `{}`); got.Code != row.code {
			t.Errorf("%s %s: %d, want %d", row.method, row.path, got.Code, row.code)
		}
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "owner", ""); err != nil {
		t.Fatal(err)
	}
	if got := proxyCall(app, "owner", "HEAD", "/p/default/api/status", ""); got.Code != 403 {
		t.Fatalf("revoked route still accessible: %d", got.Code)
	}
}

func TestUnclassifiedProjectRouteFailsClosed(t *testing.T) {
	app, _ := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "reader", "read"); err != nil {
		t.Fatal(err)
	}
	called := false
	app.routes.Mux.HandleFunc("GET /api/unclassified", func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(200) })
	got := proxyCall(app, "reader", "GET", "/p/default/api/unclassified", "")
	if got.Code != 403 || called {
		t.Fatalf("route without explicit authority dispatched: status=%d called=%v", got.Code, called)
	}

}
