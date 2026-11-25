package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var ErrUnavailable = errors.New("GitHub integration unavailable")
var ErrDenied = errors.New("GitHub access denied")
var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
var revision = regexp.MustCompile(`^[a-f0-9]{40}$`)
var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

type Config struct {
	AppID             string `json:"appId"`
	Slug              string `json:"slug"`
	ClientID          string `json:"clientId"`
	ClientSecretFile  string `json:"clientSecretFile"`
	PrivateKeyFile    string `json:"privateKeyFile"`
	WebhookSecretFile string `json:"webhookSecretFile"`
}

func (c Config) Validate() error {
	if c.AppID == "" || !identifier.MatchString(c.Slug) || c.ClientID == "" || c.ClientSecretFile == "" || c.PrivateKeyFile == "" || c.WebhookSecretFile == "" {
		return errors.New("GitHub App requires appId, slug, clientId and mounted secret files")
	}
	return nil
}
func secret(file string, max int64) ([]byte, error) {
	f, e := os.Open(file)
	if e != nil {
		return nil, errors.New("cannot read GitHub App secret")
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		return nil, errors.New("invalid GitHub App secret")
	}
	b, e := io.ReadAll(io.LimitReader(f, max+1))
	if e != nil || len(b) == 0 || int64(len(b)) > max {
		return nil, errors.New("invalid GitHub App secret")
	}
	return b, nil
}

type RateLimitError struct{ RetryAt time.Time }

func (e *RateLimitError) Error() string {
	return "GitHub rate limit reached; refresh will retry after the cooldown"
}
func (e *RateLimitError) Unwrap() error { return ErrUnavailable }

type Repository struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Size          int64  `json:"size"`
	Permissions   struct {
		Push  bool `json:"push"`
		Admin bool `json:"admin"`
	} `json:"permissions"`
	Private     bool   `json:"private"`
	Description string `json:"description"`
}
type Choice struct {
	Repository
	Installation int64 `json:"installation"`
}
type Client struct {
	Config                      Config
	key                         *rsa.PrivateKey
	clientSecret, webhookSecret string
	HTTP                        *http.Client
	contents                    string
	API, TokenURL               string
}

func New(c Config, contents string) (*Client, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	b, err := ReadPrivateKey(c.PrivateKeyFile)
	if err != nil {
		return nil, err
	}
	key, err := ParsePrivateKey(b)
	if err != nil {
		return nil, err
	}
	s, err := secret(c.ClientSecretFile, 4096)
	if err != nil {
		return nil, err
	}
	w, err := secret(c.WebhookSecretFile, 4096)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if contents != "read" && contents != "write" {
		return nil, errors.New("invalid GitHub App content permission")
	}
	g := &Client{Config: c, key: key, clientSecret: strings.TrimSpace(string(s)), webhookSecret: strings.TrimSpace(string(w)), contents: contents, API: "https://api.github.com", TokenURL: "https://github.com/login/oauth/access_token", HTTP: &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if len(g.webhookSecret) < 32 || g.clientSecret == "" || strings.ContainsAny(g.clientSecret+g.webhookSecret, "\r\n\x00") {
		return nil, errors.New("invalid repository app client/webhook secret")
	}
	return g, nil
}

// ReadPrivateKey reads a mounted App key without interpreting it.
func ReadPrivateKey(file string) ([]byte, error) { return secret(file, 32768) }

// ParsePrivateKey accepts the PEM key GitHub issues for an App.
func ParsePrivateKey(b []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("GitHub App needs a PEM RSA key")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		value, e := x509.ParsePKCS8PrivateKey(block.Bytes)
		if e != nil {
			return nil, errors.New("invalid GitHub App RSA key")
		}
		key, _ = value.(*rsa.PrivateKey)
	}
	if key == nil || key.N.BitLen() < 2048 {
		return nil, errors.New("GitHub App needs an RSA key of at least 2048 bits")
	}
	return key, nil
}

// AppJWT signs the short-lived token that authenticates as the App itself.
func AppJWT(appID string, key *rsa.PrivateKey) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	body, _ := json.Marshal(map[string]any{"iat": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(9 * time.Minute).Unix(), "iss": appID})
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(body)
	h := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), err
}

func (g *Client) jwt() (string, error) { return AppJWT(g.Config.AppID, g.key) }

func (g *Client) Request(ctx context.Context, method, address, token string, body any, dest any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return ErrUnavailable
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, address, reader)
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := g.HTTP.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode == 429 || response.StatusCode == 403 && (response.Header.Get("Retry-After") != "" || response.Header.Get("X-RateLimit-Remaining") == "0") {
		at := time.Now().Add(time.Minute)
		if seconds, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && seconds > 0 {
			at = time.Now().Add(time.Duration(min(seconds, 86400)) * time.Second)
		} else if reset, err := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && time.Unix(reset, 0).After(at) {
			at = time.Unix(reset, 0)
		}
		return &RateLimitError{RetryAt: at}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &HTTPError{Status: response.StatusCode}
	}
	if dest == nil {
		_, err := io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return err
	}
	limited := &io.LimitedReader{R: response.Body, N: 2<<20 + 1}
	decoder := json.NewDecoder(limited)
	if decoder.Decode(dest) != nil || decoder.Decode(new(any)) != io.EOF || limited.N == 0 {
		return ErrUnavailable
	}
	return nil
}

