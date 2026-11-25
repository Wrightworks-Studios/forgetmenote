package connectors

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPRefreshTokenIsPrivateAndCached(t *testing.T) {
	var exchanges atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			exchanges.Add(1)
			r.ParseForm()
			if r.Form.Get("refresh_token") != "fixture-refresh" || r.Form.Get("client_secret") != "fixture-secret" {
				t.Error("wrong OAuth refresh fields")
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"fixture-access","token_type":"Bearer","expires_in":3600}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-access" {
			t.Error("missing refreshed bearer")
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	dir := t.TempDir()
	secret := filepath.Join(dir, "client")
	refresh := filepath.Join(dir, "refresh")
	os.WriteFile(secret, []byte("fixture-secret"), 0600)
	os.WriteFile(refresh, []byte("fixture-refresh"), 0600)
	config := map[string]any{"version": 1, "connections": []any{map[string]any{"id": "drive", "kind": "http", "endpoint": upstream.URL, "oauthRefresh": map[string]any{"tokenURL": upstream.URL + "/token", "clientID": "fixture-client", "clientSecretFile": secret, "refreshTokenFile": refresh}, "operations": []any{map[string]any{"name": "list", "readOnly": true, "path": "/files"}}}}}
	raw, _ := json.Marshal(config)
	file := filepath.Join(dir, "config.json")
	os.WriteFile(file, raw, 0600)
	manager, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = manager.Call(t.Context(), "drive", "list", json.RawMessage(`{}`), ""); err != nil {
			t.Fatal(err)
		}
	}
	if exchanges.Load() != 1 {
		t.Fatal("OAuth token was not reused")
	}
	if manager.LearningInputSafe("drive", "fixture-refresh") {
		t.Fatal("refresh credential eligible for evidence capture")
	}
}

func TestGoogleRefreshCredentialChangeAndRevocation(t *testing.T) {
	var exchanges atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			exchanges.Add(1)
			r.ParseForm()
			switch r.Form.Get("refresh_token") {
			case "revoked-secret":
				w.WriteHeader(400)
				w.Write([]byte(`{"error":"invalid_grant","error_description":"revoked-secret"}`))
			case "other-account":
				w.Write([]byte(`{"access_token":"other","token_type":"Bearer","expires_in":3600}`))
			default:
				w.Write([]byte(`{"access_token":"expected","token_type":"Bearer","expires_in":3600,"scope":"https://www.googleapis.com/auth/gmail.readonly"}`))
			}
			return
		}
		email := "finance@example.test"
		if r.Header.Get("Authorization") == "Bearer other" {
			email = "other@example.test"
		}
		json.NewEncoder(w).Encode(map[string]string{"emailAddress": email, "historyId": "1"})
	}))
	defer upstream.Close()
	t.Setenv("REFRESH_TEST_SECRET", "synthetic")
	t.Setenv("REFRESH_TEST_TOKEN", "expected-account")
	var cfg Config
	raw := `{"version":1,"connections":[{"id":"gmail","kind":"email","endpoint":"` + upstream.URL + `","oauthRefresh":{"tokenURL":"` + upstream.URL + `/token","clientID":"fixture","clientSecretEnv":"REFRESH_TEST_SECRET","refreshTokenEnv":"REFRESH_TEST_TOKEN"},"email":{"provider":"gmail","mailbox":"finance@example.test","workflow":"test"},"operations":[{"name":"status","readOnly":true}]}]}`
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	call := func() error { _, err := m.Call(t.Context(), "gmail", "status", json.RawMessage(`{}`), ""); return err }
	for range 2 {
		if err = call(); err != nil {
			t.Fatal(err)
		}
	}
	if exchanges.Load() != 1 {
		t.Fatal("refresh not cached")
	}
	t.Setenv("REFRESH_TEST_TOKEN", "other-account")
	if err = call(); err == nil || !strings.Contains(err.Error(), "account mismatch") {
		t.Fatal(err)
	}
	t.Setenv("REFRESH_TEST_TOKEN", "revoked-secret")
	if err = call(); err == nil || !strings.Contains(err.Error(), "reauthorize") || strings.Contains(err.Error(), "revoked-secret") {
		t.Fatal(err)
	}
	if exchanges.Load() != 3 {
		t.Fatal("refresh retried or rotation not checked", exchanges.Load())
	}
}
