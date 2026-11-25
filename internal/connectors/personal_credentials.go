package connectors

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/integrations"
	"forgetmenote/internal/jsonfile"
	"forgetmenote/internal/pg"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type OAuthApplication struct {
	ClientID   string `json:"clientID"`
	SecretFile string `json:"secretFile"`
	Workspace  string `json:"workspace,omitempty"`
}

type PersonalConfig struct {
	Version int               `json:"version"`
	KeyFile string            `json:"keyFile"`
	Google  *OAuthApplication `json:"google,omitempty"`
	Slack   *OAuthApplication `json:"slack,omitempty"`
}

type PersonalConnection struct {
	Provider   string   `json:"provider"`
	Subject    string   `json:"subject,omitempty"`
	Email      string   `json:"email,omitempty"`
	Workspace  string   `json:"workspace,omitempty"`
	Scopes     []string `json:"scopes,omitempty"`
	Generation uint64   `json:"generation"`
	State      string   `json:"state"`
}

type personalCredential struct {
	PersonalConnection
	Owner        string    `json:"owner"`
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken"`
	Expires      time.Time `json:"expires"`
	Refreshing   bool      `json:"refreshing,omitempty"`
	// RefreshHolder is the gateway refreshing the grant. Only once it is gone may
	// another gateway conclude the refresh was interrupted.
	RefreshHolder string `json:"refreshHolder,omitempty"`
}

type personalPending struct {
	Owner, Provider, Binding, Verifier, Redirect string
	Generation                                   uint64
	Expires                                      time.Time
}

// PersonalConnections owns renewable user grants. The catalog remains the
// authority for whether their owner or any workflow may use a grant. Grants are
// sealed rows in Postgres, so every gateway reads the same ones; each call
// reads the rows it needs and writes only over the version it read.
type PersonalConnections struct {
	mu           sync.Mutex
	db           *pg.DB
	key          cipher.AEAD
	config       PersonalConfig
	client       *http.Client
	refreshLocks map[string]*sync.Mutex
	// Fixed production origins; loopback fixtures can replace them in tests.
	googleAuth, googleToken, googleUser, googleAPI, slackAuth, slackAPI string
}

func LoadPersonalConfig(path string) (PersonalConfig, error) {
	var config PersonalConfig
	if err := jsonfile.Load(path, 1<<20, &config); err != nil {
		return config, errors.New("invalid personal connections configuration")
	}
	return config, nil
}

// ErrPersonalUnavailable reports that the database could not be read or written.
var ErrPersonalUnavailable = errors.New("personal connections are unavailable; retry")

// ErrPersonalChanged reports a grant another call or gateway wrote after this
// one read it.
var ErrPersonalChanged = errors.New("personal connection changed while this request was in progress; retry")

const personalStorageTimeout = 10 * time.Second

func OpenPersonalConnections(db *pg.DB, config PersonalConfig) (*PersonalConnections, error) {
	if config.Version != 1 || !filepath.IsAbs(config.KeyFile) {
		return nil, errors.New("personal connections require version 1 and an absolute keyFile")
	}
	if config.Slack != nil {
		if err := integrations.Check("slack"); err != nil {
			return nil, fmt.Errorf("personal connections: %w", err)
		}
	}
	for _, app := range []*OAuthApplication{config.Google, config.Slack} {
		if app != nil && (strings.TrimSpace(app.ClientID) == "" || !filepath.IsAbs(app.SecretFile)) {
			return nil, errors.New("OAuth applications require clientID and an absolute secretFile")
		}
	}
	aead, err := personalCipher(config.KeyFile)
	if err != nil {
		return nil, err
	}
	p := &PersonalConnections{db: db, key: aead, config: config, refreshLocks: map[string]*sync.Mutex{}, client: noRedirectClient(nil, 30*time.Second), googleAuth: "https://accounts.google.com/o/oauth2/v2/auth", googleToken: "https://oauth2.googleapis.com/token", googleUser: "https://openidconnect.googleapis.com/v1/userinfo", googleAPI: "https://www.googleapis.com", slackAuth: "https://slack.com/oauth/v2/authorize", slackAPI: "https://slack.com/api"}
	ctx, cancel := context.WithTimeout(context.Background(), personalStorageTimeout)
	defer cancel()
	// Every grant must decrypt under this key before serving.
	stored, err := p.rows(ctx, db.Pool, "", "")
	if err != nil {
		return nil, err
	}
	for _, s := range stored {
		if s.Refreshing {
			if _, err = p.interrupted(ctx, s.personalCredential, s.version, true); err != nil {
				return nil, err
			}
		}
	}
	return p, nil
}

