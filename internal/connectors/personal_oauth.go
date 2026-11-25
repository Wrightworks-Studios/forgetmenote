package connectors

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/capacity"
	"forgetmenote/internal/integrations"
	"forgetmenote/internal/pg"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

var personalGoogleScopes = []string{"openid", "email", "profile", "https://www.googleapis.com/auth/gmail.modify", "https://www.googleapis.com/auth/calendar.events", "https://www.googleapis.com/auth/calendar.calendarlist.readonly", "https://www.googleapis.com/auth/drive.readonly", "https://www.googleapis.com/auth/documents", "https://www.googleapis.com/auth/spreadsheets"}
var personalSlackScopes = []string{"channels:read", "channels:history", "groups:read", "groups:history", "im:read", "im:history", "mpim:read", "mpim:history", "users:read", "chat:write", "search:read.public", "search:read.private", "search:read.im", "search:read.mpim"}

// ErrProviderBusy is a provider asking for fewer requests; the caller may try
// again later.
var ErrProviderBusy = errors.New("personal provider is limiting requests; try again later")

// errNotSent is a request that never reached the provider.
var errNotSent = errors.New("personal provider unreachable; try again later")

// providerRefusal is a provider's answer refusing a request: an HTTP error
// status or an error field, with the OAuth or Slack error code it named.
type providerRefusal struct {
	status int
	code   string
}

func (e *providerRefusal) Error() string {
	if e.status != 0 {
		return fmt.Sprintf("personal provider HTTP %d; check authorization and provider limits", e.status)
	}
	return "personal provider rejected the request"
}

// refreshRevoked reports whether a failed refresh needs the person to authorize
// again. A definite revocation does. Slack rotates refresh tokens, so any other
// Slack outcome it may have acted on does too: it can have spent the old token.
// A rate limit, a request that never reached the provider, and Google's other
// failures keep the grant, and a later request refreshes it.
func refreshRevoked(provider string, err error) bool {
	var refused *providerRefusal
	if errors.As(err, &refused) && (refused.code == "invalid_grant" || refused.code == "invalid_client") {
		return true
	}
	if errors.Is(err, ErrProviderBusy) || errors.Is(err, errNotSent) {
		return false
	}
	return provider == "slack"
}

func (p *PersonalConnections) application(provider string) (*OAuthApplication, string, string, error) {
	switch provider {
	case "google":
		if p.config.Google != nil {
			return p.config.Google, p.googleAuth, p.googleToken, nil
		}
	case "slack":
		if p.config.Slack != nil {
			return p.config.Slack, p.slackAuth, p.slackAPI + "/oauth.v2.access", nil
		}
	}
	return nil, "", "", errors.New("personal provider is not configured")
}

func (p *PersonalConnections) Begin(owner, provider, binding, redirect string) (string, error) {
	app, auth, _, err := p.application(provider)
	if err != nil {
		return "", err
	}
	if !validPersonalOwner(owner) || binding == "" {
		return "", ErrInvalid
	}
	ctx, cancel := personalContext()
	defer cancel()
	current, _, err := p.load(ctx, p.db.Pool, owner, provider)
	if err != nil {
		return "", err
	}
	state, verifier := personalRandom(), personalRandom()
	// The callback may reach any gateway. A new flow replaces the owner's earlier
	// one for this provider.
	sealed, err := p.sealPending(hash(state), personalPending{Owner: owner, Provider: provider, Binding: hash(binding), Verifier: verifier, Redirect: redirect, Generation: current.Generation, Expires: time.Now().Add(10 * time.Minute)})
	if err != nil {
		return "", err
	}
	switch err = p.db.BeginFlow(ctx, personalFlow(provider), hash(state), owner, sealed, 10*time.Minute, pg.FlowBound{Limit: 256, Replace: true}); {
	case errors.Is(err, pg.ErrFlowsFull):
		return "", errors.New("too many pending personal connections; try again later")
	case err != nil:
		return "", fmt.Errorf("%w: %w", ErrPersonalUnavailable, err)
	}
	challenge := sha256.Sum256([]byte(verifier))
	query := url.Values{"client_id": {app.ClientID}, "redirect_uri": {redirect}, "state": {state}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}}
	if provider == "google" {
		query.Set("scope", strings.Join(personalGoogleScopes, " "))
		query.Set("access_type", "offline")
		query.Set("prompt", "consent select_account")
		query.Set("include_granted_scopes", "true")
	} else {
		query.Set("user_scope", strings.Join(personalSlackScopes, ","))
		if app.Workspace != "" {
			query.Set("team", app.Workspace)
		}
	}
	return auth + "?" + query.Encode(), nil
}

