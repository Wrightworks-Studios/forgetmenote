package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Binding cookies keep their names and attributes; only the OIDC binding takes
// the __Host- form on HTTPS, and then its plain name is not read.
func TestConnectionBindingCookies(t *testing.T) {
	plain, _ := fixture(t)
	secure := httpsFixture(t)
	for _, tc := range []struct {
		app        *Server
		b          connectionBinding
		name, path string
	}{
		{plain, oidcBinding, "gitnotes-oidc", "/auth/oidc/callback"},
		{secure, oidcBinding, "__Host-gitnotes-oidc", "/"},
		{plain, githubLinkBinding, "gitnotes-github-link", "/account/github"},
		{secure, githubLinkBinding, "gitnotes-github-link", "/account/github"},
		{plain, repositoryBinding, "fmn-repository-connect", "/"},
		{secure, repositoryBinding, "fmn-repository-connect", "/"},
		{plain, backupBinding, "fmn-backup-connect", "/"},
		{secure, backupBinding, "fmn-backup-connect", "/"},
	} {
		w := httptest.NewRecorder()
		tc.app.setBinding(w, tc.b, "bound")
		tc.app.clearBinding(w, tc.b)
		cookies := w.Result().Cookies()
		if len(cookies) != 2 || cookies[0].Value != "bound" || cookies[1].Value != "" {
			t.Fatalf("%s cookies = %#v", tc.name, cookies)
		}
		for i, age := range []int{600, -1} {
			c := cookies[i]
			if c.Name != tc.name || c.Path != tc.path || c.Domain != "" || !c.HttpOnly || c.Secure != (tc.app == secure) || c.SameSite != http.SameSiteLaxMode || c.MaxAge != age {
				t.Errorf("%s cookie %d = %#v", tc.name, i, c)
			}
		}
		r := httptest.NewRequest("GET", "/", nil)
		r.AddCookie(&http.Cookie{Name: tc.b.name + "-other", Value: "x"})
		if _, err := tc.app.binding(r, tc.b); !errors.Is(err, http.ErrNoCookie) {
			t.Errorf("%s without cookie: %v", tc.name, err)
		}
		r.AddCookie(cookies[0])
		if v, err := tc.app.binding(r, tc.b); err != nil || v != "bound" {
			t.Errorf("%s binding = %q, %v", tc.name, v, err)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: "gitnotes-oidc", Value: "planted"})
	if _, err := secure.binding(r, oidcBinding); !errors.Is(err, http.ErrNoCookie) {
		t.Fatalf("HTTPS OIDC binding read the plain name: %v", err)
	}
}