func (g *Client) Exchange(ctx context.Context, code, verifier, callback string) (string, error) {
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	err := g.Request(ctx, "POST", g.TokenURL, "", map[string]string{"client_id": g.Config.ClientID, "client_secret": g.clientSecret, "code": code, "code_verifier": verifier, "redirect_uri": callback}, &out)
	if err != nil || out.AccessToken == "" || strings.ContainsAny(out.AccessToken, "\r\n\x00") || !strings.EqualFold(out.TokenType, "bearer") {
		return "", ErrDenied
	}
	return out.AccessToken, nil
}

func (g *Client) Choices(ctx context.Context, token string) ([]Choice, error) {
	var installations struct {
		Total         int `json:"total_count"`
		Installations []struct {
			ID int64 `json:"id"`
		} `json:"installations"`
	}
	if err := g.Request(ctx, "GET", g.API+"/user/installations?per_page=100", token, nil, &installations); err != nil {
		return nil, err
	}
	if installations.Total > 100 {
		return nil, errors.New("too many GitHub installations; narrow app access")
	}
	out := []Choice{}
	for _, installation := range installations.Installations {
		for page := 1; page <= 5; page++ {
			var repos struct {
				Total        int          `json:"total_count"`
				Repositories []Repository `json:"repositories"`
			}
			address := fmt.Sprintf("%s/user/installations/%d/repositories?per_page=100&page=%d", g.API, installation.ID, page)
			if err := g.Request(ctx, "GET", address, token, nil, &repos); err != nil {
				return nil, err
			}
			if repos.Total > 500 || len(out)+len(repos.Repositories) > 500 {
				return nil, errors.New("select fewer repositories in the GitHub App installation (maximum 500)")
			}
			for _, repo := range repos.Repositories {
				if ValidRepo(repo) {
					out = append(out, Choice{repo, installation.ID})
				}
			}
			if len(repos.Repositories) < 100 {
				break
			}
		}
	}
	return out, nil
}

func ValidRepo(r Repository) bool {
	return r.ID > 0 && repoName.MatchString(r.FullName) && !strings.Contains(r.FullName, "..") && r.DefaultBranch != "" && len(r.DefaultBranch) <= 255 && !strings.ContainsAny(r.DefaultBranch, "\r\n\x00")
}

func (g *Client) Token(ctx context.Context, installation, repo int64) (string, error) {
	if installation <= 0 || repo <= 0 {
		return "", ErrDenied
	}
	jwt, err := g.jwt()
	if err != nil {
		return "", ErrUnavailable
	}
	var out struct {
		Token   string    `json:"token"`
		Expires time.Time `json:"expires_at"`
	}
	err = g.Request(ctx, "POST", fmt.Sprintf("%s/app/installations/%d/access_tokens", g.API, installation), jwt, map[string]any{"repository_ids": []int64{repo}, "permissions": map[string]string{"contents": g.contents, "metadata": "read"}}, &out)
	if err != nil {
		return "", err
	}
	if out.Token == "" || !out.Expires.After(time.Now()) || strings.ContainsAny(out.Token, "\r\n\x00") {
		return "", ErrUnavailable
	}
	return out.Token, nil
}

func (g *Client) Resolve(ctx context.Context, installation, id int64) (Repository, string, string, error) {
	token, err := g.Token(ctx, installation, id)
	if err != nil {
		return Repository{}, "", "", err
	}
	var repo Repository
	if err = g.Request(ctx, "GET", fmt.Sprintf("%s/repositories/%d", g.API, id), token, nil, &repo); err != nil {
		return repo, "", "", err
	}
	if !ValidRepo(repo) || repo.ID != id {
		return repo, "", "", ErrDenied
	}
	var head struct {
		SHA string `json:"sha"`
	}
	err = g.Request(ctx, "GET", g.API+"/repos/"+repo.FullName+"/commits/"+url.PathEscape(repo.DefaultBranch), token, nil, &head)
	if err != nil {
		return repo, "", "", err
	}
	if !revision.MatchString(head.SHA) {
		return repo, "", "", ErrUnavailable
	}
	return repo, head.SHA, token, nil
}

func (g *Client) VerifyWebhook(body []byte, signature string) bool {
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return false
	}
	h := hmac.New(sha256.New, []byte(g.webhookSecret))
	h.Write(body)
	return hmac.Equal(got, h.Sum(nil))
}

type HTTPError struct{ Status int }

func (e *HTTPError) Error() string { return fmt.Sprintf("GitHub returned HTTP %d", e.Status) }
func (e *HTTPError) Unwrap() error {
	if e.Status == 401 || e.Status == 403 || e.Status == 404 {
		return ErrDenied
	}
	return ErrUnavailable
}
func IsStatus(err error, status int) bool {
	var e *HTTPError
	return errors.As(err, &e) && e.Status == status
}
