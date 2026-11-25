package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"

	"github.com/jackc/pgx/v5"
)

type Client struct {
	ID        string   `json:"client_id"`
	Name      string   `json:"client_name"`
	Redirects []string `json:"redirect_uris"`
	Created   int64    `json:"created"`
	// Pending marks a registration that has not completed a code exchange.
	// Registrations stored before this marker are never treated as pending.
	Pending bool `json:"pending,omitempty"`
}
type GrantRequest struct {
	Client, Redirect, Challenge, Scope, Resource, State string
	Subject                                             string
	Expires                                             int64
	Epoch                                               uint64
}
type Grant struct {
	UsedAt                  int64  `json:"usedAt,omitempty"`
	Family                  string `json:"family,omitempty"`
	FamilyExpires           int64  `json:"familyExpires,omitempty"`
	Client, Scope, Resource string
	Subject                 string
	Expires                 int64
	Epoch                   uint64
	RecoveryKey             string `json:"recoveryKey,omitempty"`
}

// State is the instance's OAuth, browser-session and API-credential state. It
// is one versioned Postgres document, so every gateway sees the same grants. A
// lookup catches up with the stored version at most once per freshness window,
// and again when it does not find what it looks for, since another gateway may
// have just issued it: only another gateway's removals can be that stale here.
// Each change holds the document's row lock from its read through its commit,
// so this gateway's own changes are current at once. Maps are guarded by Mu.
type State struct {
	Credentials map[string]APICredential `json:"credentials,omitempty"`
	Mu          sync.Mutex               `json:"-"`
	Clients     map[string]Client        `json:"clients"`
	Access      map[string]Grant         `json:"access"`
	Refresh     map[string]Grant         `json:"refresh"`
	Sessions    map[string]Grant         `json:"sessions,omitempty"`
	Used        map[string]Grant         `json:"usedRefresh,omitempty"`
	// Codes are shared so the token exchange can reach any gateway.
	Codes         map[string]GrantRequest `json:"codes,omitempty"`
	Registrations int                     `json:"registrations,omitempty"`
	Window        time.Time               `json:"window,omitzero"`
	db            *pg.DB
	version       int64 // stored version the maps reflect; -1 forces a reload
	// checked is when the maps last matched the stored document; a lookup
	// reads the database again once it is older than freshness.
	checked    time.Time
	freshness  time.Duration
	refreshing chan struct{} // closed when the read in progress ends
	tx         pgx.Tx
	ctx        context.Context
	cancel     context.CancelFunc
	written    int64     // version Persist wrote inside tx, committed by finish
	unstored   bool      // a Persist inside tx failed
	locked     time.Time // when write read the document it holds locked
}

const (
	stateKind = "oauth"
	stateID   = "state"
	// One database call, and so one held Mu, never outlasts this.
	stateTimeout = 10 * time.Second
	// How stale another gateway's committed change may be here.
	stateFreshness = 250 * time.Millisecond
)

func OpenState(db *pg.DB) (*State, error) {
	o := &State{db: db, version: -1, freshness: stateFreshness}
	o.empty()
	ctx, cancel := context.WithTimeout(context.Background(), stateTimeout)
	defer cancel()
	o.Mu.Lock()
	defer o.Mu.Unlock()
	started := time.Now()
	version, body, err := db.ReadDocument(ctx, db.Pool, stateKind, stateID, -1)
	if err != nil {
		return nil, err
	}
	if err = o.decode(version, body); err != nil {
		return nil, err
	}
	o.checked = started
	return o, nil
}

func (o *State) empty() {
	o.Clients, o.Access, o.Refresh, o.Sessions, o.Used = map[string]Client{}, map[string]Grant{}, map[string]Grant{}, map[string]Grant{}, map[string]Grant{}
	o.Codes, o.Credentials, o.Registrations, o.Window = map[string]GrantRequest{}, map[string]APICredential{}, 0, time.Time{}
}

