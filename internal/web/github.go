package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg"
)

type githubPending struct {
	Subject, Binding, Verifier string
	Generation                 uint64
	Expires                    time.Time
}
type githubConnection struct {
	clientID, secret                string
	authorizeURL, tokenURL, userURL string
	client                          *http.Client
}

// ConfigureGitHub uses a dedicated OAuth app with no scopes. Tokens never enter
// the catalog, note repositories, remote credentials, or browser responses.
func (s *Server) ConfigureGitHub(clientID, secret string) error {
	unlock, err := s.startupConfiguration()
	if err != nil {
		return err
	}
	defer unlock()

	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(secret) == "" {
		return errors.New("GitHub client ID and secret are both required")
	}
	s.github = &githubConnection{clientID: clientID, secret: secret, authorizeURL: "https://github.com/login/oauth/authorize", tokenURL: "https://github.com/login/oauth/access_token", userURL: "https://api.github.com/user", client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	return nil
}
func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	a, err := s.catalog.Account(subject(r.Context()))
	if err != nil {
		fail(w, err)
		return
	}
	name, err := s.catalog.SubjectName(subject(r.Context()))
	if err != nil {
		fail(w, err)
		return
	}
	jsonResponse(w, map[string]any{"displayName": name, "local": a.LocalIdentity(), "author": a.Identity(), "github": a.GitHub, "githubAvailable": s.github != nil, "user": subject(r.Context()), "passwordAvailable": s.localPasswords(), "username": s.catalog.LocalUsername(subject(r.Context())), "oidcAvailable": s.oidc != nil, "oidc": s.catalog.ExternalIdentities(subject(r.Context()))})
}
func (s *Server) accountWrite(w http.ResponseWriter, r *http.Request) bool {
	// Linking is a same-origin browser action, even when an admin bearer is used.
	if r.Header.Get("Origin") != s.origin {
		http.Error(w, "Same-origin account settings required", 403)
		return false
	}
	return true
}
func (s *Server) connectGitHub(w http.ResponseWriter, r *http.Request) {
	if !s.accountWrite(w, r) {
		return
	}
	g := s.github
	if g == nil {
		http.Error(w, "GitHub linking is not configured on this server", 503)
		return
	}
	var in struct{}
	if !decode(w, r, &in) {
		return
	}
	a, err := s.catalog.Account(subject(r.Context()))
	if err != nil {
		fail(w, err)
		return
	}
	state, binding, verifier := RandomToken(), RandomToken(), RandomToken()
	pending := githubPending{subject(r.Context()), digest(binding), verifier, a.Generation, time.Now().Add(10 * time.Minute)}
	if err = s.beginFlow(r.Context(), "github-link/pending", digest(state), pending.Subject, pending, 10*time.Minute, pg.FlowBound{Limit: flowLimit, Replace: true}); err != nil {
		fail(w, err)
		return
	}
	s.setBinding(w, githubLinkBinding, binding)
	challenge := sha256.Sum256([]byte(verifier))
	params := url.Values{"client_id": {g.clientID}, "redirect_uri": {s.origin + "/account/github/callback"}, "state": {state}, "scope": {""}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}, "allow_signup": {"false"}, "prompt": {"select_account"}}
	jsonResponse(w, map[string]string{"url": g.authorizeURL + "?" + params.Encode()})
}
func (s *Server) githubCallback(w http.ResponseWriter, r *http.Request) {
	g := s.github
	if g == nil {
		http.Error(w, "GitHub linking is not configured", 503)
		return
	}
	binding, err := s.binding(r, githubLinkBinding)
	if err != nil {
		http.Error(w, "Connection session missing; start again in Your settings", 403)
		return
	}
	p, valid, err := claimFlow(s, r.Context(), "github-link/pending", digest(r.URL.Query().Get("state")), func(p githubPending) bool {
		return p.Subject == subject(r.Context()) && p.Expires.After(time.Now()) && subtle.ConstantTimeCompare([]byte(p.Binding), []byte(digest(binding))) == 1
	})
	if err != nil {
		fail(w, err)
		return
	}
	if !valid {
		http.Error(w, "Invalid or expired GitHub connection; start again in Your settings", 403)
		return
	}
	s.clearBinding(w, githubLinkBinding)
	code := r.URL.Query().Get("code")
	if r.URL.Query().Get("error") != "" || code == "" || len(code) > 1024 {
		http.Error(w, "GitHub authorization was cancelled or invalid", 400)
		return
	}
	form := url.Values{"client_id": {g.clientID}, "client_secret": {g.secret}, "code": {code}, "code_verifier": {p.Verifier}, "redirect_uri": {s.origin + "/account/github/callback"}}
	req, err := http.NewRequestWithContext(r.Context(), "POST", g.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		http.Error(w, "GitHub connection unavailable", 502)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var token struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
		TokenType   string `json:"token_type"`
	}
	if err = g.request(req, &token); err != nil || token.AccessToken == "" || !strings.EqualFold(token.TokenType, "bearer") {
		http.Error(w, "GitHub authorization failed; start again", 502)
		return
	}
	// GitHub may return scopes from an older grant to a reused OAuth app.
	// Refuse such a grant; self-hosters should use a dedicated identity-only app.
	if strings.TrimSpace(token.Scope) != "" {
		http.Error(w, "GitHub returned extra permissions. Revoke this app's old authorization and reconnect using an identity-only OAuth app.", 400)
		return
	}
	req, err = http.NewRequestWithContext(r.Context(), "GET", g.userURL, nil)
	if err != nil {
		http.Error(w, "GitHub connection unavailable", 502)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	var profile struct {
		ID      int64     `json:"id"`
		Login   string    `json:"login"`
		Created time.Time `json:"created_at"`
		Type    string    `json:"type"`
	}
	if err = g.request(req, &profile); err != nil || profile.Type != "User" {
		http.Error(w, "Could not verify the GitHub user; start again", 502)
		return
	}
	if err = s.catalog.LinkGitHub(p.Subject, catalog.GitHubAccount{ID: profile.ID, Login: profile.Login, Created: profile.Created}, p.Generation); err != nil {
		fail(w, err)
		return
	}
	http.Redirect(w, r, "/?account=github", http.StatusSeeOther)
}
func (g *githubConnection) request(req *http.Request, dest any) error {
	resp, err := g.client.Do(req)
	if err != nil {
		return errors.New("GitHub request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New("GitHub request rejected")
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(dest)
}
func (s *Server) githubAttribution(w http.ResponseWriter, r *http.Request) {
	if !s.accountWrite(w, r) {
		return
	}
	var in struct {
		Email   string `json:"email"`
		Enabled bool   `json:"enabled"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.catalog.GitHubAttribution(subject(r.Context()), in.Email, in.Enabled); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	s.account(w, r)
}
func (s *Server) disconnectGitHub(w http.ResponseWriter, r *http.Request) {
	if !s.accountWrite(w, r) {
		return
	}
	var in struct{}
	if !decode(w, r, &in) {
		return
	}
	if err := s.catalog.DisconnectGitHub(subject(r.Context())); err != nil {
		fail(w, err)
		return
	}
	s.account(w, r)
}
