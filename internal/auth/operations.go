package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
	"maps"
	"net/url"
	"slices"
	"sort"
	"time"
)

const (
	accessLifetime    = time.Hour
	refreshLifetime   = 30 * 24 * time.Hour
	familyLifetime    = 90 * 24 * time.Hour
	refreshReuseGrace = 30 * time.Second
	// A registration that never completes a code exchange is abandoned.
	pendingClientLifetime = time.Hour
	// One account's authorizations cannot fill the instance-wide refresh limit.
	subjectRefreshLimit = 64
)

var ErrTemporarilyUnavailable = errors.New("temporarily_unavailable")

// UsedAt is when the replayed token was first exchanged (Unix seconds), or 0 when
// it was never exchanged; ChainRevoked reports a family revoked before this replay.
type RefreshReplayError struct {
	Subject, Client, Family string
	UsedAt                  int64
	ChainRevoked            bool
}

func (*RefreshReplayError) Error() string { return "invalid_grant" }
func (*RefreshReplayError) Unwrap() error { return ErrInvalidGrant }

var ErrInvalidGrant = errors.New("invalid_grant")
var ErrUnsupportedGrant = errors.New("unsupported_grant_type")
var ErrAuthorizationCapacity = errors.New("Authorization capacity reached; retry after expiry")
var ErrRegistrationRate = errors.New("Registration rate limit")
var ErrRegistrationCapacity = errors.New("Client registration limit reached")

type TokenResponse struct {
	Access    string `json:"access_token"`
	Refresh   string `json:"refresh_token"`
	Type      string `json:"token_type"`
	ExpiresIn int64  `json:"expires_in"`
	Scope     string `json:"scope"`
}

// The catalog owns the atomic distinction between revoked and unavailable authority.
type grantAuthority interface{ CheckSession(string, uint64) error }