// personalCipher reads the personal credential key, which stays a mounted file
// outside the data directory and the database.
func personalKeyBytes(keyFile string) ([]byte, error) {
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, errors.New("personal credential encryption key unavailable")
	}
	key := raw
	if len(key) != 32 {
		key, err = hex.DecodeString(strings.TrimSpace(string(raw)))
	}
	if err != nil || len(key) != 32 {
		return nil, errors.New("personal credential key must contain 32 random bytes or 64 hex characters")
	}
	return key, nil
}

func personalCipher(keyFile string) (cipher.AEAD, error) {
	key, err := personalKeyBytes(keyFile)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// interrupted settles a refresh whose gateway stopped: this gateway's previous
// store when opening, or another gateway that is gone. Its token may already
// have rotated at the provider, so the grant needs reauthorization rather than
// reuse. It reports false for a refresh still in progress elsewhere.
func (p *PersonalConnections) interrupted(ctx context.Context, c personalCredential, version int64, opening bool) (bool, error) {
	if c.RefreshHolder == p.db.Holder {
		if !opening {
			return false, nil
		}
	} else if c.RefreshHolder != "" {
		gone, err := p.db.Gone(ctx, p.db.Pool, c.RefreshHolder)
		if err != nil {
			return false, fmt.Errorf("%w: %w", ErrPersonalUnavailable, err)
		}
		if !gone {
			return false, nil
		}
	}
	c.State, c.Refreshing, c.RefreshHolder = "reauthorize", false, ""
	return true, p.save(ctx, p.db.Pool, c, version)
}

const personalSeal = "ForgetMeNote personal credentials v1"

// sealCredential encrypts a grant bound to its account and provider, so a row
// cannot be moved to another identity without failing to open.
func (p *PersonalConnections) sealCredential(c personalCredential) ([]byte, error) {
	plain, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, p.key.NonceSize())
	rand.Read(nonce)
	return p.key.Seal(nonce, nonce, plain, []byte(personalSeal+"\x00"+personalKey(c.Owner, c.Provider))), nil
}

func (p *PersonalConnections) openCredential(owner, provider string, raw []byte) (personalCredential, error) {
	var c personalCredential
	if len(raw) < p.key.NonceSize() {
		return c, errors.New("cannot decrypt personal credentials; restore the matching key")
	}
	plain, err := p.key.Open(nil, raw[:p.key.NonceSize()], raw[p.key.NonceSize():], []byte(personalSeal+"\x00"+personalKey(owner, provider)))
	if err != nil || json.Unmarshal(plain, &c) != nil {
		return c, errors.New("cannot decrypt personal credentials; restore the matching key")
	}
	if c.Owner != owner || c.Provider != provider || !validPersonalOwner(owner) || c.Generation == 0 {
		return c, errors.New("invalid personal credential identity")
	}
	return c, nil
}

type storedCredential struct {
	personalCredential
	version int64
}

// rows reads the grants of one owner, one provider, or all when both are empty.
func (p *PersonalConnections) rows(ctx context.Context, q pg.Querier, owner, provider string) ([]storedCredential, error) {
	rows, err := q.Query(ctx, `SELECT owner, provider, version, sealed FROM personal_credentials
		WHERE org = $1 AND ($2 = '' OR owner = $2) AND ($3 = '' OR provider = $3) ORDER BY owner COLLATE "C", provider COLLATE "C"`, p.db.Org, owner, provider)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPersonalUnavailable, err)
	}
	defer rows.Close()
	out := []storedCredential{}
	for rows.Next() {
		var s storedCredential
		var rowOwner, rowProvider string
		var raw []byte
		if err = rows.Scan(&rowOwner, &rowProvider, &s.version, &raw); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrPersonalUnavailable, err)
		}
		if s.personalCredential, err = p.openCredential(rowOwner, rowProvider, raw); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPersonalUnavailable, err)
	}
	return out, nil
}

