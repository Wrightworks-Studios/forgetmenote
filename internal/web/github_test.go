package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/catalog"
)

type accountResult struct {
	Local     attribution.Identity   `json:"local"`
	Author    attribution.Identity   `json:"author"`
	GitHub    *catalog.GitHubAccount `json:"github"`
	Available bool                   `json:"githubAvailable"`
}

func accountSettings(t *testing.T, app *Server, user string) accountResult {
	t.Helper()
	w := proxyCall(app, user, "GET", "/account", "")
	var result accountResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("account: %d %s", w.Code, w.Body)
	}
	return result
}
func startGitHub(t *testing.T, app *Server, user string) (url.Values, *http.Cookie) {
	t.Helper()
	w := proxyCall(app, user, "POST", "/account/github/connect", `{}`)
	var result map[string]string
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	u, err := url.Parse(result["url"])
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("scope") != "" || u.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("excess scope or missing PKCE")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatal("missing session binding")
	}
	return u.Query(), cookies[0]
}
func callbackGitHub(app *Server, user, state, code string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", app.origin+"/account/github/callback?"+url.Values{"state": {state}, "code": {code}}.Encode(), nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Remote-User", user)
	req.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, req)
	return w
}
func TestAccountSettingsRequireAuthenticationAndDefaultToLocal(t *testing.T) {
	app, _ := fixture(t)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	a := accountSettings(t, app, "alice")
	if a.Local.ID == "" || a.Author != a.Local || a.GitHub != nil || a.Available {
		t.Fatalf("bad default: %+v", a)
	}
	w := call(app, "GET", "/account", "", "")
	if w.Code != 401 {
		t.Fatal("anonymous settings")
	}
	w = proxyCall(app, "alice", "POST", "/account/github/connect", `{}`)
	if w.Code != 503 {
		t.Fatal("unconfigured linking should be explicit")
	}
}
func TestGitHubLinkingOwnershipAttributionAndDisconnect(t *testing.T) {
	app, store := fixture(t)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner")
	var exchanges atomic.Int32
	var expectedChallenge string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			exchanges.Add(1)
			r.ParseForm()
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if r.Form.Get("client_secret") != "synthetic-secret" || r.Form.Get("redirect_uri") != app.origin+"/account/github/callback" || base64.RawURLEncoding.EncodeToString(sum[:]) != expectedChallenge {
				t.Error("missing PKCE or callback binding")
				http.Error(w, "invalid", 400)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"access_token": "synthetic-github-token", "scope": "", "token_type": "bearer"})
		case "/user":
			if r.Header.Get("Authorization") != "Bearer synthetic-github-token" {
				t.Error("profile lookup not authenticated")
			}
			json.NewEncoder(w).Encode(map[string]any{"id": 12345, "login": "astro-writer", "type": "User", "created_at": "2019-01-01T00:00:00Z"})
		default:
			t.Errorf("unexpected GitHub API: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	app.ConfigureGitHub("synthetic-client", "synthetic-secret")
	app.github.tokenURL = provider.URL + "/token"
	app.github.userURL = provider.URL + "/user"
	original := accountSettings(t, app, "alice")
	params, cookie := startGitHub(t, app, "alice")
	expectedChallenge = params.Get("code_challenge")
	for _, attempt := range []struct {
		user, state string
		cookie      *http.Cookie
	}{{"bob", params.Get("state"), cookie}, {"alice", "forged", cookie}, {"alice", params.Get("state"), nil}, {"alice", params.Get("state"), &http.Cookie{Name: cookie.Name, Value: "forged"}}} {
		w := callbackGitHub(app, attempt.user, attempt.state, "code", attempt.cookie)
		if w.Code != 403 {
			t.Fatalf("invalid callback accepted: %d", w.Code)
		}
	}
	if exchanges.Load() != 0 {
		t.Fatal("invalid callback contacted GitHub")
	}
	w := callbackGitHub(app, "alice", params.Get("state"), "code", cookie)
	if w.Code != 303 {
		t.Fatalf("connect: %d %s", w.Code, w.Body)
	}
	if callbackGitHub(app, "alice", params.Get("state"), "code", cookie).Code != 403 {
		t.Fatal("callback replay accepted")
	}
	linked := accountSettings(t, app, "alice")
	if linked.GitHub == nil || linked.GitHub.Login != "astro-writer" || linked.Author != original.Author || linked.Local.ID != original.Local.ID {
		t.Fatalf("link changed local identity or opted in without consent: %+v", linked)
	}
	for _, body := range []string{`{"email":"someone@users.noreply.github.com","enabled":true}`, `{"email":"12345+imposter@users.noreply.github.com","enabled":true}`, `{"email":"astro-writer@users.noreply.github.com","enabled":true}`, `{"email":"12345+astro-writer@users.noreply.github.com","enabled":true,"user":"bob"}`} {
		if proxyCall(app, "alice", "POST", "/account/github/attribution", body).Code == 200 {
			t.Fatal("impersonation accepted")
		}
	}
	w = proxyCall(app, "alice", "POST", "/account/github/attribution", `{"email":"12345+astro-writer@users.noreply.github.com","enabled":true}`)
	if w.Code != 200 {
		t.Fatalf("opt-in: %d %s", w.Code, w.Body)
	}
	provider.Close() // Subsequent local saves and disconnect cannot need GitHub.
	w = proxyCall(app, "alice", "POST", "/p/default/api/note", `{"path":"linked.md","content":"Linked author"}`)
	if w.Code != 200 {
		t.Fatalf("offline save %d %s", w.Code, w.Body)
	}
	git := gitIdentity(t, app, store)
	if git[0] != "astro-writer" || git[1] != "12345+astro-writer@users.noreply.github.com" || !strings.Contains(git[4], original.Local.ID) {
		t.Fatal("wrong linked identity", git)
	}
	w = proxyCall(app, "bob", "POST", "/account/github/disconnect", `{"user":"alice"}`)
	if w.Code == 200 {
		t.Fatal("cross-user disconnect accepted")
	}
	w = proxyCall(app, "alice", "POST", "/account/github/disconnect", `{}`)
	if w.Code != 200 {
		t.Fatal("disconnect failed")
	}
	disconnected := accountSettings(t, app, "alice")
	if disconnected.GitHub != nil || disconnected.Author != original.Author {
		t.Fatal("disconnect did not restore local identity")
	}
	note, _ := store.Read(context.Background(), "linked.md", "")
	body, _ := json.Marshal(map[string]string{"path": "linked.md", "content": "Local again", "revision": note.Revision})
	w = proxyCall(app, "alice", "POST", "/p/default/api/note", string(body))
	if w.Code != 200 {
		t.Fatal("local save after disconnect failed")
	}
	if got := gitIdentity(t, app, store); got[1] != original.Author.Email {
		t.Fatal("disconnect affected wrong identity", got)
	}
}
func TestGitHubPendingFlowExpiryAndDisconnect(t *testing.T) {
	app, _ := fixture(t)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	app.ConfigureGitHub("client", "secret")
	params, cookie := startGitHub(t, app, "alice")
	expireFlow(t, app, "github-link/pending", digest(params.Get("state")))
	if callbackGitHub(app, "alice", params.Get("state"), "code", cookie).Code != 403 {
		t.Fatal("expired flow accepted")
	}
	params, _ = startGitHub(t, app, "alice")
	p := pendingFlow[githubPending](t, app, "github-link/pending", digest(params.Get("state")))
	if proxyCall(app, "alice", "POST", "/account/github/disconnect", `{}`).Code != 200 {
		t.Fatal("disconnect pending flow")
	}
	err := app.catalog.LinkGitHub("alice", catalog.GitHubAccount{ID: 12345, Login: "astro-writer", Created: time.Now()}, p.Generation)
	if err == nil {
		t.Fatal("late OAuth callback undid disconnect")
	}
	req := httptest.NewRequest("POST", app.origin+"/account/github/connect", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal("account action bypassed origin check")
	}
}