func (o *State) Exchange(projects grantAuthority, form url.Values) (out TokenResponse, err error) {
	if err = o.write(); err != nil {
		return out, err
	}
	defer o.finish(&err)
	now := time.Now().Unix()
	var g Grant
	var consumed, code string
	replay, chainRevoked := false, false
	switch form.Get("grant_type") {
	case "authorization_code":
		code = Digest(form.Get("code"))
		request, ok := o.Codes[code]
		verifier := form.Get("code_verifier")
		sum := sha256.Sum256([]byte(verifier))
		actual := base64.RawURLEncoding.EncodeToString(sum[:])
		if !ok || request.Expires <= now || request.Client != form.Get("client_id") || request.Redirect != form.Get("redirect_uri") || len(verifier) < 43 || len(verifier) > 128 || subtle.ConstantTimeCompare([]byte(actual), []byte(request.Challenge)) != 1 {
			return out, ErrInvalidGrant
		}
		g = Grant{Client: request.Client, Scope: request.Scope, Resource: request.Resource, Subject: request.Subject, Epoch: request.Epoch, Family: Token(), FamilyExpires: now + int64(familyLifetime/time.Second)}
	case "refresh_token":
		key := Digest(form.Get("refresh_token"))
		if prior, used := o.Used[key]; used && prior.Client == form.Get("client_id") && (form.Get("resource") == "" || form.Get("resource") == prior.Resource) {
			if prior.Family == "" || prior.FamilyExpires <= now {
				return out, ErrInvalidGrant
			}
			g = prior
			chainRevoked = !o.familyActive(prior.Family, now)
			replay = !(prior.UsedAt > 0 && now >= prior.UsedAt && now-prior.UsedAt <= int64(refreshReuseGrace/time.Second) && !chainRevoked)
			if replay && prior.Expires <= now {
				return out, ErrInvalidGrant
			}
		} else {
			var ok bool
			g, ok = o.Refresh[key]
			if !ok || g.Expires <= now || g.Family == "" || g.FamilyExpires <= now || g.Client != form.Get("client_id") {
				return out, ErrInvalidGrant
			}
			consumed = key
		}
	default:
		return out, ErrUnsupportedGrant
	}
	if resource := form.Get("resource"); resource != "" && resource != g.Resource {
		return out, ErrInvalidGrant
	}
	if authorityErr := projects.CheckSession(g.Subject, g.Epoch); authorityErr != nil {
		if errors.Is(authorityErr, catalog.ErrStorageUnavailable) {
			return out, ErrTemporarilyUnavailable
		}
		return out, ErrInvalidGrant
	}
	// Validation above never mutates state; snapshot only an exchange that will.
	oldAccess, oldRefresh, oldUsed, oldCodes, oldCredentials, oldClients := maps.Clone(o.Access), maps.Clone(o.Refresh), maps.Clone(o.Used), maps.Clone(o.Codes), maps.Clone(o.Credentials), maps.Clone(o.Clients)
	committed := false
	defer func() {
		if err != nil && !committed {
			o.Access, o.Refresh, o.Used, o.Codes, o.Credentials, o.Clients = oldAccess, oldRefresh, oldUsed, oldCodes, oldCredentials, oldClients
		}
	}()
	if replay {
		o.RevokeFamily(g.Family)
		if err = o.Persist(); err != nil {
			return out, errors.Join(ErrTemporarilyUnavailable, err)
		}
		committed = true
		return out, &RefreshReplayError{Subject: g.Subject, Client: g.Client, Family: g.Family, UsedAt: g.UsedAt, ChainRevoked: chainRevoked}
	}
	for key, value := range o.Access {
		if value.Expires <= now {
			delete(o.Access, key)
		}
	}
	for key, value := range o.Refresh {
		if value.Expires <= now || value.FamilyExpires <= now {
			delete(o.Refresh, key)
		}
	}
	for key, value := range o.Used {
		if value.Expires <= now && (value.UsedAt == 0 || now-value.UsedAt > int64(refreshReuseGrace/time.Second)) {
			delete(o.Used, key)
		}
	}
	// A code exchange or retry-window reuse adds a live refresh token; rotation replaces one.
	if consumed == "" && !o.makeSubjectRoom(g.Subject, g.Family) {
		return out, ErrAuthorizationCapacity
	}
	refreshCount, usedCount := len(o.Refresh), len(o.Used)
	if consumed != "" {
		refreshCount--
		usedCount++
	}
	if usedCount > 32768 || refreshCount >= 4096 {
		return out, ErrAuthorizationCapacity
	}
	if consumed != "" {
		used := g
		used.UsedAt = now
		o.Used[consumed] = used
		delete(o.Refresh, consumed)
	}
	if code != "" {
		delete(o.Codes, code)
		if c, ok := o.Clients[g.Client]; ok && c.Pending {
			c.Pending = false
			o.Clients[g.Client] = c
		}
	}
	access, refresh := Token(), Token()
	g.UsedAt = 0
	g.Expires = min(now+int64(accessLifetime/time.Second), g.FamilyExpires)
	lifetime := g.Expires - now
	o.Access[Digest(access)] = g
	g.Expires = min(now+int64(refreshLifetime/time.Second), g.FamilyExpires)
	o.Refresh[Digest(refresh)] = g
	if err = o.Persist(); err != nil {
		return out, errors.Join(ErrTemporarilyUnavailable, err)
	}
	return TokenResponse{Access: access, Refresh: refresh, Type: "Bearer", ExpiresIn: lifetime, Scope: g.Scope}, nil
}

// makeSubjectRoom keeps one account below subjectRefreshLimit live refresh
// tokens by dropping its oldest other authorizations. It reports false when
// only the current authorization holds them.
func (o *State) makeSubjectRoom(subject, current string) bool {
	for {
		held, oldest := 0, ""
		var oldestExpires int64
		for _, g := range o.Refresh {
			if g.Subject != subject {
				continue
			}
			held++
			if g.Family != current && (oldest == "" || g.FamilyExpires < oldestExpires) {
				oldest, oldestExpires = g.Family, g.FamilyExpires
			}
		}
		if held < subjectRefreshLimit {
			return true
		}
		if oldest == "" {
			return false
		}
		o.dropFamily(oldest)
	}
}

