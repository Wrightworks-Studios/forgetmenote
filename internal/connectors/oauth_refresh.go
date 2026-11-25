package connectors

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

type OAuthRefresh struct {
	TokenURL         string `json:"tokenURL"`
	ClientID         string `json:"clientID"`
	ClientSecretFile string `json:"clientSecretFile,omitempty"`
	ClientSecretEnv  string `json:"clientSecretEnv,omitempty"`
	RefreshTokenFile string `json:"refreshTokenFile,omitempty"`
	RefreshTokenEnv  string `json:"refreshTokenEnv,omitempty"`
}

func (c *OAuthRefresh) validate() error {
	u, err := url.Parse(c.TokenURL)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" || u.Scheme != "https" && (u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost")) || c.ClientID == "" || len(c.ClientID) > 512 || !credentialReference(c.ClientSecretFile, c.ClientSecretEnv) || !credentialReference(c.RefreshTokenFile, c.RefreshTokenEnv) {
		return errors.New("invalid runtime OAuth refresh configuration")
	}
	return nil
}

func (r *runtime) refreshedToken(ctx context.Context) (string, []string, error) {
	if r.config.personalToken != nil {
		return r.config.personalToken(ctx)
	}
	c := r.config.OAuthRefresh
	secret, err := readCredential(c.ClientSecretFile, c.ClientSecretEnv)
	if err != nil {
		return "", nil, err
	}
	refresh, err := readCredential(c.RefreshTokenFile, c.RefreshTokenEnv)
	if err != nil {
		return "", nil, err
	}
	r.loginMu.Lock()
	defer r.loginMu.Unlock()
	fingerprint := hash([]string{secret, refresh})
	if r.loginFingerprint == fingerprint && time.Now().Before(r.loginUntil) {
		return r.loginToken, []string{secret, refresh, r.loginToken}, nil
	}
	if err := checkAuthority(ctx); err != nil {
		return "", nil, err
	}
	conf := oauth2.Config{ClientID: c.ClientID, ClientSecret: secret, Endpoint: oauth2.Endpoint{TokenURL: c.TokenURL, AuthStyle: oauth2.AuthStyleInParams}}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, r.client)
	token, err := conf.TokenSource(ctx, &oauth2.Token{RefreshToken: refresh}).Token()
	if err != nil {
		var rejected *oauth2.RetrieveError
		if errors.As(err, &rejected) && (rejected.ErrorCode == "invalid_grant" || rejected.ErrorCode == "invalid_client") {
			return "", nil, errors.New("OAuth authorization expired or revoked; reauthorize the configured account and replace its runtime credential reference")
		}
		return "", nil, errors.New("runtime OAuth refresh failed; check token endpoint and operator credential references")
	}
	if token.AccessToken == "" || len(token.AccessToken) > 32<<10 || strings.ContainsAny(token.AccessToken, "\r\n") || token.Expiry.IsZero() || time.Until(token.Expiry) < 30*time.Second {
		return "", nil, errors.New("invalid refreshed access token")
	}
	if token.RefreshToken != "" && token.RefreshToken != refresh {
		return "", nil, errors.New("rotating refresh tokens require a credential manager; configured static refresh token is no longer sufficient")
	}
	r.loginFingerprint = fingerprint
	r.loginToken = token.AccessToken
	r.loginScopes, _ = token.Extra("scope").(string)
	r.loginUntil = token.Expiry.Add(-30 * time.Second)
	return token.AccessToken, []string{secret, refresh, token.AccessToken}, nil
}

func (r *runtime) authorizeRefresh(ctx context.Context, req *http.Request) ([]string, error) {
	token, secrets, err := r.refreshedToken(ctx)
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return secrets, err
}