func (p *PersonalConnections) Finish(ctx context.Context, owner, provider, binding, state, code string, guard func() error) (PersonalConnection, error) {
	pending, valid, err := p.claimPending(ctx, provider, hash(state), func(pending personalPending) bool {
		return pending.Owner == owner && pending.Provider == provider && pending.Expires.After(time.Now()) && subtle.ConstantTimeCompare([]byte(pending.Binding), []byte(hash(binding))) == 1
	})
	if err != nil {
		return PersonalConnection{}, err
	}
	if !valid || code == "" || len(code) > 8192 {
		return PersonalConnection{}, errors.New("invalid or expired connection; start again")
	}
	app, _, tokenURL, err := p.application(provider)
	if err != nil {
		return PersonalConnection{}, err
	}
	secret, err := os.ReadFile(app.SecretFile)
	if err != nil {
		return PersonalConnection{}, errors.New("OAuth client secret unavailable")
	}
	if err = guard(); err != nil {
		return PersonalConnection{}, err
	}
	values := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {pending.Verifier}, "client_id": {app.ClientID}, "client_secret": {strings.TrimSpace(string(secret))}, "redirect_uri": {pending.Redirect}}
	result, err := p.oauthRequest(ctx, tokenURL, values)
	if err != nil {
		return PersonalConnection{}, err
	}
	cred, err := p.parseToken(owner, provider, result)
	if err != nil {
		return PersonalConnection{}, err
	}
	if provider == "google" {
		profile, err := p.requestJSON(ctx, "GET", p.googleUser, cred.AccessToken, nil)
		if err != nil {
			return PersonalConnection{}, err
		}
		if profile["email_verified"] != true || str(profile["sub"]) == "" {
			return PersonalConnection{}, errors.New("Google account identity was not verified")
		}
		cred.Subject, cred.Email = str(profile["sub"]), strings.ToLower(str(profile["email"]))
		if _, err = emailAddress(cred.Email); err != nil {
			return PersonalConnection{}, errors.New("Google returned an invalid email identity")
		}
		if cred.RefreshToken == "" {
			return PersonalConnection{}, errors.New("Google did not grant offline access; revoke the app grant and reconnect")
		}
	} else {
		identity, err := p.requestJSON(ctx, "GET", p.slackAPI+"/auth.test", cred.AccessToken, nil)
		if err != nil {
			return PersonalConnection{}, err
		}
		if identity["ok"] != true || str(identity["user_id"]) != cred.Subject || str(identity["team_id"]) != cred.Workspace || app.Workspace != "" && cred.Workspace != app.Workspace {
			return PersonalConnection{}, errors.New("Slack user or workspace does not match the authorized connection")
		}
	}
	if err = guard(); err != nil {
		return PersonalConnection{}, err
	}
	if cred.Generation, err = p.link(cred, pending.Generation); err != nil {
		return PersonalConnection{}, err
	}
	return cred.PersonalConnection, nil
}

func personalFlow(provider string) string { return "personal-" + provider + "/pending" }

const personalPendingSeal = "ForgetMeNote personal pending v1"

// sealPending encrypts a flow, including its PKCE verifier, bound to its
// state digest.
func (p *PersonalConnections) sealPending(key string, pending personalPending) ([]byte, error) {
	plain, err := json.Marshal(pending)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, p.key.NonceSize())
	rand.Read(nonce)
	return p.key.Seal(nonce, nonce, plain, []byte(personalPendingSeal+"\x00"+key)), nil
}

// claimPending completes a live flow once when valid accepts it; a flow valid
// refuses stays pending for its own browser.
func (p *PersonalConnections) claimPending(ctx context.Context, provider, key string, valid func(personalPending) bool) (personalPending, bool, error) {
	var pending personalPending
	raw, err := p.db.ReadFlow(ctx, personalFlow(provider), key)
	if errors.Is(err, pg.ErrNoFlow) {
		return pending, false, nil
	}
	if err != nil {
		return pending, false, fmt.Errorf("%w: %w", ErrPersonalUnavailable, err)
	}
	if len(raw) < p.key.NonceSize() {
		return pending, false, nil
	}
	plain, err := p.key.Open(nil, raw[:p.key.NonceSize()], raw[p.key.NonceSize():], []byte(personalPendingSeal+"\x00"+key))
	if err != nil || json.Unmarshal(plain, &pending) != nil || !valid(pending) {
		return pending, false, nil
	}
	if _, err = p.db.TakeFlow(ctx, personalFlow(provider), key); errors.Is(err, pg.ErrNoFlow) {
		return pending, false, nil
	} else if err != nil {
		return pending, false, fmt.Errorf("%w: %w", ErrPersonalUnavailable, err)
	}
	return pending, true, nil
}

