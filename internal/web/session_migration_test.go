package web

import (
	"forgetmenote/internal/catalog"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The gitnotes session alias is retired: sign-in no longer issues it, it no
// longer authenticates, and logout still expires it in browsers that kept one.
func TestSessionCookieRetiresGitnotesAliasAndRejectsConflicts(t *testing.T) {
	app, _ := fixture(t)
	w := httptest.NewRecorder()
	if err := app.createSession(w, "@local-admin"); err != nil {
		t.Fatal(err)
	}
	cookies := w.Result().Cookies()
	canonical := cookieNamed(cookies, "forgetmenote")
	if canonical == nil {
		t.Fatal("session did not issue the canonical cookie")
	}
	if cookieNamed(cookies, "gitnotes") != nil {
		t.Fatal("session still issued the retired gitnotes alias")
	}
	if got := oidcRequest(app, "GET", "/api/me", "", canonical); got.Code != 200 {
		t.Fatalf("canonical session rejected: %d %s", got.Code, got.Body)
	}
	legacy := &http.Cookie{Name: "gitnotes", Value: canonical.Value}
	if got := oidcRequest(app, "GET", "/api/me", "", legacy); got.Code != 401 {
		t.Fatalf("retired gitnotes cookie authenticated: %d", got.Code)
	}
	invalid := &http.Cookie{Name: "__Host-forgetmenote", Value: "invalid-canonical"}
	if got := oidcRequest(app, "GET", "/api/me", "", invalid, canonical); got.Code != 401 {
		t.Fatalf("conflicting session cookie fell back to another name: %d", got.Code)
	}
	got := oidcRequest(app, "POST", "/logout", "", canonical, legacy)
	if got.Code != 303 {
		t.Fatalf("logout: %d %s", got.Code, got.Body)
	}
	expired := map[string]bool{}
	for _, cookie := range got.Result().Cookies() {
		expired[cookie.Name] = cookie.MaxAge < 0
	}
	if !expired["forgetmenote"] || !expired["gitnotes"] {
		t.Fatal("logout did not expire the session cookie and the retired alias")
	}
	if got := oidcRequest(app, "GET", "/api/me", "", canonical); got.Code != 401 {
		t.Fatalf("logged-out session accepted: %d", got.Code)
	}
}

func TestProxyIdentityRequiresCanonicalSecretHeader(t *testing.T) {
	app, _ := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		peer    string
		headers map[string][]string
		code    int
	}{
		{"127.0.0.1:1234", map[string][]string{"X-ForgetMeNote-Proxy-Secret": {proxyTestSecret}}, 200},
		{"127.0.0.1:1234", map[string][]string{"X-Gitnotes-Proxy-Secret": {proxyTestSecret}}, 401},
		{"127.0.0.1:1234", map[string][]string{"X-ForgetMeNote-Proxy-Secret": {proxyTestSecret, "different-secret"}}, 401},
		{"192.0.2.1:1234", map[string][]string{"X-ForgetMeNote-Proxy-Secret": {proxyTestSecret}}, 401},
	} {
		r := httptest.NewRequest("GET", app.origin+"/api/me", nil)
		r.RemoteAddr = row.peer
		r.Header.Set("Remote-User", "migration-reader")
		for name, values := range row.headers {
			for _, value := range values {
				r.Header.Add(name, value)
			}
		}
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		if w.Code != row.code {
			t.Fatalf("%s %v: got %d, want %d", row.peer, row.headers, w.Code, row.code)
		}
	}
}

func TestMalformedSessionCookieCannotSelectAnotherAccount(t *testing.T) {
	app, _ := fixture(t)
	login := httptest.NewRecorder()
	if err := app.createSession(login, catalog.Bootstrap); err != nil {
		t.Fatal(err)
	}
	session := cookieNamed(login.Result().Cookies(), sessionCookieName)
	if session == nil {
		t.Fatal("missing session cookie")
	}
	server := httptest.NewServer(app)
	defer server.Close()
	for _, cookies := range []string{
		`__Host-forgetmenote="unterminated; forgetmenote=` + session.Value,
		`__Host-forgetmenote=bad\value; forgetmenote=` + session.Value,
		`forgetmenote=bad\value; __Host-forgetmenote=` + session.Value,
	} {
		t.Run(cookies, func(t *testing.T) {
			req, err := http.NewRequest("GET", server.URL+"/api/me", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = "localhost"
			req.Header.Set("Cookie", cookies)
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("invalid session cookie was silently dropped; another account accepted with HTTP %d", resp.StatusCode)
			}
		})
	}
}
