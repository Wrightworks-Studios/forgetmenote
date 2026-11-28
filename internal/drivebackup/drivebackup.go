// Package drivebackup is the OAuth boundary of the Google Drive and Dropbox
// backup destinations: the authorization request, the code exchange, the
// provider account a grant belongs to, refreshing an access token and revoking
// a grant. It holds the operator's OAuth client secrets and the key that seals
// refresh tokens and encryption passwords for shared state. It never uploads:
// the external backup worker does, with an access token it leases.
package drivebackup

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

// The destination kinds, as bindings, leases and receipts name them.
const (
	Google  = "gdrive"
	Dropbox = "dropbox"
)

// GoogleScope is the one Google permission requested: the files this OAuth
// client creates, never the rest of the account's Drive.
const GoogleScope = "https://www.googleapis.com/auth/drive.file"

// DropboxScopes are requested from an App folder app; its console must enable
// each of them.
var DropboxScopes = []string{"account_info.read", "files.metadata.read", "files.metadata.write", "files.content.read", "files.content.write"}

// Kinds lists the destination kinds in the order people see them.
var Kinds = []string{Google, Dropbox}

// ErrRevoked is a provider refusing the grant itself: the account owner
// removed the app's access, or the grant expired. Only a new connection helps.
var ErrRevoked = errors.New("the provider no longer accepts this authorization; reconnect")

// ErrUnavailable is a provider request that failed for now.
var ErrUnavailable = errors.New("backup provider unavailable; try again later")

// MinimumSealKey is the least key material a seal key file may hold.
const MinimumSealKey = 32

type Config struct {
	SealKeyFile string         `json:"sealKeyFile"`
	Google      *GoogleConfig  `json:"google,omitempty"`
	Dropbox     *DropboxConfig `json:"dropbox,omitempty"`
}
type GoogleConfig struct {
	ClientID         string `json:"clientId"`
	ClientSecretFile string `json:"clientSecretFile"`
}
type DropboxConfig struct {
	AppKey        string `json:"appKey"`
	AppSecretFile string `json:"appSecretFile"`
}

// SecretFiles are the files New reads, for the caller's placement checks.
func (c Config) SecretFiles() []string {
	files := []string{c.SealKeyFile}
	if c.Google != nil {
		files = append(files, c.Google.ClientSecretFile)
	}
	if c.Dropbox != nil {
		files = append(files, c.Dropbox.AppSecretFile)
	}
	return files
}

// provider is one OAuth client and its endpoints. The endpoints are the
// providers' own; tests reach fakes through Client.HTTP's transport.
type provider struct {
	clientID, secret                  string
	authorize, token, account, revoke string
}

type Client struct {
	// HTTP makes every provider request.
	HTTP      *http.Client
	providers map[string]*provider
	aead      cipher.AEAD
}

