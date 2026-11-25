package web

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"forgetmenote/internal/auth"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type oauthClient = auth.Client
type grantRequest = auth.GrantRequest
type accessGrant = auth.Grant
type oauthState = auth.State
type scopeKey = auth.ScopeKey
type clientKey = auth.ClientKey

func digest(value string) string { return auth.Digest(value) }
func (s *Server) oauthResource(r *http.Request) string {
	if r.URL != nil && strings.HasPrefix(r.URL.Path, apiPrefix+"/") {
		return s.origin + apiPrefix
	}
	return s.origin + "/mcp"
}
func (s *Server) validAccessToken(r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return false
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	g, ok := s.oauth.AccessToken(s.catalog, token, s.oauthResource(r))
	if !ok {
		return false
	}
	setSubject(r, g.Subject)
	*r = *r.WithContext(context.WithValue(r.Context(), clientKey{}, g.Client))
	*r = *r.WithContext(context.WithValue(r.Context(), scopeKey{}, g.Scope))
	*r = *r.WithContext(context.WithValue(r.Context(), auth.FamilyKey{}, g.Family))
	*r = *r.WithContext(context.WithValue(r.Context(), leaseKey{}, revisionLease{g.Subject, "grant:" + g.Family}))
	return true
}

// Persisted work uses a consent family; older receipts can still name a live access grant.
func (s *Server) oauthAuthorization(key string) (accessGrant, bool) {
	if family, ok := strings.CutPrefix(key, "grant:"); ok {
		return s.oauth.FamilyGrant(family)
	}
	grant, ok := s.oauth.AccessGrant(key)
	return grant, ok && grant.Family != "" && grant.FamilyExpires > time.Now().Unix()
}
func (s *Server) canWrite(ctx context.Context) bool {
	return s.hasScope(ctx, "notes:write")
}
func (s *Server) hasScope(ctx context.Context, required string) bool {
	return auth.HasScope(ctx, required)
}
func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && u.Fragment == "" && u.User == nil && !strings.ContainsAny(u.Host, " \t\r\n;\"'") && (u.Scheme == "https" || u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"))
}
func (s *Server) oauthRoute(w http.ResponseWriter, r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/oauth/") && r.Method == "POST" && !s.admitOAuth(w, r) {
		return true
	}
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource/api/v1":
		jsonResponse(w, map[string]any{"resource": s.origin + apiPrefix, "authorization_servers": []string{s.origin}, "scopes_supported": []string{"notes:read", "notes:write", "notes:manage", "data:read", "data:write"}, "bearer_methods_supported": []string{"header"}})
	case "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp":
		jsonResponse(w, map[string]any{"resource": s.origin + "/mcp", "authorization_servers": []string{s.origin}, "scopes_supported": employeeScopes(), "bearer_methods_supported": []string{"header"}})
	case "/.well-known/oauth-authorization-server":
		// Codex 0.160 requests every advertised AS scope when none is specified.
		// RFC 8414 permits omitting supported scopes: management/data writes remain
		// explicit opt-ins, with unchanged validation, consent and refresh grants.
		jsonResponse(w, map[string]any{"issuer": s.origin, "authorization_endpoint": s.origin + "/oauth/authorize", "token_endpoint": s.origin + "/oauth/token", "registration_endpoint": s.origin + "/oauth/register", "revocation_endpoint": s.origin + "/oauth/revoke", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"}, "scopes_supported": employeeScopes()})
	case "/oauth/register":
		if r.Method != "POST" {
			http.Error(w, "POST required", 405)
		} else {
			s.registerClient(w, r)
		}
	case "/oauth/authorize":
		s.authorize(w, r)
	case "/oauth/token":
		if r.Method != "POST" {
			http.Error(w, "POST required", 405)
		} else {
			s.exchangeToken(w, r)
		}
	case "/oauth/revoke":
		if r.Method != "POST" {
			http.Error(w, "POST required", 405)
			break
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16384)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid form", 400)
			break
		}
		err := s.oauth.RevokeToken(r.Form.Get("token"))
		if err != nil {
			fail(w, err)
		} else {
			w.WriteHeader(200)
		}
	default:
		return false
	}
	return true
}
func (s *Server) registerClient(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name          string   `json:"client_name"`
		Redirects     []string `json:"redirect_uris"`
		Method        string   `json:"token_endpoint_auth_method"`
		GrantTypes    []string `json:"grant_types"`
		ResponseTypes []string `json:"response_types"`
		Scope         string   `json:"scope"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Redirects) == 0 || len(in.Redirects) > 5 || len(in.Name) > 100 || in.Method != "" && in.Method != "none" {
		http.Error(w, "Invalid client registration", 400)
		return
	}
	for _, uri := range in.Redirects {
		if !validRedirect(uri) {
			http.Error(w, "Invalid redirect URI", 400)
			return
		}
	}
	c, err := s.oauth.RegisterClient(in.Name, in.Redirects)
	if errors.Is(err, auth.ErrRegistrationRate) || errors.Is(err, auth.ErrRegistrationCapacity) {
		http.Error(w, err.Error(), 429)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	json.NewEncoder(w).Encode(map[string]any{"client_id": c.ID, "client_name": c.Name, "redirect_uris": c.Redirects, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}})
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	if !s.authenticated(r) {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), 303)
		return
	}
	if r.Method == "POST" {
		if r.Header.Get("Origin") != s.origin {
			http.Error(w, "Same-origin consent required", 403)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid consent", 400)
			return
		}
		payload := r.Form.Get("request")
		if subtle.ConstantTimeCompare([]byte(s.sign(payload)), []byte(r.Form.Get("signature"))) != 1 {
			http.Error(w, "Invalid consent signature", 403)
			return
		}
		raw, err := base64.RawURLEncoding.DecodeString(payload)
		var g grantRequest
		if err != nil || json.Unmarshal(raw, &g) != nil || g.Expires < time.Now().Unix() || g.Subject != subject(r.Context()) || !s.catalog.ValidSession(g.Subject, g.Epoch) {
			http.Error(w, "Consent expired; reconnect", 400)
			return
		}
		code, err := s.issueCode(g)
		if err != nil {
			fail(w, err)
			return
		}
		s.allowConsentRedirects(w)
		target, _ := url.Parse(g.Redirect)
		q := target.Query()
		q.Set("code", code)
		q.Set("state", g.State)
		q.Set("iss", s.origin)
		target.RawQuery = q.Encode()
		http.Redirect(w, r, target.String(), 303)
		return
	}
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", 405)
		return
	}
	q := r.URL.Query()
	client, ok := s.oauth.Client(q.Get("client_id"))
	redirect := q.Get("redirect_uri")
	matches := false
	for _, u := range client.Redirects {
		if u == redirect {
			matches = true
		}
	}
	challenge, err := base64.RawURLEncoding.DecodeString(q.Get("code_challenge"))
	if !ok || !matches || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || err != nil || len(challenge) != 32 {
		http.Error(w, "Invalid authorization request", 400)
		return
	}
	scope := strings.TrimSpace(q.Get("scope"))
	if scope == "" {
		scope = "notes:read notes:write"
	}
	for _, v := range strings.Fields(scope) {
		if v != "notes:read" && v != "notes:write" && v != "notes:manage" && v != "data:read" && v != "data:write" {
			http.Error(w, "Unknown scope", 400)
			return
		}
	}
	resource := q.Get("resource")
	if resource == "" {
		resource = s.origin + "/mcp"
	}
	if resource != s.origin+"/mcp" && resource != s.origin+apiPrefix {
		http.Error(w, "Invalid resource", 400)
		return
	}
	g := grantRequest{Client: client.ID, Redirect: redirect, Challenge: q.Get("code_challenge"), Scope: scope, Resource: resource, State: q.Get("state"), Subject: subject(r.Context()), Expires: time.Now().Add(5 * time.Minute).Unix(), Epoch: s.catalog.Epoch(subject(r.Context()))}
	raw, _ := json.Marshal(g)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	s.allowConsentRedirects(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.page.ExecuteTemplate(w, "consent.html", struct {
		Name, Scope, Redirect, Payload, Signature string
		Manage                                    bool
	}{client.Name, scope, redirect, payload, s.sign(payload), strings.Contains(" "+scope+" ", " notes:manage ")})
}
func (s *Server) issueCode(g grantRequest) (string, error) { return s.oauth.IssueCode(g) }
func (s *Server) exchangeToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid token request", 400)
		return
	}
	out, err := s.oauth.Exchange(s.catalog, r.Form)
	var replay *auth.RefreshReplayError
	if errors.As(err, &replay) {
		details := map[string]string{"family": digest(replay.Family)[:12], "chainAlreadyRevoked": strconv.FormatBool(replay.ChainRevoked)}
		if replay.UsedAt > 0 {
			details["usedAgeSeconds"] = strconv.FormatInt(max(time.Now().Unix()-replay.UsedAt, 0), 10)
		}
		if auditErr := s.catalog.RecordActivity(replay.Subject, "oauth.refresh_replay", replay.Client, "revoked", details); auditErr != nil {
			slog.Warn("OAuth replay audit could not be persisted", "error", auditErr)
		}
	}
	if errors.Is(err, auth.ErrInvalidGrant) || errors.Is(err, auth.ErrUnsupportedGrant) {
		code := "invalid_grant"
		if errors.Is(err, auth.ErrUnsupportedGrant) {
			code = "unsupported_grant_type"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": code})
		return
	}
	if errors.Is(err, auth.ErrTemporarilyUnavailable) {
		http.Error(w, auth.ErrTemporarilyUnavailable.Error(), 503)
		return
	}
	if errors.Is(err, auth.ErrAuthorizationCapacity) {
		http.Error(w, err.Error(), 503)
		return
	}
	respond(w, out, err)
}

// Chrome applies form-action throughout the redirect chain after a consent POST.
// A registered callback may redirect to another client origin (for example, a
// canonical hostname), which registration cannot predict. Omit only form-action
// on validated OAuth consent responses; the signed grant still binds the exact
// registered redirect URI and the POST still requires a same-origin request.
func (s *Server) allowConsentRedirects(w http.ResponseWriter) {
	policy := w.Header().Get("Content-Security-Policy")
	w.Header().Set("Content-Security-Policy", strings.Replace(policy, "; form-action 'self'", "", 1))
}

// Permit the discovered OIDC provider as a form redirect destination.
func (s *Server) allowCallback(w http.ResponseWriter, redirect string) {
	u, _ := url.Parse(redirect)
	policy := w.Header().Get("Content-Security-Policy")
	w.Header().Set("Content-Security-Policy", strings.Replace(policy, "form-action 'self'", "form-action 'self' "+u.Scheme+"://"+u.Host, 1))
}