// decode replaces the maps with a stored document after validating it whole.
func (o *State) decode(version int64, body []byte) error {
	stored := &State{}
	if err := json.Unmarshal(body, stored); err != nil {
		return fmt.Errorf("invalid OAuth state: %w", err)
	}
	if version > 0 && (stored.Clients == nil || stored.Access == nil || stored.Refresh == nil) {
		return errors.New("invalid OAuth state")
	}
	for _, grants := range []map[string]Grant{stored.Access, stored.Refresh, stored.Used} {
		for _, g := range grants {
			if g.Family == "" || g.FamilyExpires == 0 {
				return errors.New("invalid OAuth state: grants must carry an authorization family")
			}
		}
	}
	for _, g := range stored.Credentials {
		if g.CredentialSpec.Validate() != nil || g.ID == "" || g.Subject == "" || g.Expires <= g.Created {
			return errors.New("invalid API credential state")
		}
	}
	o.empty()
	for target, source := range map[*map[string]Grant]map[string]Grant{&o.Access: stored.Access, &o.Refresh: stored.Refresh, &o.Sessions: stored.Sessions, &o.Used: stored.Used} {
		if source != nil {
			*target = source
		}
	}
	if stored.Clients != nil {
		o.Clients = stored.Clients
	}
	if stored.Codes != nil {
		o.Codes = stored.Codes
	}
	if stored.Credentials != nil {
		o.Credentials = stored.Credentials
	}
	o.Registrations, o.Window, o.version = stored.Registrations, stored.Window, version
	return nil
}

// SetFreshness sets how stale another gateway's changes may be here. OpenState
// uses 250 ms; zero reads the database on every lookup.
func (o *State) SetFreshness(window time.Duration) {
	o.Mu.Lock()
	defer o.Mu.Unlock()
	o.freshness = window
}

// read locks Mu over maps at most one freshness window old. It reports
// whether it read the database to get them.
func (o *State) read() bool {
	o.Mu.Lock()
	return o.catchUp(time.Now().Add(-o.freshness))
}

// lookup runs find over the maps and returns with Mu unlocked. A miss on
// cached maps is looked up once more in the stored document, so a token
// another gateway has just issued is never refused for being new; while the
// database is unreachable the emptied maps refuse everything instead.
func lookup[T any](o *State, find func() (T, bool)) (T, bool) {
	queried := o.read()
	defer o.Mu.Unlock()
	v, ok := find()
	if !ok && !queried && o.version >= 0 {
		o.catchUp(time.Now())
		v, ok = find()
	}
	return v, ok
}

// catchUp, with Mu held, makes the maps match the stored document as of since
// at the latest. The database is read without Mu, by one caller at a time,
// so lookups that memory can serve never wait for it. It reports whether this
// call read the database.
func (o *State) catchUp(since time.Time) (read bool) {
	for o.checked.Before(since) {
		if wait := o.refreshing; wait != nil {
			o.Mu.Unlock()
			<-wait
			o.Mu.Lock()
			continue
		}
		read = o.check() || read
	}
	return read
}

// check reads the stored document, releasing Mu meanwhile. A change this
// gateway made in the meantime already holds current maps, so it decides. When
// the database cannot be read the maps are emptied, so lookups fail closed;
// the failure also stands for the window, so an outage never queues every
// lookup behind its own database timeout.
func (o *State) check() bool {
	done := make(chan struct{})
	known, started := o.version, time.Now()
	o.refreshing = done
	o.Mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), stateTimeout)
	version, body, err := o.db.ReadDocument(ctx, o.db.Pool, stateKind, stateID, known)
	cancel()
	o.Mu.Lock()
	o.refreshing = nil
	close(done)
	if o.version != known {
		return false
	}
	if err == nil && version != known {
		err = o.decode(version, body)
	}
	if err != nil {
		log.Printf("OAuth state unavailable: %v", err)
		o.empty()
		o.version = -1
	}
	o.checked = started
	return true
}

// stale makes the next lookup reload the stored document.
func (o *State) stale() { o.version, o.checked = -1, time.Time{} }