// link stores an authorized grant as the next generation of the one the flow
// began from. Linking a provider identity is serialized across gateways, so
// two employees cannot both claim one account.
func (p *PersonalConnections) link(cred personalCredential, began uint64) (uint64, error) {
	ctx, cancel := personalContext()
	defer cancel()
	tx, err := p.db.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrPersonalUnavailable, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('personal-link ' || $1 || ' ' || $2, 0))`, p.db.Org, cred.Provider); err != nil {
		return 0, fmt.Errorf("%w: %w", ErrPersonalUnavailable, err)
	}
	linked, err := p.rows(ctx, tx, "", cred.Provider)
	if err != nil {
		return 0, err
	}
	var old personalCredential
	var version int64
	for _, other := range linked {
		if other.Owner == cred.Owner {
			old, version = other.personalCredential, other.version
		} else if other.Subject == cred.Subject && other.Workspace == cred.Workspace && other.State != "disconnected" {
			return 0, errors.New("this provider account is already linked to another employee")
		}
	}
	if old.Generation != began {
		return 0, errors.New("connection changed while authorization was in progress")
	}
	cred.Generation = old.Generation + 1
	if err = p.save(ctx, tx, cred, version); err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("%w: %w", ErrPersonalUnavailable, err)
	}
	return cred.Generation, nil
}

func (p *PersonalConnections) parseToken(owner, provider string, value map[string]any) (personalCredential, error) {
	root := value
	if provider == "slack" {
		if value["ok"] != true {
			return personalCredential{}, errors.New("Slack authorization rejected")
		}
		if user, ok := value["authed_user"].(map[string]any); ok {
			value = user
		}
		if str(value["token_type"]) != "user" {
			return personalCredential{}, errors.New("a Slack user grant is required")
		}
	}
	c := personalCredential{Owner: owner, PersonalConnection: PersonalConnection{Provider: provider, State: "connected"}, AccessToken: str(value["access_token"]), RefreshToken: str(value["refresh_token"])}
	if c.AccessToken == "" || strings.ContainsAny(c.AccessToken, "\r\n") {
		return c, errors.New("provider returned an invalid access token")
	}
	c.Scopes = strings.Fields(strings.ReplaceAll(str(value["scope"]), ",", " "))
	if seconds, ok := value["expires_in"].(float64); ok && seconds > 0 && seconds < 365*86400 {
		c.Expires = time.Now().Add(time.Duration(seconds) * time.Second)
	}
	if provider == "google" && c.Expires.IsZero() {
		return c, errors.New("Google omitted token expiration")
	}
	if provider == "slack" {
		c.Subject = str(value["id"])
		if team, ok := root["team"].(map[string]any); ok {
			c.Workspace = str(team["id"])
		}
	}
	return c, nil
}

func (p *PersonalConnections) oauthRequest(ctx context.Context, target string, form url.Values) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", target, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, ErrInvalid
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return p.response(req)
}
func (p *PersonalConnections) requestJSON(ctx context.Context, method, target, token string, body io.Reader) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return p.response(req)
}
func (p *PersonalConnections) response(req *http.Request) (map[string]any, error) {
	resp, err := p.client.Do(req)
	if err != nil {
		// A failed dial sent nothing; a later failure may have been acted on.
		var dial *net.OpError
		if errors.As(err, &dial) && dial.Op == "dial" {
			return nil, errNotSent
		}
		return nil, errors.New("personal provider request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, ErrProviderBusy
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var refused struct {
			Error string `json:"error"`
		}
		if raw, err := readBounded(resp.Body, 64<<10); err == nil {
			json.Unmarshal(raw, &refused)
		}
		return nil, &providerRefusal{status: resp.StatusCode, code: refused.Error}
	}
	raw, err := readBounded(resp.Body, capacity.StateBytes.Limit())
	if errors.Is(err, errTooLarge) {
		return nil, capacity.StateBytes.Error()
	}
	if err != nil {
		return nil, errors.New("incomplete personal provider response")
	}
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return nil, errors.New("invalid personal provider response")
	}
	// Slack reports a rate limit as HTTP 429 or as a "ratelimited" error.
	if value["error"] == "ratelimited" {
		return nil, ErrProviderBusy
	}
	if refused, exists := value["error"]; exists {
		code, _ := refused.(string)
		return nil, &providerRefusal{code: code}
	}
	return value, nil
}

// Token refresh is serialized per connection, without blocking unrelated users:
// within a gateway by a mutex, so one request at a time waits on the database,
// and across gateways by a lock there, so a request that finds the token
// expiring waits for a refresh elsewhere and then uses its token. A persisted
// in-flight marker prevents reuse of a rotated token after a crash.
func (p *PersonalConnections) token(ctx context.Context, owner, provider string, generation uint64) (personalCredential, error) {
	// A Slack grant that a build with Slack stored stays, unused, in a build
	// without it.
	if provider == "slack" {
		if err := integrations.Check("slack"); err != nil {
			return personalCredential{}, err
		}
	}
	id := personalKey(owner, provider)
	p.mu.Lock()
	lock := p.refreshLocks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		p.refreshLocks[id] = lock
	}
	p.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	connected := func(storage context.Context) (personalCredential, int64, error) {
		c, version, err := p.load(storage, p.db.Pool, owner, provider)
		if err == nil && (c.State != "connected" || c.Generation != generation) {
			err = errors.New("personal connection needs authorization")
		}
		return c, version, err
	}
	storage, cancel := personalContext()
	c, _, err := connected(storage)
	cancel()
	if err != nil || !c.Refreshing && (c.Expires.IsZero() || time.Until(c.Expires) > time.Minute) {
		return c, err
	}
	held, err := p.db.Exclusive(ctx, "personal-refresh "+id)
	if err != nil {
		return c, errors.Join(ErrPersonalChanged, err)
	}
	defer held()
	storage, cancel = personalContext()
	defer cancel()
	c, version, err := connected(storage)
	if err != nil {
		return c, err
	}
	// Under the lock, a marker is a refresh whose gateway stopped or lost its
	// connection to the database.
	if c.Refreshing {
		if settled, err := p.interrupted(storage, c, version, false); err != nil || settled {
			return c, errors.Join(errors.New("personal connection needs authorization"), err)
		}
		return c, fmt.Errorf("%w: another gateway is refreshing it", ErrPersonalChanged)
	}
	if c.Expires.IsZero() || time.Until(c.Expires) > time.Minute {
		return c, nil
	}
	if c.RefreshToken == "" {
		return c, errors.New("personal connection expired; reconnect")
	}
	// The marker is claimed over the version read, so one gateway refreshes.
	c.Refreshing, c.RefreshHolder = true, p.db.Holder
	if err = p.save(storage, p.db.Pool, c, version); err != nil {
		return c, err
	}
	app, _, target, err := p.application(provider)
	if err != nil {
		return c, err
	}
	secret, err := os.ReadFile(app.SecretFile)
	if err != nil {
		return c, errors.New("OAuth client secret unavailable")
	}
	result, err := p.oauthRequest(ctx, target, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.RefreshToken}, "client_id": {app.ClientID}, "client_secret": {strings.TrimSpace(string(secret))}})
	var next personalCredential
	if err == nil {
		next, err = p.parseToken(owner, provider, result)
	}
	storage, cancel = personalContext()
	defer cancel()
	current, version, loadErr := p.load(storage, p.db.Pool, owner, provider)
	if loadErr != nil {
		return c, loadErr
	}
	if current.Generation != generation || current.State != "connected" || !current.Refreshing || current.RefreshHolder != p.db.Holder {
		return c, errors.New("personal connection was revoked during refresh")
	}
	c.Refreshing, c.RefreshHolder = false, ""
	if err != nil {
		if refreshRevoked(provider, err) {
			c.State = "reauthorize"
		}
		if saveErr := p.save(storage, p.db.Pool, c, version); saveErr != nil {
			return c, saveErr
		}
		return c, err
	}
	next.PersonalConnection = c.PersonalConnection
	if reported := strings.Fields(strings.ReplaceAll(str(result["scope"]), ",", " ")); len(reported) > 0 {
		next.Scopes = reported
	}
	if next.RefreshToken == "" {
		next.RefreshToken = c.RefreshToken
	}
	if err = p.save(storage, p.db.Pool, next, version); err != nil {
		return c, err
	}
	return next, nil
}

func (p *PersonalConnections) scope(owner, provider string, generation uint64, want ...string) error {
	ctx, cancel := personalContext()
	defer cancel()
	c, _, err := p.load(ctx, p.db.Pool, owner, provider)
	if err != nil {
		return err
	}
	if c.State != "connected" || c.Generation != generation {
		return errors.New("personal connection changed")
	}
	for _, scope := range want {
		if slices.Contains(c.Scopes, scope) {
			return nil
		}
	}
	return errors.New("required provider permission was not granted; reconnect to enable this feature")
}
