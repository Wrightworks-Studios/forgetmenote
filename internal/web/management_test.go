package web

import (
	"encoding/json"
	"forgetmenote/internal/catalog"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkspaceManagementHTTPAndMCP(t *testing.T) {
	app, _ := fixture(t)
	adminCall := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, app.origin+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+testToken)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		return w
	}
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []catalog.User{{ID: "alice", DisplayName: "Alice"}, {ID: "bob", DisplayName: "Bob Builder"}, {ID: "outsider", DisplayName: "Outside"}, {ID: "disabled", DisplayName: "Bob Disabled", Disabled: true}} {
		if err := app.catalog.SetUser(catalog.Bootstrap, u); err != nil {
			t.Fatal(err)
		}
	}
	w := adminCall("POST", "/api/users", `{"id":"alice","canCreateProjects":true}`)
	if w.Code != 200 {
		t.Fatalf("grant: %d %s", w.Code, w.Body.String())
	}
	w = proxyCall(app, "alice", "POST", "/api/projects", `{"id":"alice-space","name":"Alice space"}`)
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if w = proxyCall(app, "bob", "GET", "/api/people?project=alice-space&q=bo", ""); w.Code != 403 {
		t.Fatal("non-owner searched directory", w.Code)
	}
	w = proxyCall(app, "alice", "GET", "/api/people?project=alice-space&q=bo", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Bob Builder") || strings.Contains(w.Body.String(), "Disabled") {
		t.Fatalf("people: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "admin") || strings.Contains(w.Body.String(), "password") {
		t.Fatal("search leaked account state")
	}
	w = proxyCall(app, "alice", "POST", "/api/members", `{"project":"alice-space","user":"bob","role":"edit"}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = proxyCall(app, "alice", "GET", "/api/admin", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "outsider") || !strings.Contains(w.Body.String(), "Bob Builder") {
		t.Fatalf("owned members: %s", w.Body.String())
	}
	if w = proxyCall(app, "alice", "POST", "/api/users", `{"id":"alice","admin":true}`); w.Code != 403 {
		t.Fatal("owner escalated")
	}
	if w = proxyCall(app, "alice", "POST", "/api/members", `{"project":"alice-space","user":"alice","role":"edit"}`); w.Code != 409 {
		t.Fatal("last owner removal did not conflict", w.Code)
	}
	client := actionClient(t, app, "alice", "notes:read notes:write notes:manage")
	actionCall(t, client, "find_workspace_people", map[string]any{"project": "alice-space", "query": "Bob"}, false)
	actionCall(t, client, "create_project", map[string]any{"id": "ai-space", "name": "AI space"}, false)
	actionCall(t, client, "set_user_access", map[string]any{"id": "alice", "admin": true}, true)
	actionCall(t, client, "set_project_member", map[string]any{"project": "ai-space", "user": "alice", "role": ""}, true)
	actionCall(t, client, "set_project_member", map[string]any{"project": "default", "user": "alice", "role": "owner"}, true)
	// An older client editing only Admin must preserve the newer creation grant/name.
	w = adminCall("POST", "/api/users", `{"id":"alice","admin":false}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = adminCall("GET", "/api/admin", "")
	var data struct{ Users []catalog.User }
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	for _, u := range data.Users {
		if u.ID == "alice" && (!u.CanCreateProjects || u.DisplayName != "Alice") {
			t.Fatal("partial update lost fields", u)
		}
	}
	// Revoking workspace creation leaves current ownership intact.
	w = adminCall("POST", "/api/users", `{"id":"alice","canCreateProjects":false}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	actionCall(t, client, "create_project", map[string]any{"id": "denied", "name": "Denied"}, true)
	actionCall(t, client, "administration", map[string]any{}, false)
}