// write locks Mu and the stored document for one change; finish commits what
// Persist wrote, or rolls back. A failure leaves Mu unlocked.
func (o *State) write() error {
	o.Mu.Lock()
	o.ctx, o.cancel = context.WithTimeout(context.Background(), stateTimeout)
	locked := time.Now()
	tx, err := o.db.Pool.Begin(o.ctx)
	if err == nil {
		var version int64
		var body []byte
		if version, body, err = o.db.LockDocument(o.ctx, tx, stateKind, stateID, o.version); err == nil && version != o.version {
			err = o.decode(version, body)
		}
		if err != nil {
			tx.Rollback(o.ctx)
		}
	}
	if err != nil {
		o.cancel()
		o.stale()
		o.Mu.Unlock()
		return errors.Join(ErrTemporarilyUnavailable, err)
	}
	o.tx, o.written, o.locked = tx, 0, locked
	return nil
}

// finish ends write. Without a Persist it rolls back. Validation never edits
// the maps, but after a failed Persist the next call reloads them in case the
// change left unstored edits.
func (o *State) finish(err *error) {
	tx, ctx, written, unstored := o.tx, o.ctx, o.written, o.unstored
	o.tx, o.written, o.unstored = nil, 0, false
	defer o.Mu.Unlock()
	defer o.cancel()
	if written == 0 {
		tx.Rollback(ctx)
		if unstored {
			o.stale()
		} else {
			o.checked = o.locked
		}
		return
	}
	if commitErr := tx.Commit(ctx); commitErr != nil {
		o.stale()
		*err = errors.Join(*err, ErrTemporarilyUnavailable, commitErr)
		return
	}
	// The row lock kept the document ours from its read through this commit.
	o.version, o.checked = written, o.locked
}

// Persist stores the maps. Inside write it joins that transaction; otherwise,
// as in tests that edit the maps under Mu, it is one conditional write.
func (o *State) Persist() error {
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	if o.tx != nil {
		version := o.version
		if o.written != 0 {
			version = o.written
		}
		if o.written, err = o.db.WriteDocument(o.ctx, o.tx, stateKind, stateID, version, raw); err != nil {
			o.unstored = true
		}
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), stateTimeout)
	defer cancel()
	stored := time.Now()
	version, err := o.db.WriteDocument(ctx, o.db.Pool, stateKind, stateID, max(o.version, 0), raw)
	if err != nil {
		o.stale()
		return err
	}
	o.version, o.checked = version, stored
	return nil
}
func Digest(value string) string { h := sha256.Sum256([]byte(value)); return hex.EncodeToString(h[:]) }

// RevokeFamily ends an OAuth authorization, including API credentials minted
// through it. Its refresh tokens stay recorded for replay detection.
func (o *State) RevokeFamily(family string) {
	for key, g := range o.Refresh {
		if g.Family == family {
			o.Used[key] = g
		}
	}
	o.dropFamily(family)
}

// dropFamily deletes an authorization's tokens and the credentials minted through it.
func (o *State) dropFamily(family string) {
	if family == "" {
		return // grants always carry a family; never match an empty one
	}
	for key, g := range o.Access {
		if g.Family == family {
			delete(o.Access, key)
		}
	}
	for key, g := range o.Refresh {
		if g.Family == family {
			delete(o.Refresh, key)
		}
	}
	minted := Digest(family)
	for key, c := range o.Credentials {
		if c.FamilyDigest == minted {
			delete(o.Credentials, key)
		}
	}
}

type CredentialSpec struct {
	Connections []string `json:"connections,omitempty"`
	Name        string   `json:"name"`
	Projects    []string `json:"projects"`
	AllProjects bool     `json:"allProjects,omitempty"`
	Scopes      []string `json:"scopes"`
	Audiences   []string `json:"audiences"`
	Days        int      `json:"days"`
}

type APICredential struct {
	CredentialSpec
	ID      string `json:"id"`
	Subject string `json:"subject"`
	Epoch   uint64 `json:"epoch"`
	Created int64  `json:"created"`
	Expires int64  `json:"expires"`
	// FamilyDigest identifies the OAuth authorization that issued the credential;
	// revoking that authorization revokes the credential. Only a digest is kept
	// because credential metadata is returned to clients. Empty for browser,
	// recovery and operator issue, and for credentials stored before it existed.
	FamilyDigest string `json:"familyDigest,omitempty"`
}
type IssuedCredential struct {
	APICredential
	Token string `json:"token"`
}
type CredentialContext struct {
	Hash  string
	Grant APICredential
	// admitted marks the credential an invocation's admission resolved. It is
	// never stored, so a credential restored from persisted work is unmarked.
	admitted bool
}

