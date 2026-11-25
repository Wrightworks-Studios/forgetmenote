package connectors

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// OAuthClientCredentials owns an application token, without a user account or
// refresh credential. Its fixed endpoint and credential references are operator policy.
type OAuthClientCredentials struct {
	TokenURL         string `json:"tokenURL"`
	ClientIDFile     string `json:"clientIDFile,omitempty"`
	ClientIDEnv      string `json:"clientIDEnv,omitempty"`
	ClientSecretFile string `json:"clientSecretFile,omitempty"`
	ClientSecretEnv  string `json:"clientSecretEnv,omitempty"`
}

func (c *OAuthClientCredentials) validate() error {
	u, err := url.Parse(c.TokenURL)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" || u.Scheme != "https" && (u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost")) || !credentialReference(c.ClientIDFile, c.ClientIDEnv) || !credentialReference(c.ClientSecretFile, c.ClientSecretEnv) {
		return errors.New("invalid runtime OAuth client credentials configuration")
	}
	return nil
}

func (r *runtime) authorizeClientCredentials(ctx context.Context, req *http.Request) ([]string, error) {
	c := r.config.OAuthClientCredentials
	id, err := readCredential(c.ClientIDFile, c.ClientIDEnv)
	if err != nil {
		return nil, err
	}
	secret, err := readCredential(c.ClientSecretFile, c.ClientSecretEnv)
	if err != nil {
		return nil, err
	}
	sensitive := []string{id, secret, base64.StdEncoding.EncodeToString([]byte(id + ":" + secret))}
	r.loginMu.Lock()
	defer r.loginMu.Unlock()
	fingerprint := hash([]string{id, secret})
	if r.loginFingerprint != fingerprint || !time.Now().Before(r.loginUntil) {
		if err := checkAuthority(ctx); err != nil {
			return sensitive, err
		}
		conf := clientcredentials.Config{ClientID: id, ClientSecret: secret, TokenURL: c.TokenURL, AuthStyle: oauth2.AuthStyleInHeader}
		ctx = context.WithValue(ctx, oauth2.HTTPClient, r.client)
		token, err := conf.Token(ctx)
		if err != nil {
			return sensitive, errors.New("runtime OAuth client credentials exchange failed; check operator credentials and provider access")
		}
		if token.AccessToken == "" || len(token.AccessToken) > 32<<10 || strings.ContainsAny(token.AccessToken, "\r\n") || token.Expiry.IsZero() || time.Until(token.Expiry) < 30*time.Second || !strings.EqualFold(token.TokenType, "Bearer") {
			return sensitive, errors.New("invalid application access token")
		}
		r.loginFingerprint, r.loginToken, r.loginUntil = fingerprint, token.AccessToken, token.Expiry.Add(-30*time.Second)
	}
	req.Header.Set("Authorization", "Bearer "+r.loginToken)
	return append(sensitive, r.loginToken), nil
}
