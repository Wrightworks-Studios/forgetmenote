package web

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg"
)

// Configuration controls login availability, never provider health.
func (s *Server) localPasswords() bool { return s.oidc == nil && s.proxy == nil }

// passwordRequests charges one-minute request windows that every gateway
// shares in Postgres under group; active bounds this process's concurrent
// password work.
type passwordRequests struct {
	db     *pg.DB
	group  string
	mu     sync.Mutex
	active int
}

// take charges one request to each key's window, or refuses without charging
// when any window is at its limit or the group already holds 1024 windows.
// Keys are digested, so a password typed as a user name is never stored.
func (p *passwordRequests) take(ctx context.Context, keys []string, limits []int) (bool, error) {
	hashed := make([]string, len(keys))
	for i, k := range keys {
		hashed[i] = digest(k)
	}
	return p.db.Take(ctx, p.group, hashed, limits, 1024)
}

func (p *passwordRequests) refund(ctx context.Context, keys []string) error {
	hashed := make([]string, len(keys))
	for i, k := range keys {
		hashed[i] = digest(k)
	}
	return p.db.Refund(ctx, p.group, hashed)
}

// limited answers a request that take refused or could not charge.
func limited(w http.ResponseWriter, err error, message string) {
	if err != nil {
		http.Error(w, "Sign-in limits are unavailable; try again shortly", 503)
		return
	}
	w.Header().Set("Retry-After", "60")
	http.Error(w, message, 429)
}

// socketPeer is the connecting address, never an untrusted forwarding header.
func socketPeer(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Bound expensive password work, and throttle both account guessing and spraying.
// Use the actual peer, never untrusted forwarding headers. A reverse proxy shares
// its peer budget; operators can add their own edge limits for larger deployments.
func (s *Server) passwordRequest(w http.ResponseWriter, r *http.Request, name string) (func(), bool) {
	p := &s.passwords
	p.mu.Lock()
	busy := p.active >= 4
	if !busy {
		p.active++
	}
	p.mu.Unlock()
	done := func() { p.mu.Lock(); p.active--; p.mu.Unlock() }
	if busy {
		limited(w, nil, "Too many password attempts. Try again in a minute.")
		return nil, false
	}
	if ok, err := p.take(r.Context(), passwordKeys(r, name), []int{120, 30, 10}); !ok {
		done()
		limited(w, err, "Too many password attempts. Try again in a minute.")
		return nil, false
	}
	return done, true
}
func passwordKeys(r *http.Request, name string) []string {
	return []string{"all", "peer:" + socketPeer(r), "user:" + strings.ToLower(strings.TrimSpace(name))}
}

// refundPasswordRequest returns the charge of an attempt that proved its secret.
// The charge is taken before checking, so concurrent guesses stay bounded.
func (s *Server) refundPasswordRequest(r *http.Request, name string) {
	// A lost refund only leaves the attempt charged until its window ends.
	_ = s.passwords.refund(r.Context(), passwordKeys(r, name))
}
func (s *Server) passwordLogin(w http.ResponseWriter, r *http.Request) {
	if !s.localPasswords() {
		http.Error(w, "Local password sign-in is disabled. Use your configured identity provider or trusted proxy.", 403)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid login form", 400)
		return
	}
	done, ok := s.passwordRequest(w, r, r.Form.Get("username"))
	if !ok {
		return
	}
	defer done()
	id, epoch, err := s.catalog.AuthenticateLocal(r.Form.Get("username"), r.Form.Get("password"))
	if err != nil {
		http.Error(w, "Invalid username or password", 401)
		return
	}
	if err = s.createSessionAtEpoch(w, id, epoch); err != nil {
		fail(w, err)
		return
	}
	http.Redirect(w, r, safeLoginNext(r.Form.Get("next")), 303)
}
func (s *Server) createLocalUser(w http.ResponseWriter, r *http.Request) {
	if !s.localPasswords() || !s.catalog.IsAdmin(subject(r.Context())) {
		http.Error(w, "Local account creation is unavailable", 403)
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !decode(w, r, &in) {
		return
	}
	done, ok := s.passwordRequest(w, r, in.Username)
	if !ok {
		return
	}
	defer done()
	id, err := s.catalog.CreateLocalUser(subject(r.Context()), in.Username, in.Password)
	respond(w, map[string]string{"id": id}, err)
}
func (s *Server) changeLocalPassword(w http.ResponseWriter, r *http.Request) {
	if !s.localPasswords() {
		http.Error(w, "Local passwords are disabled; manage credentials with your identity provider.", 403)
		return
	}
	var in struct {
		User     string `json:"user"`
		Current  string `json:"current"`
		Password string `json:"password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !decode(w, r, &in) {
		return
	}
	actor := subject(r.Context())
	if in.User == "" {
		in.User = actor
	}
	if actor != in.User && !s.catalog.IsAdmin(actor) || in.User == catalog.Bootstrap {
		http.Error(w, "Password change forbidden", 403)
		return
	}
	done, ok := s.passwordRequest(w, r, in.User)
	if !ok {
		return
	}
	defer done()
	respond(w, map[string]bool{"ok": true}, s.catalog.SetLocalPassword(actor, in.User, in.Current, in.Password))
}

// Setup is an explicit recovery-authenticated action, never public registration.
func (s *Server) setupAvailable(r *http.Request) bool {
	return s.localPasswords() && subject(r.Context()) == catalog.Bootstrap && s.catalog.InitialSetupAvailable()
}
func (s *Server) setupAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.setupAvailable(r) {
		http.Error(w, "Initial setup is unavailable. Manage existing accounts through Administration.", http.StatusForbidden)
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !decode(w, r, &in) {
		return
	}
	done, ok := s.passwordRequest(w, r, in.Username)
	if !ok {
		return
	}
	defer done()
	id, err := s.catalog.CreateInitialAdmin(subject(r.Context()), in.Username, in.Password)
	if err != nil {
		fail(w, err)
		return
	}
	if err = s.createSession(w, id); err != nil {
		http.Error(w, "Your administrator account was created, but automatic sign-in failed. Sign in with your new username and password.", 500)
		return
	}
	jsonResponse(w, map[string]string{"id": id})
}
