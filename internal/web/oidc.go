package web

import (
	"context"
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type oidcPending struct {
	Binding, Nonce, Verifier, Link, Next string
	Epoch                                uint64
	Expires                              time.Time
}
type oidcLogin struct {
	mu                 sync.Mutex
	issuer, id, secret string
	client             *http.Client
	provider           *oidc.Provider
	// discovery closes when the attempt in flight ends; failure is the last
	// attempt's error, which callers receive until retry.
	discovery     chan struct{}
	failure       error
	retry         time.Time
	starts        passwordRequests
	defaultGroups []string
	// requestEmail asks for the email scope, which some providers need before
	// they put the verified address in the ID token.
	requestEmail bool
}

func (s *Server) ConfigureOIDC(issuer, id, secret string, groups ...string) error {
	unlock, err := s.startupConfiguration()
	if err != nil {
		return err
	}
	defer unlock()

	u, err := url.Parse(issuer)
	if err != nil || !validRedirect(issuer) || u.RawQuery != "" || id == "" || secret == "" {
		return errors.New("OIDC requires an HTTPS issuer, client ID and secret (HTTP allowed only on loopback)")
	}
	s.oidc = &oidcLogin{issuer: issuer, id: id, secret: secret, client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, starts: passwordRequests{db: s.db, group: "oidc-starts"}}
	for _, group := range groups {
		if group = strings.TrimSpace(group); group != "" {
			s.oidc.defaultGroups = append(s.oidc.defaultGroups, group)
		}
	}
	return nil
}

// oidcRetry is how long a failed discovery is reported before another attempt.
const oidcRetry = 30 * time.Second

func (o *oidcLogin) config(ctx context.Context, origin string) (*oidc.Provider, oauth2.Config, error) {
	provider, err := o.discover(ctx)
	if err != nil {
		return nil, oauth2.Config{}, err
	}
	scopes := []string{oidc.ScopeOpenID}
	if o.requestEmail {
		scopes = append(scopes, "email")
	}
	return provider, oauth2.Config{ClientID: o.id, ClientSecret: o.secret, Endpoint: provider.Endpoint(), RedirectURL: origin + "/auth/oidc/callback", Scopes: scopes}, nil
}

// Discovery is lazy: a provider outage must not prevent recovery or existing
// sessions. Page loads share one attempt, which runs outside the lock and
// outlives a caller that stops waiting, and a failure is reported without
// contacting the provider again for oidcRetry.
func (o *oidcLogin) discover(ctx context.Context) (*oidc.Provider, error) {
	for {
		o.mu.Lock()
		if o.provider != nil {
			defer o.mu.Unlock()
			return o.provider, nil
		}
		if o.failure != nil && time.Now().Before(o.retry) {
			defer o.mu.Unlock()
			return nil, o.failure
		}
		if o.discovery == nil {
			o.discovery = make(chan struct{})
			go o.discoverProvider(o.discovery)
		}
		done := o.discovery
		o.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (o *oidcLogin) discoverProvider(done chan struct{}) {
	p, err := o.fetchProvider(oidc.ClientContext(context.Background(), o.client))
	o.mu.Lock()
	defer o.mu.Unlock()
	o.discovery = nil
	if err != nil {
		o.failure, o.retry = err, time.Now().Add(oidcRetry)
	} else {
		o.provider, o.failure = p, nil
	}
	close(done)
}

// fetchProvider reads the issuer's metadata; the client bounds its duration.
func (o *oidcLogin) fetchProvider(ctx context.Context) (*oidc.Provider, error) {
	p, err := oidc.NewProvider(ctx, o.issuer)
	if err != nil {
		return nil, err
	}
	var metadata struct {
		JWKS string `json:"jwks_uri"`
	}
	if err = p.Claims(&metadata); err != nil || !validRedirect(metadata.JWKS) {
		return nil, errors.New("unsafe OIDC signing-key endpoint")
	}
	e := p.Endpoint()
	if !validRedirect(e.AuthURL) || !validRedirect(e.TokenURL) {
		return nil, errors.New("unsafe OIDC endpoints")
	}
	return p, nil
}
func safeLoginNext(next string) string {
	if strings.HasPrefix(next, "/oauth/authorize?") || strings.HasPrefix(next, "/connections/start?") {
		return next
	}
	return "/"
}
func (s *Server) startOIDC(w http.ResponseWriter, r *http.Request) {
	o := s.oidc
	if o == nil {
		http.Error(w, "OIDC is not configured", 503)
		return
	}
	if !s.accountWrite(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", 400)
		return
	}
	link := ""
	if r.URL.Path == "/account/oidc/link" {
		link = subject(r.Context())
		if link == "" || link == catalog.Bootstrap {
			http.Error(w, "Sign in to your existing personal account to link it. Recovery administrator cannot be linked.", 403)
			return
		}
	}
	// Bound sign-in starts so a flood cannot cycle other people's pending logins out.
	if ok, err := o.starts.take(r.Context(), []string{"all", "peer:" + socketPeer(r)}, []int{120, 30}); !ok {
		limited(w, err, "Too many sign-in attempts. Try again in a minute.")
		return
	}
	_, config, err := o.config(r.Context(), s.origin)
	if err != nil {
		log.Printf("OIDC discovery failed: %v", err)
		http.Error(w, "Identity provider unavailable; try again or use recovery access", 502)
		return
	}
	state, binding, nonce, verifier := RandomToken(), RandomToken(), RandomToken(), oauth2.GenerateVerifier()
	p := oidcPending{Binding: digest(binding), Nonce: nonce, Verifier: verifier, Link: link, Next: safeLoginNext(r.Form.Get("next")), Expires: time.Now().Add(10 * time.Minute)}
	if link != "" {
		p.Epoch = s.catalog.Epoch(link)
	}
	// A full table drops the soonest-expiring start rather than refusing new ones.
	if err = s.beginFlow(r.Context(), "oidc/login", digest(state), link, p, 10*time.Minute, pg.FlowBound{Limit: flowLimit, Evict: true}); err != nil {
		fail(w, err)
		return
	}
	// A trusted-proxy account otherwise has no ForgetMeNote session cookie. Carry
	// that verified identity across the public provider callback during linking.
	if link != "" && s.proxyIdentity(r) != "" {
		if err = s.createSession(w, link); err != nil {
			fail(w, err)
			return
		}
	}
	s.setBinding(w, oidcBinding, binding)
	options := []oauth2.AuthCodeOption{oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)}
	if link != "" {
		options = append(options, oauth2.SetAuthURLParam("prompt", "login"), oauth2.SetAuthURLParam("max_age", "0"))
	}
	target := config.AuthCodeURL(state, options...)
	s.allowCallback(w, target)
	http.Redirect(w, r, target, 303)
}
func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	o := s.oidc
	if o == nil {
		http.Error(w, "OIDC is not configured", 503)
		return
	}
	binding, err := s.binding(r, oidcBinding)
	if err != nil {
		http.Error(w, "Invalid or expired login; start again", 403)
		return
	}
	p, ok, err := claimFlow(s, r.Context(), "oidc/login", digest(r.URL.Query().Get("state")), func(p oidcPending) bool {
		return subtle.ConstantTimeCompare([]byte(p.Binding), []byte(digest(binding))) == 1
	})
	if err != nil {
		fail(w, err)
		return
	}
	if !ok || time.Now().After(p.Expires) {
		http.Error(w, "Invalid or expired login; start again", 403)
		return
	}
	s.clearBinding(w, oidcBinding)
	if p.Link != "" && (!s.authenticated(r) || subject(r.Context()) != p.Link || !s.catalog.ValidSession(p.Link, p.Epoch)) {
		http.Error(w, "Existing account session changed; start linking again", 403)
		return
	}
	if r.URL.Query().Get("error") != "" || r.URL.Query().Get("code") == "" {
		http.Error(w, "Login cancelled or denied", 401)
		return
	}
	provider, config, err := o.config(r.Context(), s.origin)
	if err != nil {
		http.Error(w, "Identity provider unavailable", 502)
		return
	}
	ctx := oidc.ClientContext(r.Context(), o.client)
	token, err := config.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(p.Verifier))
	if err != nil {
		http.Error(w, "OIDC code exchange failed; start again", 401)
		return
	}
	raw, _ := token.Extra("id_token").(string)
	idToken, err := provider.Verifier(&oidc.Config{ClientID: o.id}).Verify(ctx, raw)
	if err != nil || idToken.Subject == "" || subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(p.Nonce)) != 1 {
		http.Error(w, "OIDC identity verification failed", 401)
		return
	}
	// Only a provider-verified address may name the account; a missing flag is unverified.
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		// Google's Workspace domain; personal accounts carry none.
		HostedDomain string `json:"hd"`
	}
	if err := idToken.Claims(&claims); err != nil || !claims.EmailVerified {
		claims.Email, claims.HostedDomain = "", ""
	}
	id, err := s.catalog.ResolveOIDC(idToken.Issuer, idToken.Subject, p.Link, claims.Email, o.defaultGroups...)
	if err != nil {
		fail(w, err)
		return
	}
	if err = s.catalog.ObserveOIDCEmail(idToken.Issuer, idToken.Subject, claims.Email, claims.HostedDomain); err != nil {
		fail(w, err)
		return
	}
	if err = s.createSession(w, id); err != nil {
		fail(w, err)
		return
	}
	next := p.Next
	if p.Link != "" {
		next = "/?account=oidc"
	}
	http.Redirect(w, r, next, 303)
}

// WebKit checks the initiating document's form-action policy throughout redirects.
// Permit only the discovered, validated authorization origin on pages with OIDC forms.
func (s *Server) allowOIDCForm(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		return
	}
	_, config, err := s.oidc.config(r.Context(), s.origin)
	if err == nil {
		s.allowCallback(w, config.Endpoint.AuthURL)
	}
	// Recovery and existing sessions still work if discovery is unavailable.
}
