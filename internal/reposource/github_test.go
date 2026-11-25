package reposource

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"forgetmenote/internal/githubapp"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestGitHubRateLimitRemainsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", "120")
		http.Error(w, "rate limited", 403)
	}))
	defer server.Close()
	g := &githubapp.Client{HTTP: server.Client()}
	var out any
	if err := g.Request(context.Background(), "GET", server.URL, "", nil, &out); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrDenied) {
		t.Fatalf("rate limiting should retry rather than revoke consent: %v", err)
	}
}

func testGitHub(t *testing.T, key *rsa.PrivateKey, server *httptest.Server, c Config) *GitHub {
	t.Helper()
	dir := t.TempDir()
	c.Slug = "fixture"
	c.ClientID = "fixture"
	c.PrivateKeyFile = filepath.Join(dir, "key")
	c.ClientSecretFile = filepath.Join(dir, "client")
	c.WebhookSecretFile = filepath.Join(dir, "webhook")
	for name, b := range map[string][]byte{c.PrivateKeyFile: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), c.ClientSecretFile: []byte("synthetic-client"), c.WebhookSecretFile: []byte("synthetic-webhook-secret-0123456789")} {
		if err := os.WriteFile(name, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	g, err := NewGitHub(c)
	if err != nil {
		t.Fatal(err)
	}
	g.Client.API = server.URL
	g.Client.HTTP = server.Client()
	return g
}
