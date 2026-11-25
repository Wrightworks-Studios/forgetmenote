package web

import (
	"crypto/hmac"
	"net/http"
	"strconv"
	"strings"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
)

const sessionCookieName = "forgetmenote"

// retiredSessionCookieName is no longer issued or accepted; logout still
// expires it in browsers that kept one.
const retiredSessionCookieName = "gitnotes"

// HTTPS sessions use the __Host- prefix: a sibling subdomain cannot set such a
// cookie, so it cannot plant a session for login CSRF. Plain-HTTP loopback
// development keeps sessionCookieName.
const hostSessionCookieName = "__Host-forgetmenote"

func (s *Server) secureCookies() bool { return strings.HasPrefix(s.origin, "https:") }
func (s *Server) canonicalSessionCookie() string {
	if s.secureCookies() {
		return hostSessionCookieName
	}
	return sessionCookieName
}

// Sessions issued before the __Host- name keep working until they move to it.
// Ambiguous cookies never choose one account or fall back to another token.
func sessionToken(r *http.Request) (string, bool) {
	var token string
	for _, line := range r.Header.Values("Cookie") {
		for _, field := range strings.Split(line, ";") {
			name, _, _ := strings.Cut(strings.TrimSpace(field), "=")
			name = strings.TrimSpace(name)
			if name != hostSessionCookieName && name != sessionCookieName {
				continue
			}
			cookies, err := http.ParseCookie(strings.TrimSpace(field))
			if err != nil || len(cookies) != 1 || cookies[0].Name != name {
				return "", false
			}
			value := cookies[0].Value
			if value == "" || token != "" && token != value {
				return "", false
			}
			token = value
		}
	}
	return token, token != ""
}
func (s *Server) setSessionCookie(w http.ResponseWriter, name, token string, age int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: token, Path: "/", HttpOnly: true, Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode, MaxAge: age})
}
func (s *Server) migrateSessionCookie(w http.ResponseWriter, r *http.Request) {
	if _, err := r.Cookie(s.canonicalSessionCookie()); err == nil {
		return
	}
	token, ok := sessionToken(r)
	if !ok {
		return
	}
	expiry := int64(0)
	if grant, found := s.oauth.Session(digest(token)); found && grant.Subject == subject(r.Context()) {
		expiry = grant.Expires
	}
	if expiry == 0 && subject(r.Context()) == catalog.Bootstrap {
		parts := strings.Split(token, ".")
		if len(parts) == 2 && hmac.Equal([]byte(parts[1]), []byte(s.sign(parts[0]))) {
			expiry, _ = strconv.ParseInt(parts[0], 10, 64)
		}
	}
	remaining := min(expiry-time.Now().Unix(), int64(43200))
	if remaining <= 0 {
		return
	}
	s.setSessionCookie(w, s.canonicalSessionCookie(), token, int(remaining))
}

func (s *Server) createSession(w http.ResponseWriter, id string) error {
	return s.createSessionAtEpoch(w, id, s.catalog.Epoch(id))
}
func (s *Server) createSessionAtEpoch(w http.ResponseWriter, id string, epoch uint64) error {
	if !s.catalog.Interactive(id) {
		return workspace.ErrForbidden
	}
	g := accessGrant{Subject: id, Expires: time.Now().Add(12 * time.Hour).Unix(), Epoch: epoch}
	if id == catalog.Bootstrap {
		g.RecoveryKey = digest(s.token)
	}
	token, err := s.oauth.CreateSession(g)
	if err != nil {
		return err
	}
	s.setSessionCookie(w, s.canonicalSessionCookie(), token, 43200)
	return nil
}
func (s *Server) browserSession(r *http.Request, token string) bool {
	g, ok := s.oauth.Session(digest(token))
	if g.Subject == catalog.Bootstrap && g.RecoveryKey != digest(s.token) {
		return false
	}
	if !ok || g.Expires <= time.Now().Unix() || !s.catalog.ValidSession(g.Subject, g.Epoch) || !s.catalog.Interactive(g.Subject) {
		return false
	}
	setSubject(r, g.Subject)
	return true
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if token, ok := sessionToken(r); ok {
		err := s.oauth.DeleteSession(digest(token))
		if err != nil {
			fail(w, err)
			return
		}
	}
	for _, name := range []string{s.canonicalSessionCookie(), retiredSessionCookieName} {
		s.setSessionCookie(w, name, "", -1)
	}
	if s.secureCookies() {
		s.setSessionCookie(w, sessionCookieName, "", -1)
	}
	http.Redirect(w, r, "/login", 303)
}
func (s *Server) revokeSessions(w http.ResponseWriter, r *http.Request) {
	var in struct {
		User string `json:"user"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.User == "" {
		in.User = subject(r.Context())
	}
	respond(w, map[string]bool{"ok": true}, s.catalog.Revoke(subject(r.Context()), in.User))
}