// New reads the seal key and the client secrets. It refuses a seal key with
// less than MinimumSealKey bytes of key material and a configuration with no
// provider.
func New(c Config) (*Client, error) {
	if c.Google == nil && c.Dropbox == nil {
		return nil, errors.New("drive backups need a Google or Dropbox client")
	}
	material, err := readSealKey(c.SealKeyFile)
	if err != nil {
		return nil, err
	}
	key, err := hkdf.Key(sha256.New, material, nil, "forgetmenote drive backup seal v1", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &Client{providers: map[string]*provider{}, aead: aead, HTTP: &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if g := c.Google; g != nil {
		secret, err := readSecret(g.ClientSecretFile)
		if err != nil || g.ClientID == "" || strings.ContainsAny(g.ClientID, "\r\n\x00") {
			return nil, errors.Join(errors.New("Google Drive backups need clientId and clientSecretFile"), err)
		}
		client.providers[Google] = &provider{clientID: g.ClientID, secret: secret,
			authorize: "https://accounts.google.com/o/oauth2/v2/auth", token: "https://oauth2.googleapis.com/token",
			account: "https://www.googleapis.com/drive/v3/about?fields=user(emailAddress)", revoke: "https://oauth2.googleapis.com/revoke"}
	}
	if d := c.Dropbox; d != nil {
		secret, err := readSecret(d.AppSecretFile)
		if err != nil || d.AppKey == "" || strings.ContainsAny(d.AppKey, "\r\n\x00") {
			return nil, errors.Join(errors.New("Dropbox backups need appKey and appSecretFile"), err)
		}
		client.providers[Dropbox] = &provider{clientID: d.AppKey, secret: secret,
			authorize: "https://www.dropbox.com/oauth2/authorize", token: "https://api.dropboxapi.com/oauth2/token",
			account: "https://api.dropboxapi.com/2/users/get_current_account", revoke: "https://api.dropboxapi.com/2/auth/token/revoke"}
	}
	return client, nil
}

// readSealKey accepts raw random bytes or their hex or base64 text, as
// `openssl rand 32`, `openssl rand -hex 32` and `openssl rand -base64 32`
// write them, and returns the key material they encode.
func readSealKey(file string) ([]byte, error) {
	raw, err := readFile(file, 4096)
	if err != nil {
		return nil, errors.New("cannot read the drive backup seal key")
	}
	material := raw
	if text := strings.TrimSpace(string(raw)); text != "" {
		if decoded, err := hex.DecodeString(text); err == nil {
			material = decoded
		} else {
			for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
				if decoded, err := encoding.DecodeString(text); err == nil {
					material = decoded
					break
				}
			}
		}
	}
	if len(material) < MinimumSealKey {
		return nil, fmt.Errorf("the drive backup seal key needs at least %d random bytes", MinimumSealKey)
	}
	return material, nil
}

func readSecret(file string) (string, error) {
	raw, err := readFile(file, 4096)
	if err != nil {
		return "", errors.New("cannot read a drive backup client secret")
	}
	secret := strings.TrimSpace(string(raw))
	if secret == "" || strings.ContainsAny(secret, "\r\n\x00") {
		return "", errors.New("invalid drive backup client secret")
	}
	return secret, nil
}

func readFile(file string, max int64) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(raw)) > max || len(raw) == 0 {
		return nil, errors.New("unreadable file")
	}
	return raw, nil
}

// Configured reports whether the operator configured kind.
func (c *Client) Configured(kind string) bool { return c != nil && c.providers[kind] != nil }

// Configured lists the configured kinds in Kinds order.
func (c *Client) Kinds() []string {
	out := []string{}
	for _, kind := range Kinds {
		if c.Configured(kind) {
			out = append(out, kind)
		}
	}
	return out
}

func (c *Client) provider(kind string) (*provider, error) {
	if !c.Configured(kind) {
		return nil, fmt.Errorf("%s backups are not configured on this server", Name(kind))
	}
	return c.providers[kind], nil
}

// Name is the provider's name for people.
func Name(kind string) string {
	switch kind {
	case Google:
		return "Google Drive"
	case Dropbox:
		return "Dropbox"
	}
	return "Drive"
}

// AuthorizeURL is where the browser asks the provider for an offline grant
// with PKCE: the code challenge is derived from verifier, which stays with the
// server until the callback exchanges the code.
func (c *Client) AuthorizeURL(kind, redirect, state, verifier string) (string, error) {
	p, err := c.provider(kind)
	if err != nil {
		return "", err
	}
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {p.clientID}, "redirect_uri": {redirect}, "response_type": {"code"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}}
	if kind == Google {
		q.Set("scope", GoogleScope)
		q.Set("access_type", "offline")
		q.Set("prompt", "consent")
	} else {
		q.Set("scope", strings.Join(DropboxScopes, " "))
		q.Set("token_access_type", "offline")
	}
	return p.authorize + "?" + q.Encode(), nil
}

// A Grant is a new authorization: its refresh token and the account it
// belongs to. Account is the account's email address; AccountID identifies
// the account across email changes where the provider says so.
type Grant struct {
	Refresh, Account, AccountID string
}

// Access is a short-lived access token. Refresh is set when the provider
// rotated the refresh token.
type Access struct {
	Token   string
	Expires time.Time
	Refresh string
}

