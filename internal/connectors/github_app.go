package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/githubapp"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// GitHubApp signs in as one installation of a GitHub App. Each installation
// token lasts at most an hour and is limited to the configured repositories and
// permissions; the App key never leaves this process.
type GitHubApp struct {
	AppID          string            `json:"appId"`
	InstallationID int64             `json:"installationId"`
	PrivateKeyFile string            `json:"privateKeyFile"`
	Repositories   []string          `json:"repositories,omitempty"`
	Permissions    map[string]string `json:"permissions,omitempty"`
}

var githubAppID = regexp.MustCompile(`^[0-9]{1,20}$`)
var githubRepository = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
var githubPermission = regexp.MustCompile(`^[a-z_]{1,64}$`)

func (g *GitHubApp) validate() error {
	if !githubAppID.MatchString(g.AppID) || g.InstallationID <= 0 || !filepath.IsAbs(g.PrivateKeyFile) || len(g.Repositories) > 100 || len(g.Permissions) > 32 {
		return errors.New("GitHub App connections require a numeric appId, installationId and an absolute privateKeyFile")
	}
	for i, name := range g.Repositories {
		if !githubRepository.MatchString(name) || slices.Contains(g.Repositories[:i], name) {
			return errors.New("GitHub App repositories are unique names within the installation's account")
		}
	}
	for name, level := range g.Permissions {
		if !githubPermission.MatchString(name) || level != "read" && level != "write" {
			return errors.New("GitHub App permissions are read or write")
		}
	}
	return nil
}

func (r *runtime) authorizeGitHubApp(ctx context.Context, req *http.Request) ([]string, error) {
	g := r.config.GitHubApp
	pemKey, err := githubapp.ReadPrivateKey(g.PrivateKeyFile)
	if err != nil {
		return nil, errors.New("GitHub App key unavailable")
	}
	r.loginMu.Lock()
	defer r.loginMu.Unlock()
	// A replaced key file takes effect on the next call.
	fingerprint := hash([]string{g.AppID, string(pemKey)})
	if r.loginFingerprint != fingerprint || !time.Now().Before(r.loginUntil) {
		if err := checkAuthority(ctx); err != nil {
			return nil, err
		}
		token, expires, err := r.installationToken(ctx, g, pemKey)
		if err != nil {
			return nil, err
		}
		r.loginFingerprint, r.loginToken, r.loginUntil = fingerprint, token, expires.Add(-time.Minute)
	}
	req.Header.Set("Authorization", "Bearer "+r.loginToken)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return []string{r.loginToken}, nil
}

func (r *runtime) installationToken(ctx context.Context, g *GitHubApp, pemKey []byte) (string, time.Time, error) {
	failed := errors.New("GitHub App installation token exchange failed; check the installation, its repositories and permissions")
	key, err := githubapp.ParsePrivateKey(pemKey)
	if err != nil {
		return "", time.Time{}, err
	}
	jwt, err := githubapp.AppJWT(g.AppID, key)
	if err != nil {
		return "", time.Time{}, failed
	}
	scope := map[string]any{}
	if len(g.Repositories) > 0 {
		scope["repositories"] = g.Repositories
	}
	if len(g.Permissions) > 0 {
		scope["permissions"] = g.Permissions
	}
	body, _ := json.Marshal(scope)
	address := fmt.Sprintf("%s/app/installations/%d/access_tokens", strings.TrimRight(r.config.Endpoint, "/"), g.InstallationID)
	exchange, err := http.NewRequestWithContext(ctx, "POST", address, bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, failed
	}
	exchange.Header.Set("Accept", "application/vnd.github+json")
	exchange.Header.Set("Content-Type", "application/json")
	exchange.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	exchange.Header.Set("Authorization", "Bearer "+jwt)
	resp, err := r.client.Do(exchange)
	if err != nil {
		if ctx.Err() != nil {
			return "", time.Time{}, ctx.Err()
		}
		return "", time.Time{}, ErrUnavailable
	}
	defer resp.Body.Close()
	var out struct {
		Token   string    `json:"token"`
		Expires time.Time `json:"expires_at"`
	}
	raw, err := readBounded(resp.Body, 64<<10)
	if err != nil || resp.StatusCode != 201 || json.Unmarshal(raw, &out) != nil {
		return "", time.Time{}, failed
	}
	if out.Token == "" || len(out.Token) > 32<<10 || strings.ContainsAny(out.Token, "\r\n\x00 ") || time.Until(out.Expires) < 2*time.Minute {
		return "", time.Time{}, errors.New("invalid GitHub App installation token")
	}
	return out.Token, out.Expires, nil
}