// TokenSubject reports the account behind a token request's refresh token or
// authorization code without consuming it, so admission can budget exchanges
// of issued secrets apart from unauthenticated traffic.
func (o *State) TokenSubject(form url.Values) (string, bool) {
	switch form.Get("grant_type") {
	case "authorization_code":
		key := Digest(form.Get("code"))
		return lookup(o, func() (string, bool) { g, ok := o.Codes[key]; return g.Subject, ok })
	case "refresh_token":
		key := Digest(form.Get("refresh_token"))
		return lookup(o, func() (string, bool) {
			if g, ok := o.Refresh[key]; ok {
				return g.Subject, true
			}
			g, ok := o.Used[key]
			return g.Subject, ok
		})
	}
	return "", false
}

func (o *State) IssueCode(g GrantRequest) (code string, err error) {
	if err = o.write(); err != nil {
		return "", err
	}
	defer o.finish(&err)
	now := time.Now().Unix()
	for k, v := range o.Codes {
		if v.Expires < now {
			delete(o.Codes, k)
		}
	}
	if len(o.Codes) > 256 {
		return "", errors.New("too many pending authorizations")
	}
	code = Token()
	o.Codes[Digest(code)] = g
	if err = o.Persist(); err != nil {
		return "", err
	}
	return code, nil
}
func (o *State) RegisterClient(name string, redirects []string) (_ Client, err error) {
	if err = o.write(); err != nil {
		return Client{}, err
	}
	defer o.finish(&err)
	now := time.Now()
	if now.Sub(o.Window) > time.Minute {
		o.Window = now
		o.Registrations = 0
	}
	if o.Registrations >= 10 {
		return Client{}, ErrRegistrationRate
	}
	o.Registrations++
	for id, c := range o.Clients {
		if c.Pending && c.Created < now.Add(-pendingClientLifetime).Unix() {
			delete(o.Clients, id)
			continue
		}
		if c.Created < now.Add(-30*24*time.Hour).Unix() {
			active := false
			for _, g := range o.Refresh {
				if g.Client == id && g.Expires > now.Unix() {
					active = true
					break
				}
			}
			if !active {
				delete(o.Clients, id)
			}
		}
	}
	// A full table displaces the oldest registration that never connected.
	evicted, evict := Client{}, false
	if len(o.Clients) >= 256 {
		for _, c := range o.Clients {
			if c.Pending && (!evict || c.Created < evicted.Created) {
				evicted, evict = c, true
			}
		}
		if !evict {
			return Client{}, ErrRegistrationCapacity
		}
		delete(o.Clients, evicted.ID)
	}
	c := Client{ID: Token(), Name: name, Redirects: slices.Clone(redirects), Created: now.Unix(), Pending: true}
	o.Clients[c.ID] = c
	if err = o.Persist(); err != nil {
		delete(o.Clients, c.ID)
		if evict {
			o.Clients[evicted.ID] = evicted
		}
		return Client{}, err
	}
	c.Redirects = slices.Clone(c.Redirects)
	return c, nil
}