type tokenResponse struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	ExpiresIn    float64 `json:"expires_in"`
	Scope        string  `json:"scope"`
	AccountID    string  `json:"account_id"`
	Error        string  `json:"error"`
}

// Exchange trades an authorization code and its PKCE verifier for a grant,
// refusing one without offline access or without the destination scopes, and
// reads the grant's account.
func (c *Client) Exchange(ctx context.Context, kind, code, verifier, redirect string) (Grant, error) {
	p, err := c.provider(kind)
	if err != nil {
		return Grant{}, err
	}
	token, err := c.token(ctx, p, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {redirect}})
	if err != nil {
		return Grant{}, err
	}
	if token.RefreshToken == "" || strings.ContainsAny(token.RefreshToken, "\r\n\x00") {
		return Grant{}, fmt.Errorf("%s did not grant offline access; remove ForgetMeNote's access in the account and connect again", Name(kind))
	}
	granted := strings.Fields(token.Scope)
	grant := Grant{Refresh: token.RefreshToken}
	if kind == Google {
		if !slices.Contains(granted, GoogleScope) {
			return Grant{}, errors.New("Google Drive access to the files ForgetMeNote creates was not granted; connect again and allow it")
		}
		var about struct {
			User struct {
				Email string `json:"emailAddress"`
			} `json:"user"`
		}
		if err = c.call(ctx, "GET", p.account, token.AccessToken, &about); err != nil {
			return Grant{}, err
		}
		grant.Account = strings.ToLower(about.User.Email)
		grant.AccountID = grant.Account
	} else {
		for _, scope := range DropboxScopes {
			if token.Scope != "" && !slices.Contains(granted, scope) {
				return Grant{}, errors.New("Dropbox did not grant every permission the backup needs; check the app's permissions and connect again")
			}
		}
		var account struct {
			ID    string `json:"account_id"`
			Email string `json:"email"`
		}
		if err = c.call(ctx, "POST", p.account, token.AccessToken, &account); err != nil {
			return Grant{}, err
		}
		if account.ID == "" || token.AccountID != "" && token.AccountID != account.ID {
			return Grant{}, errors.New("Dropbox returned a different account than it authorized")
		}
		grant.Account, grant.AccountID = strings.ToLower(account.Email), account.ID
	}
	if address, err := mail.ParseAddress(grant.Account); err != nil || address.Address != grant.Account || len(grant.Account) > 320 || len(grant.AccountID) > 320 {
		return Grant{}, fmt.Errorf("%s returned an invalid account", Name(kind))
	}
	return grant, nil
}

