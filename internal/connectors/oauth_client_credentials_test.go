package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPClientCredentialsUsesPrivateReferencesAndCachesToken(t *testing.T) {
	var exchanges atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			exchanges.Add(1)
			user, secret, ok := r.BasicAuth()
			r.ParseForm()
			if !ok || user != "fixture-client" || secret != "fixture-secret" || r.Form.Get("grant_type") != "client_credentials" {
				t.Error("wrong client credentials exchange")
			}
			w.Write([]byte(`{"access_token":"fixture-access","token_type":"Bearer","expires_in":3600}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-access" {
			t.Error("missing bearer token")
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	dir := t.TempDir()
	id, secret := filepath.Join(dir, "client-id"), filepath.Join(dir, "client-secret")
	os.WriteFile(id, []byte("fixture-client"), 0600)
	os.WriteFile(secret, []byte("fixture-secret"), 0600)
	config := map[string]any{"version": 1, "connections": []any{map[string]any{
		"id": "music", "kind": "http", "endpoint": upstream.URL,
		"oauthClientCredentials": map[string]any{"tokenURL": upstream.URL + "/token", "clientIDFile": id, "clientSecretFile": secret},
		"operations":             []any{map[string]any{"name": "catalog", "readOnly": true, "path": "/catalog"}},
	}}}
	raw, _ := json.Marshal(config)
	file := filepath.Join(dir, "connections.json")
	os.WriteFile(file, raw, 0600)
	m, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for range 2 {
		if _, err := m.Call(t.Context(), "music", "catalog", json.RawMessage(`{}`), ""); err != nil {
			t.Fatal(err)
		}
	}
	if exchanges.Load() != 1 {
		t.Fatal("client token not cached", exchanges.Load())
	}
	for _, value := range []string{"fixture-client", "fixture-secret", "fixture-access"} {
		if m.LearningInputSafe("music", value) {
			t.Fatal("credential eligible for evidence capture")
		}
	}
}

func TestHTTPClientCredentialsRejectsUnsafeAndConflictingConfiguration(t *testing.T) {
	for _, auth := range []map[string]any{
		{"tokenURL": "http://example.org/token", "clientIDEnv": "CLIENT_ID", "clientSecretEnv": "CLIENT_SECRET"},
		{"tokenURL": "https://example.org/token?secret=value", "clientIDEnv": "CLIENT_ID", "clientSecretEnv": "CLIENT_SECRET"},
		{"tokenURL": "https://example.org/token", "clientIDEnv": "CLIENT_ID", "clientIDFile": "/private/id", "clientSecretEnv": "CLIENT_SECRET"},
		{"tokenURL": "https://example.org/token", "clientIDEnv": "CLIENT_ID"},
	} {
		raw, _ := json.Marshal(map[string]any{"version": 1, "connections": []any{map[string]any{"id": "music", "kind": "http", "endpoint": "https://example.org", "oauthClientCredentials": auth, "operations": []any{map[string]any{"name": "get", "readOnly": true, "path": "/"}}}}})
		var cfg Config
		json.Unmarshal(raw, &cfg)
		if _, err := New(cfg); err == nil {
			t.Fatal("invalid client credentials configuration accepted")
		}
	}
	auth := &OAuthClientCredentials{TokenURL: "https://example.org/token", ClientIDEnv: "CLIENT_ID", ClientSecretEnv: "CLIENT_SECRET"}
	for _, c := range []Connection{
		{Kind: "http", OAuthClientCredentials: auth, OAuthRefresh: &OAuthRefresh{}},
		{Kind: "http", OAuthClientCredentials: auth, SecretEnv: "OTHER"},
		{Kind: "mcp", OAuthClientCredentials: auth},
	} {
		c.AuthHeader, c.AuthScheme = "Authorization", "Bearer"
		if validateHTTPAuth(&c) == nil {
			t.Fatal("conflicting authentication accepted")
		}
	}
}

func TestHTTPClientCredentialsDenialDoesNotLeakCredentialsOrRetry(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid_client","error_description":"private-client private-secret"}`))
	}))
	defer upstream.Close()
	t.Setenv("CC_TEST_ID", "private-client")
	t.Setenv("CC_TEST_SECRET", "private-secret")
	m, err := New(Config{Version: 1, Connections: []Connection{{ID: "music", Kind: "http", Endpoint: upstream.URL,
		OAuthClientCredentials: &OAuthClientCredentials{TokenURL: upstream.URL + "/token", ClientIDEnv: "CC_TEST_ID", ClientSecretEnv: "CC_TEST_SECRET"},
		Operations:             []Operation{{Name: "catalog", Path: "/catalog", ReadOnly: true}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	_, err = m.Call(t.Context(), "music", "catalog", json.RawMessage(`{}`), "")
	if err == nil || strings.Contains(err.Error(), "private-client") || strings.Contains(err.Error(), "private-secret") || calls.Load() != 1 {
		t.Fatalf("token denial disclosed credentials or retried: calls=%d err=%v", calls.Load(), err)
	}
}

func TestCodeOnlyConnectionCannotPermitAIProcessingOrWrites(t *testing.T) {
	for _, c := range []Connection{
		{ID: "music", Kind: "http", Endpoint: "https://api.example.org", CodeOnly: true, ExternalProcessing: true, Operations: []Operation{{Name: "read", Path: "/", ReadOnly: true}}},
		{ID: "music", Kind: "http", Endpoint: "https://api.example.org", CodeOnly: true, Operations: []Operation{{Name: "write", Path: "/", ReadOnly: false}}},
	} {
		if _, err := New(Config{Version: 1, Connections: []Connection{c}}); err == nil {
			t.Fatal("unsafe code-only configuration accepted")
		}
	}
}

func TestHTTPClientCredentialsRotationExpiryDenialAndAuthority(t *testing.T) {
	var exchanges atomic.Int32
	var denied atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			exchanges.Add(1)
			_, secret, _ := r.BasicAuth()
			json.NewEncoder(w).Encode(map[string]any{"access_token": secret, "token_type": "Bearer", "expires_in": 3600})
			return
		}
		if denied.Swap(false) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"denied"}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	t.Setenv("CC_ROTATE_ID", "fixture")
	t.Setenv("CC_ROTATE_SECRET", "first")
	m, err := New(Config{Version: 1, Connections: []Connection{{ID: "music", Kind: "http", Endpoint: upstream.URL, OAuthClientCredentials: &OAuthClientCredentials{TokenURL: upstream.URL + "/token", ClientIDEnv: "CC_ROTATE_ID", ClientSecretEnv: "CC_ROTATE_SECRET"}, Operations: []Operation{{Name: "catalog", Path: "/catalog", ReadOnly: true}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	call := func() {
		t.Helper()
		if _, err := m.Call(t.Context(), "music", "catalog", json.RawMessage(`{}`), ""); err != nil {
			t.Fatal(err)
		}
	}
	call()
	t.Setenv("CC_ROTATE_SECRET", "second")
	call()
	if exchanges.Load() != 2 {
		t.Fatal("rotation reused prior token")
	}
	r := m.connections["music"]
	r.loginMu.Lock()
	r.loginUntil = time.Now().Add(-time.Second)
	r.loginMu.Unlock()
	call()
	if exchanges.Load() != 3 {
		t.Fatal("expired token reused")
	}
	denied.Store(true)
	if _, err := m.Call(t.Context(), "music", "catalog", json.RawMessage(`{}`), ""); err == nil {
		t.Fatal("401 accepted")
	}
	if exchanges.Load() != 3 {
		t.Fatal("401 was retried")
	}
	call()
	if exchanges.Load() != 4 {
		t.Fatal("401 did not invalidate token")
	}
	ctx := WithAuthority(t.Context(), func(context.Context) error { return errors.New("revoked") })
	if _, err := m.Call(ctx, "music", "catalog", json.RawMessage(`{}`), ""); err == nil {
		t.Fatal("revoked authority accepted")
	}
	if exchanges.Load() != 4 {
		t.Fatal("revoked authority called provider")
	}
}