func (o *State) Client(id string) (Client, bool) {
	return lookup(o, func() (Client, bool) {
		v, ok := o.Clients[id]
		v.Redirects = slices.Clone(v.Redirects)
		return v, ok
	})
}
func (o *State) AccessGrant(hash string) (Grant, bool) {
	return lookup(o, func() (Grant, bool) { v, ok := o.Access[hash]; return v, ok })
}
func (o *State) Session(hash string) (Grant, bool) {
	return lookup(o, func() (Grant, bool) { v, ok := o.Sessions[hash]; return v, ok })
}
func cloneCredential(v APICredential) APICredential {
	v.Projects = slices.Clone(v.Projects)
	v.Connections = slices.Clone(v.Connections)
	v.Scopes = slices.Clone(v.Scopes)
	v.Audiences = slices.Clone(v.Audiences)
	return v
}
func (o *State) Credential(hash string) (APICredential, bool) {
	return lookup(o, func() (APICredential, bool) { v, ok := o.Credentials[hash]; return cloneCredential(v), ok })
}
func (o *State) BearerSnapshot(hash string) (Grant, bool, APICredential, bool) {
	type bearer struct {
		access     Grant
		oauth      bool
		credential APICredential
		api        bool
	}
	b, _ := lookup(o, func() (bearer, bool) {
		g, ok := o.Access[hash]
		c, credential := o.Credentials[hash]
		return bearer{g, ok, cloneCredential(c), credential}, ok || credential
	})
	return b.access, b.oauth, b.credential, b.api
}
func (o *State) CredentialList(user, id string) []APICredential {
	o.read()
	defer o.Mu.Unlock()
	out := []APICredential{}
	for _, g := range o.Credentials {
		if g.Subject == user && (id == "" || g.ID == id) {
			out = append(out, cloneCredential(g))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created < out[j].Created })
	return out
}
func (o *State) CreateSession(g Grant) (token string, err error) {
	if err = o.write(); err != nil {
		return "", err
	}
	defer o.finish(&err)
	token = Token()
	for k, v := range o.Sessions {
		if v.Expires <= time.Now().Unix() {
			delete(o.Sessions, k)
		}
	}
	o.Sessions[Digest(token)] = g
	if err = o.Persist(); err != nil {
		delete(o.Sessions, Digest(token))
		return "", err
	}
	return token, nil
}
func (o *State) DeleteSession(hash string) (err error) {
	if err = o.write(); err != nil {
		return err
	}
	defer o.finish(&err)
	g, ok := o.Sessions[hash]
	delete(o.Sessions, hash)
	if err = o.Persist(); err != nil {
		if ok {
			o.Sessions[hash] = g
		}
		return err
	}
	return nil
}
func (o *State) RevokeToken(token string) (err error) {
	// Unknown tokens never become known, so revoking one needs no write.
	if !o.knows(Digest(token)) {
		return nil
	}
	if err = o.write(); err != nil {
		return err
	}
	defer o.finish(&err)
	key := Digest(token)
	_, credential := o.Credentials[key]
	g, access := o.Access[key]
	if v, ok := o.Refresh[key]; ok {
		g = v
		access = true
	}
	if v, ok := o.Used[key]; ok {
		g = v
		access = true
	}
	if !credential && !access {
		return nil
	}
	oldCredentials, oldAccess, oldRefresh, oldUsed := maps.Clone(o.Credentials), maps.Clone(o.Access), maps.Clone(o.Refresh), maps.Clone(o.Used)
	delete(o.Credentials, key)
	if access {
		o.RevokeFamily(g.Family)
	}
	if err = o.Persist(); err != nil {
		o.Credentials, o.Access, o.Refresh, o.Used = oldCredentials, oldAccess, oldRefresh, oldUsed
		return err
	}
	return nil
}
func (o *State) knows(key string) bool {
	_, known := lookup(o, func() (struct{}, bool) {
		_, credential := o.Credentials[key]
		_, access := o.Access[key]
		_, refresh := o.Refresh[key]
		_, used := o.Used[key]
		return struct{}{}, credential || access || refresh || used
	})
	return known
}
func (o *State) RevokeCredential(projects *catalog.Catalog, actor, user, id string) error {
	epoch := projects.Epoch(actor)
	allowed := func() bool {
		return projects.ValidSession(actor, epoch) && (actor == user || projects.IsAdmin(actor))
	}
	if !allowed() {
		return workspace.ErrForbidden
	}
	if err := projects.RecordActivity(actor, "credential.revoke", id, "pending", map[string]string{"user": user}); err != nil {
		return err
	}
	err := func() (err error) {
		if err = o.write(); err != nil {
			return err
		}
		defer o.finish(&err)
		for key, g := range o.Credentials {
			if g.Subject == user && g.ID == id && allowed() {
				delete(o.Credentials, key)
				if err = o.Persist(); err != nil {
					o.Credentials[key] = g
				}
				return err
			}
		}
		return workspace.ErrForbidden
	}()
	outcome := "succeeded"
	if err != nil {
		outcome = "failed"
	}
	receipt := projects.RecordActivity(actor, "credential.revoke", id, outcome, map[string]string{"user": user})
	return errors.Join(err, receipt)
}
