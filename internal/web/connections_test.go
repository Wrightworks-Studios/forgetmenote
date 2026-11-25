package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"forgetmenote/internal/reposource"
)

func TestHeadlessAuthorityStatusAndGitHubBrowserBridge(t *testing.T) {
	app, _ := fixture(t)
	app.ConfigureHeadless(true)
	t.Cleanup(func() { app.repositories = nil })
	status := apiCall(app, "POST", "/api/v1/actions/notebook_authority_status", `{"project":"default"}`, testToken)
	if status.Code != 200 || !strings.Contains(status.Body.String(), `"type":"forgetmenote"`) {
		t.Fatalf("status: %d %s", status.Code, status.Body.String())
	}
	app.repositoryConnections = &repositoryConnections{github: &reposource.GitHub{Config: reposource.Config{ClientID: "fixture"}}}
	start := apiCall(app, "POST", "/api/v1/actions/begin_github_connection", `{"project":"new-notebook","name":"New notebook","purpose":"source","creating":true}`, testToken)
	if start.Code != 200 {
		t.Fatalf("start: %d %s", start.Code, start.Body.String())
	}
	var ticket struct{ ID, URL string }
	if err := json.Unmarshal(start.Body.Bytes(), &ticket); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ticket.URL, app.origin+"/connections/start?") {
		t.Fatalf("not a same-origin browser bridge: %s", ticket.URL)
	}
	pending := apiCall(app, "POST", "/api/v1/actions/github_connection_status", `{"connection":"`+ticket.ID+`"}`, testToken)
	if pending.Code != 200 || !strings.Contains(pending.Body.String(), `"ready":false`) {
		t.Fatalf("pending: %d %s", pending.Code, pending.Body.String())
	}
	r := httptest.NewRequest("GET", ticket.URL, nil)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/login?next=") {
		t.Fatalf("login bridge: %d %s", w.Code, w.Body.String())
	}
	r = httptest.NewRequest("GET", ticket.URL, nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	w = httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "https://github.com/login/oauth/authorize?") || len(w.Result().Cookies()) != 1 {
		t.Fatalf("GitHub redirect: %d %s", w.Code, w.Body.String())
	}
	again := apiCall(app, "GET", strings.TrimPrefix(ticket.URL, app.origin), "", testToken)
	if again.Code != 403 {
		t.Fatalf("browser start replay: %d", again.Code)
	}
	denied := apiCall(app, "POST", "/api/v1/actions/finish_github_connection", `{"connection":"`+ticket.ID+`","repository":42,"installation":7,"shareWithNotebook":true}`, testToken)
	if denied.Code != 403 {
		t.Fatalf("unverified selection: %d %s", denied.Code, denied.Body.String())
	}
}

func TestHeadlessGitHubCallbackCompletesVerifiedNotebookCreation(t *testing.T) {
	app, _, _ := authorityFixture(t)
	app.ConfigureHeadless(true)
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]string
		if json.NewDecoder(r.Body).Decode(&request) != nil || request["code_verifier"] == "" || request["redirect_uri"] != app.origin+"/repository/github/callback" {
			t.Error("lost callback binding")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"synthetic-user","token_type":"bearer"}`))
	}))
	defer tokens.Close()
	app.repositoryConnections.github.TokenURL = tokens.URL
	w := apiCall(app, "POST", "/api/v1/actions/begin_github_connection", `{"project":"github-new","name":"GitHub new","purpose":"source","creating":true}`, testToken)
	var ticket struct{ ID, URL string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &ticket) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	w = apiCall(app, "GET", strings.TrimPrefix(ticket.URL, app.origin), "", testToken)
	if w.Code != 303 || len(w.Result().Cookies()) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	redirect, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	callback := httptest.NewRequest("GET", app.origin+"/repository/github/callback?"+url.Values{"state": {redirect.Query().Get("state")}, "code": {"synthetic-code"}}.Encode(), nil)
	callback.Header.Set("Authorization", "Bearer "+testToken)
	callback.AddCookie(w.Result().Cookies()[0])
	w = httptest.NewRecorder()
	app.ServeHTTP(w, callback)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Return to your terminal") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = apiCall(app, "POST", "/api/v1/actions/github_connection_status", `{"connection":"`+ticket.ID+`"}`, testToken)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"full_name":"example/notebook"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	finish := `{"connection":"` + ticket.ID + `","repository":42,"installation":7,"shareWithNotebook":true}`
	for i := 0; i < 2; i++ {
		w = apiCall(app, "POST", "/api/v1/actions/finish_github_connection", finish, testToken)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if app.catalog.Authority("github-new").Type != "github" {
		t.Fatal("source notebook was not read-only")
	}
	w = httptest.NewRecorder()
	app.ServeHTTP(w, callback)
	if w.Code != 403 {
		t.Fatal("callback was reusable", w.Code)
	}
}