// Refresh mints a new access token from a refresh token.
func (c *Client) Refresh(ctx context.Context, kind, refresh string) (Access, error) {
	p, err := c.provider(kind)
	if err != nil {
		return Access{}, err
	}
	started := time.Now()
	token, err := c.token(ctx, p, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	if err != nil {
		return Access{}, err
	}
	if token.ExpiresIn <= 0 || token.ExpiresIn > 7*86400 {
		return Access{}, fmt.Errorf("%s omitted the access token's lifetime", Name(kind))
	}
	access := Access{Token: token.AccessToken, Expires: started.Add(time.Duration(token.ExpiresIn) * time.Second)}
	if token.RefreshToken != refresh && token.RefreshToken != "" && !strings.ContainsAny(token.RefreshToken, "\r\n\x00") {
		access.Refresh = token.RefreshToken
	}
	return access, nil
}

// Revoke ends a grant at the provider. Google ends the whole grant of this
// client for that account, so a caller keeps a grant another binding still
// uses. Dropbox revokes through an access token, which ends the refresh token
// it came from and every access token minted from it. A grant the provider
// already refuses counts as revoked.
func (c *Client) Revoke(ctx context.Context, kind, refresh string) error {
	p, err := c.provider(kind)
	if err != nil {
		return err
	}
	if kind == Google {
		req, err := http.NewRequestWithContext(ctx, "POST", p.revoke, strings.NewReader(url.Values{"token": {refresh}}.Encode()))
		if err != nil {
			return ErrUnavailable
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		var out tokenResponse
		err = c.do(req, &out)
		if refused, ok := errors.AsType[*refusal](err); ok && (refused.code == "invalid_token" || refused.code == "invalid_grant") {
			return nil
		}
		return err
	}
	access, err := c.Refresh(ctx, kind, refresh)
	if errors.Is(err, ErrRevoked) {
		return nil
	}
	if err != nil {
		return err
	}
	return c.call(ctx, "POST", p.revoke, access.Token, nil)
}

func (c *Client) token(ctx context.Context, p *provider, form url.Values) (tokenResponse, error) {
	form.Set("client_id", p.clientID)
	form.Set("client_secret", p.secret)
	req, err := http.NewRequestWithContext(ctx, "POST", p.token, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out tokenResponse
	err = c.do(req, &out)
	if refused, ok := errors.AsType[*refusal](err); ok && (refused.code == "invalid_grant" || refused.code == "invalid_client" || refused.code == "unauthorized_client") {
		return out, ErrRevoked
	}
	if err != nil {
		return out, err
	}
	if out.AccessToken == "" || strings.ContainsAny(out.AccessToken, "\r\n\x00") {
		return out, errors.New("the provider returned an invalid access token")
	}
	return out, nil
}

// call makes an authorized API request; a nil out discards the answer.
func (c *Client) call(ctx context.Context, method, address, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, address, nil)
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if out == nil {
		var discard json.RawMessage
		return c.do(req, &discard)
	}
	return c.do(req, out)
}

// refusal is a provider's error answer, with the OAuth error code it named.
// Its message carries no response text, which could echo a credential.
type refusal struct {
	status int
	code   string
}

func (e *refusal) Error() string {
	return fmt.Sprintf("backup provider refused the request (HTTP %d)", e.status)
}

func (c *Client) do(req *http.Request, out any) error {
	response, err := c.HTTP.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 64<<10+1))
	if err != nil || len(raw) > 64<<10 {
		return ErrUnavailable
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
		return ErrUnavailable
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var answer struct {
			Error any `json:"error"`
		}
		json.Unmarshal(raw, &answer)
		code, _ := answer.Error.(string)
		// Dropbox names an expired or revoked token in a structured error.
		if summary, ok := answer.Error.(map[string]any); ok {
			code, _ = summary[".tag"].(string)
			if code == "invalid_access_token" || code == "expired_access_token" {
				code = "invalid_token"
			}
		}
		return &refusal{status: response.StatusCode, code: code}
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("invalid backup provider response")
	}
	return nil
}

const sealContext = "forgetmenote drive backup\x00"

// Seal encrypts a secret for shared state under the seal key, bound to its
// purpose, such as the binding it belongs to, so a sealed value moved to
// another purpose does not open.
func (c *Client) Seal(purpose, plain string) (string, error) {
	if c == nil || c.aead == nil {
		return "", errors.New("drive backups are not configured")
	}
	nonce := make([]byte, c.aead.NonceSize())
	rand.Read(nonce)
	return base64.RawURLEncoding.EncodeToString(c.aead.Seal(nonce, nonce, []byte(plain), []byte(sealContext+purpose))), nil
}

// Open returns what Seal sealed for the same purpose under the same key.
func (c *Client) Open(purpose, sealed string) (string, error) {
	if c == nil || c.aead == nil {
		return "", errors.New("drive backups are not configured")
	}
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil || len(raw) < c.aead.NonceSize() {
		return "", errors.New("invalid sealed drive secret")
	}
	plain, err := c.aead.Open(nil, raw[:c.aead.NonceSize()], raw[c.aead.NonceSize():], []byte(sealContext+purpose))
	if err != nil {
		return "", errors.New("invalid sealed drive secret")
	}
	return string(plain), nil
}

// Password returns a new random encryption password: 32 random bytes,
// base64url.
func Password() string {
	var key [32]byte
	rand.Read(key[:])
	return base64.RawURLEncoding.EncodeToString(key[:])
}