// load reads one grant, or the empty grant at version 0 when there is none.
func (p *PersonalConnections) load(ctx context.Context, q pg.Querier, owner, provider string) (personalCredential, int64, error) {
	stored, err := p.rows(ctx, q, owner, provider)
	if err != nil || len(stored) == 0 {
		return personalCredential{}, 0, err
	}
	return stored[0].personalCredential, stored[0].version, nil
}

// save writes c over the version read, 0 for a grant that had no row, and
// returns ErrPersonalChanged when another writer committed first.
func (p *PersonalConnections) save(ctx context.Context, q pg.Querier, c personalCredential, version int64) error {
	sealed, err := p.sealCredential(c)
	if err != nil {
		return err
	}
	var written int64
	if version == 0 {
		tag, e := q.Exec(ctx, `INSERT INTO personal_credentials (org, owner, provider, version, sealed) VALUES ($1, $2, $3, 1, $4)
			ON CONFLICT DO NOTHING`, p.db.Org, c.Owner, c.Provider, sealed)
		err, written = e, tag.RowsAffected()
	} else {
		tag, e := q.Exec(ctx, `UPDATE personal_credentials SET version = version + 1, sealed = $5
			WHERE org = $1 AND owner = $2 AND provider = $3 AND version = $4`, p.db.Org, c.Owner, c.Provider, version, sealed)
		err, written = e, tag.RowsAffected()
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPersonalUnavailable, err)
	}
	if written == 0 {
		return ErrPersonalChanged
	}
	return nil
}

func personalContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), personalStorageTimeout)
}

func validPersonalOwner(owner string) bool {
	_, err := hex.DecodeString(owner)
	return len(owner) == 32 && err == nil
}
func personalKey(owner, provider string) string { return owner + "/" + provider }
func personalRandom() string {
	var b [32]byte
	rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func (p *PersonalConnections) Providers() []string {
	out := []string{}
	if p.config.Google != nil {
		out = append(out, "google")
	}
	if p.config.Slack != nil {
		out = append(out, "slack")
	}
	return out
}

// List reports the owner's grant for each configured provider.
func (p *PersonalConnections) List(owner string) ([]PersonalConnection, error) {
	ctx, cancel := personalContext()
	defer cancel()
	stored, err := p.rows(ctx, p.db.Pool, owner, "")
	if err != nil {
		return nil, err
	}
	out := []PersonalConnection{}
	for _, provider := range p.Providers() {
		view := PersonalConnection{Provider: provider, State: "disconnected"}
		for _, s := range stored {
			if s.Provider == provider {
				view = s.PersonalConnection
				view.Scopes = slices.Clone(view.Scopes)
			}
		}
		out = append(out, view)
	}
	return out, nil
}

func (p *PersonalConnections) Disconnect(owner, provider string) error {
	if !validPersonalOwner(owner) || !slices.Contains(p.Providers(), provider) {
		return ErrInvalid
	}
	ctx, cancel := personalContext()
	defer cancel()
	if err := p.db.EndFlows(ctx, personalFlow(provider), owner); err != nil {
		return fmt.Errorf("%w: %w", ErrPersonalUnavailable, err)
	}
	old, version, err := p.load(ctx, p.db.Pool, owner, provider)
	if err != nil {
		return err
	}
	return p.save(ctx, p.db.Pool, personalCredential{Owner: owner, PersonalConnection: PersonalConnection{Provider: provider, Generation: old.Generation + 1, State: "disconnected"}}, version)
}

func (p *PersonalConnections) Check(owner, provider string, generation uint64) error {
	ctx, cancel := personalContext()
	defer cancel()
	c, _, err := p.load(ctx, p.db.Pool, owner, provider)
	if err != nil {
		return err
	}
	if c.State != "connected" || c.Generation != generation {
		return errors.New("personal connection changed or needs authorization")
	}
	return nil
}

// LinkedOwner finds the account whose connected grant is this provider
// identity, or none, including while the database is unreachable.
func (p *PersonalConnections) LinkedOwner(provider, workspace, subject string) string {
	ctx, cancel := personalContext()
	defer cancel()
	stored, err := p.rows(ctx, p.db.Pool, "", provider)
	if err != nil {
		return ""
	}
	for _, c := range stored {
		if c.Workspace == workspace && c.Subject == subject && c.State == "connected" {
			return c.Owner
		}
	}
	return ""
}