// Admitted reports whether this invocation's admission resolved the credential,
// so the invocation's own later checks need not look it up again.
func (c CredentialContext) Admitted() bool { return c.admitted }

func (in CredentialSpec) Validate() error {
	if len(in.Connections) > 128 {
		return errors.New("too many connection restrictions")
	}
	for _, id := range in.Connections {
		if id == "" || len(id) > 96 || strings.ContainsAny(id, "/\\*\r\n") {
			return errors.New("invalid connection restriction")
		}
	}
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 100 || in.Days < 1 || in.Days > 365 || len(in.Projects) > 256 || in.AllProjects == (len(in.Projects) > 0) || len(in.Scopes) == 0 || len(in.Audiences) == 0 {
		return errors.New("provide a name, 1–365 days, scopes, audiences, and either projects or allProjects")
	}
	for _, scope := range in.Scopes {
		if scope != "notes:read" && scope != "notes:write" && scope != "notes:manage" && scope != "data:read" && scope != "data:write" {
			return errors.New("unknown credential scope")
		}
	}
	for _, audience := range in.Audiences {
		if audience != "api" && audience != "mcp" {
			return errors.New("audiences must be api and/or mcp")
		}
	}
	return nil
}

// IssueCredentialFor mints a credential that never exceeds the calling
// authorization: an OAuth caller (ScopeKey and FamilyKey in ctx) can request
// only its own scopes, and the credential ends with that authorization.
func IssueCredentialFor(ctx context.Context, projects *catalog.Catalog, o *State, issuer, actor string, spec CredentialSpec) (IssuedCredential, error) {
	issuerEpoch, actorEpoch := projects.Epoch(issuer), projects.Epoch(actor)
	authorized := func() bool {
		return projects.ValidSession(issuer, issuerEpoch) && projects.ValidSession(actor, actorEpoch) &&
			(issuer == actor || projects.IsAdmin(issuer) && projects.IsService(actor))
	}
	if projects.IsService(actor) && spec.AllProjects {
		return IssuedCredential{}, workspace.ErrForbidden
	}
	if issuer != actor && (!projects.IsAdmin(issuer) || !projects.IsService(actor)) {
		return IssuedCredential{}, workspace.ErrForbidden
	}
	if err := spec.Validate(); err != nil {
		return IssuedCredential{}, err
	}
	for _, scope := range spec.Scopes {
		if !HasScope(ctx, scope) {
			return IssuedCredential{}, fmt.Errorf("%w: this authorization does not include %s", workspace.ErrForbidden, scope)
		}
	}
	family, _ := ctx.Value(FamilyKey{}).(string)
	for _, project := range spec.Projects {
		if _, err := projects.Access(actor, project, "read"); err != nil {
			return IssuedCredential{}, err
		}
	}
	for _, connection := range spec.Connections {
		if !projects.ConnectionAllowed(actor, connection, "") {
			return IssuedCredential{}, workspace.ErrForbidden
		}
	}
	if !authorized() {
		return IssuedCredential{}, workspace.ErrForbidden
	}
	token := "fmn_" + Token()
	now := time.Now().Unix()
	grant := cloneCredential(APICredential{CredentialSpec: spec, ID: Token(), Subject: actor, Epoch: actorEpoch, Created: now, Expires: now + int64(spec.Days)*86400})
	details := map[string]string{"user": actor}
	if err := projects.RecordActivity(issuer, "credential.issue", grant.ID, "pending", details); err != nil {
		return IssuedCredential{}, err
	}
	key := Digest(token)
	save := func() (err error) {
		if err = o.write(); err != nil {
			return err
		}
		defer o.finish(&err)
		if !authorized() {
			return workspace.ErrForbidden
		}
		if family != "" {
			issuing, active := o.familyGrant(family, now)
			if !active {
				return workspace.ErrForbidden
			}
			grant.FamilyDigest = Digest(family)
			grant.Expires = min(grant.Expires, issuing.FamilyExpires)
		}
		if len(o.Credentials) >= 4096 {
			return errors.New("credential limit reached; revoke unused credentials")
		}
		o.Credentials[key] = grant
		if err = o.Persist(); err != nil {
			delete(o.Credentials, key)
			return err
		}
		if !authorized() {
			delete(o.Credentials, key)
			return errors.Join(workspace.ErrForbidden, o.Persist())
		}
		return nil
	}
	err := save()
	outcome := "succeeded"
	if err != nil {
		outcome = "failed"
	}
	receipt := projects.RecordActivity(issuer, "credential.issue", grant.ID, outcome, details)
	if receipt != nil && err == nil {
		err = func() (err error) {
			if err = o.write(); err != nil {
				return err
			}
			defer o.finish(&err)
			delete(o.Credentials, key)
			return o.Persist()
		}()
	}
	if err = errors.Join(err, receipt); err != nil {
		return IssuedCredential{}, err
	}
	return IssuedCredential{cloneCredential(grant), token}, nil
}

