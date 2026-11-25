package web

import "net/http"

// A connectionBinding names the cookie that ties a pending login or provider
// connection to the browser that started it. The cookie is HttpOnly,
// SameSite=Lax, Secure on HTTPS and lasts the ten-minute pending window.
// A hostOnly binding becomes a __Host- cookie (Secure, Path=/, no Domain) on
// HTTPS so a sibling subdomain cannot plant one; pending logins do not survive a
// restart, so no older name is read.
type connectionBinding struct {
	name, path string
	hostOnly   bool
}

var (
	oidcBinding       = connectionBinding{name: "gitnotes-oidc", path: "/auth/oidc/callback", hostOnly: true}
	githubLinkBinding = connectionBinding{name: "gitnotes-github-link", path: "/account/github"}
	repositoryBinding = connectionBinding{name: "fmn-repository-connect", path: "/"}
	backupBinding     = connectionBinding{name: "fmn-backup-connect", path: "/"}
)

func (s *Server) bindingCookie(b connectionBinding, value string, age int) *http.Cookie {
	c := &http.Cookie{Name: b.name, Value: value, Path: b.path, HttpOnly: true, Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode, MaxAge: age}
	if b.hostOnly && c.Secure {
		c.Name, c.Path = "__Host-"+b.name, "/"
	}
	return c
}
func (s *Server) setBinding(w http.ResponseWriter, b connectionBinding, value string) {
	http.SetCookie(w, s.bindingCookie(b, value, 600))
}
func (s *Server) clearBinding(w http.ResponseWriter, b connectionBinding) {
	http.SetCookie(w, s.bindingCookie(b, "", -1))
}

// binding returns the browser's binding value, or http.ErrNoCookie.
func (s *Server) binding(r *http.Request, b connectionBinding) (string, error) {
	c, err := r.Cookie(s.bindingCookie(b, "", 0).Name)
	if err != nil {
		return "", err
	}
	return c.Value, nil
}
