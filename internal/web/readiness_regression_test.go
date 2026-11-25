package web

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func tokenForm(app *Server, path string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", app.origin+path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	return w
}
func TestUnknownRevocationDoesNotWrite(t *testing.T) {
	app, _ := fixture(t)
	before, _, err := app.db.ReadDocument(t.Context(), app.db.Pool, "oauth", "state", -1)
	if err != nil {
		t.Fatal(err)
	}
	w := tokenForm(app, "/oauth/revoke", url.Values{"token": {"unknown"}})
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	if after, _, err := app.db.ReadDocument(t.Context(), app.db.Pool, "oauth", "state", -1); err != nil || after != before {
		t.Fatalf("unknown token wrote credential store: version %d to %d, %v", before, after, err)
	}
}
func TestRefreshReplayRevokesDescendantAfterRestart(t *testing.T) {
	app, _ := fixture(t)
	app.oauth.Refresh[digest("old")] = withOAuthFamily(accessGrant{Client: "client", Subject: "@local-admin", Resource: app.origin + "/mcp", Expires: time.Now().Add(time.Hour).Unix()})
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {"client"}, "refresh_token": {"old"}}
	if w := tokenForm(app, "/oauth/token", form); w.Code != 200 {
		t.Fatalf("rotate: %s", w.Body)
	}
	var err error
	app.oauth, err = reopenOAuth(app)
	if err != nil {
		t.Fatal(err)
	}
	used := app.oauth.Used[digest("old")]
	used.UsedAt = time.Now().Add(-31 * time.Second).Unix()
	app.oauth.Used[digest("old")] = used
	if w := tokenForm(app, "/oauth/token", form); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if len(app.oauth.Refresh) != 0 || len(app.oauth.Access) != 0 {
		t.Fatal("replayed family still authorized")
	}
	app.oauth, err = reopenOAuth(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(app.oauth.Refresh) != 0 {
		t.Fatal("revocation not persisted")
	}
}
func TestUnscopedProjectRoutesReject(t *testing.T) {
	app, _ := fixture(t)
	for _, path := range []string{"/api/knowledge/status", "/api/note", "/api/browse", "/api/knowledge/snapshot"} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest("GET", app.origin+path, nil)
			r.Header.Set("Authorization", "Bearer "+app.token)
			w := httptest.NewRecorder()
			app.ServeHTTP(w, r)
			if w.Code != 400 {
				t.Fatalf("expected project-required, got %d", w.Code)
			}
		})
	}
}

func TestSourceAdmissionRejectsBeforeBodyDecode(t *testing.T) {
	app, _ := fixture(t)
	for i := 0; i < cap(app.sourceWork); i++ {
		app.sourceWork <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(app.sourceWork); i++ {
			<-app.sourceWork
		}
	}()
	for _, path := range []string{"/api/sources/preview", "/api/sources/import", "/api/sources/jobs"} {
		w := call(app, "POST", path, "invalid-json", app.token)
		if w.Code != 429 {
			t.Fatalf("%s decoded rejected body: %d %s", path, w.Code, w.Body)
		}
	}
	// The published action API retains its original generic capacity error;
	// the transport extraction must not silently change an existing client wire.
	w := apiCall(app, "POST", "/api/v1/actions/archive_source", `{"project":"default","filename":"queued.txt","data":"cXVldWVk"}`, app.token)
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if w.Code != 400 || json.Unmarshal(w.Body.Bytes(), &payload) != nil || payload.Error.Code != "invalid_request" || payload.Error.Message != "source import capacity reached; retry shortly" {
		t.Fatalf("source action capacity contract changed: %d %s", w.Code, w.Body)
	}
}

func TestRevocationBoundedBody(t *testing.T) {
	app, _ := fixture(t)
	if w := tokenForm(app, "/oauth/revoke", url.Values{"token": {strings.Repeat("x", 20000)}}); w.Code != 400 {
		t.Fatal(w.Code)
	}
}

func TestOAuthAbuseLimitIsIndependent(t *testing.T) {
	app, _ := fixture(t)
	app.oauth.Refresh[digest("valid")] = withOAuthFamily(accessGrant{Client: "client", Subject: "@local-admin", Resource: app.origin + "/mcp", Expires: time.Now().Add(time.Hour).Unix()})
	for i := 0; i < 240; i++ {
		if w := tokenForm(app, "/oauth/revoke", url.Values{"token": {"unknown"}}); w.Code != 200 {
			t.Fatal(i, w.Code)
		}
	}
	if w := tokenForm(app, "/oauth/revoke", url.Values{"token": {"unknown"}}); w.Code != 429 {
		t.Fatal(w.Code)
	}
	if rateWindows(t, app, "passwords") != 0 {
		t.Fatal("OAuth consumed password quota")
	}
	if w := tokenForm(app, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {"client"}, "refresh_token": {"valid"}}); w.Code != 200 {
		t.Fatalf("revocation traffic blocked legitimate refresh: %d", w.Code)
	}

}

func TestRefreshReportsActualRemainingLifetime(t *testing.T) {
	app, _ := fixture(t)
	expiry := time.Now().Add(30 * time.Second).Unix()
	app.oauth.Refresh[digest("ending")] = accessGrant{Client: "client", Subject: "@local-admin", Resource: app.origin + "/mcp", Family: "ending", FamilyExpires: expiry, Expires: expiry}
	w := tokenForm(app, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {"client"}, "refresh_token": {"ending"}})
	var out struct {
		Expires int64 `json:"expires_in"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || out.Expires < 1 || out.Expires > 30 {
		t.Fatalf("incorrect lifetime %d: %s", out.Expires, w.Body)
	}
}