// Context keys identify trusted application admission, never document content.
type IdentityKey struct{}
type ScopeKey struct{}

// FamilyKey holds the OAuth authorization family of an admitted access token.
type FamilyKey struct{}
type ClientKey struct{}
type CredentialKey struct{}

func Subject(ctx context.Context) string { actor, _ := ctx.Value(IdentityKey{}).(string); return actor }
func HasScope(ctx context.Context, required string) bool {
	scope, restricted := ctx.Value(ScopeKey{}).(string)
	return !restricted || strings.Contains(" "+scope+" ", " "+required+" ")
}
func CredentialFrom(ctx context.Context) (CredentialContext, bool) {
	grant, ok := ctx.Value(CredentialKey{}).(CredentialContext)
	return grant, ok
}

// CheckCredential looks up the credential in ctx, if any, and refuses one that
// is no longer current. Disclosures call it even when admission resolved it.
func (o *State) CheckCredential(ctx context.Context, projects *catalog.Catalog) error {
	credential, ok := CredentialFrom(ctx)
	if !ok {
		return nil
	}
	current, exists := lookup(o, func() (APICredential, bool) {
		current, exists := o.Credentials[credential.Hash]
		return current, exists
	})
	if !exists || current.ID != credential.Grant.ID || current.Expires <= time.Now().Unix() || !projects.ValidSession(current.Subject, current.Epoch) {
		return workspace.ErrForbidden
	}
	return nil
}
func Token() string {
	raw := make([]byte, 32)
	rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// AccessToken binds bearer admission to the current account epoch and resource.
func (o *State) AccessToken(projects *catalog.Catalog, token, resource string) (Grant, bool) {
	grant, ok := o.AccessGrant(Digest(token))
	return grant, ok && grant.Family != "" && grant.FamilyExpires > time.Now().Unix() && grant.Expires > time.Now().Unix() && grant.Resource == resource && projects.ValidSession(grant.Subject, grant.Epoch)
}

// CredentialToken admits a credential for one invocation.
func (o *State) CredentialToken(projects *catalog.Catalog, token, audience string) (CredentialContext, bool) {
	key := Digest(token)
	grant, ok := o.Credential(key)
	return CredentialContext{Hash: key, Grant: grant, admitted: true}, ok && grant.Expires > time.Now().Unix() && slices.Contains(grant.Audiences, audience) && projects.ValidSession(grant.Subject, grant.Epoch)
}

// ProjectAllowed reports whether the credential in ctx, if any, extends to
// project. It does not look the credential up; callers check it once per
// invocation.
func ProjectAllowed(ctx context.Context, project string) bool {
	credential, restricted := CredentialFrom(ctx)
	return !restricted || credential.Grant.AllProjects || slices.Contains(credential.Grant.Projects, project)
}

// FamilyGrant reports current consent, independent of the rotating access token.
func (o *State) FamilyGrant(family string) (Grant, bool) {
	return lookup(o, func() (Grant, bool) { return o.familyGrant(family, time.Now().Unix()) })
}
func (o *State) familyGrant(family string, now int64) (Grant, bool) {
	if family == "" {
		return Grant{}, false
	}
	for _, g := range o.Refresh {
		if g.Family == family && g.Expires > now && g.FamilyExpires > now {
			return g, true
		}
	}
	return Grant{}, false
}
func (o *State) familyActive(family string, now int64) bool {
	_, ok := o.familyGrant(family, now)
	return ok
}
