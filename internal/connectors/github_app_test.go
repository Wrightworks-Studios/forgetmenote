package connectors

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func githubAppFixture(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	return key, file
}

func verifyAppJWT(t *testing.T, header string, key *rsa.PublicKey, appID string) {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(header, "Bearer "), ".")
	if len(parts) != 3 {
		t.Fatal("app token is not a JWT")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], signature); err != nil {
		t.Fatal("app JWT not signed by the configured key")
	}
	var claims struct {
		Issuer string `json:"iss"`
		Expiry int64  `json:"exp"`
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if json.Unmarshal(raw, &claims) != nil || claims.Issuer != appID || time.Until(time.Unix(claims.Expiry, 0)) > 10*time.Minute {
		t.Fatalf("wrong app JWT claims %s", raw)
	}
}

var issueSchema = map[string]any{"type": "object", "additionalProperties": false, "required": []string{"owner", "repo", "number"}, "properties": map[string]any{
	"owner": map[string]any{"type": "string"}, "repo": map[string]any{"type": "string"}, "number": map[string]any{"type": "string"},
}}

func loadGitHubConnection(t *testing.T, endpoint string, app map[string]any) *Manager {
	t.Helper()
	config := map[string]any{"version": 1, "connections": []any{map[string]any{
		"id": "github", "kind": "http", "endpoint": endpoint, "githubApp": app,
		"operations": []any{
			map[string]any{"name": "get_issue", "readOnly": true, "path": "/repos/{owner}/{repo}/issues/{number}", "inputSchema": issueSchema},
		},
	}}}
	raw, _ := json.Marshal(config)
	file := filepath.Join(t.TempDir(), "connections.json")
	os.WriteFile(file, raw, 0600)
	m, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func TestHTTPGitHubAppUsesScopedInstallationTokens(t *testing.T) {
	key, keyFile := githubAppFixture(t)
	var exchanges atomic.Int32
	var reject atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/app/installations/77/access_tokens" {
			n := exchanges.Add(1)
			verifyAppJWT(t, r.Header.Get("Authorization"), &key.PublicKey, "1234")
			var body struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&body) != nil || strings.Join(body.Repositories, ",") != "example-service" || len(body.Permissions) != 2 || body.Permissions["issues"] != "write" || body.Permissions["metadata"] != "read" {
				t.Errorf("token request not scoped: %+v", body)
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"token": "ghs_fixture" + string(rune('0'+n)), "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
			return
		}
		if reject.Swap(false) {
			w.WriteHeader(401)
			return
		}
		if want := "Bearer ghs_fixture" + string(rune('0'+exchanges.Load())); r.Header.Get("Authorization") != want {
			t.Errorf("API call used %q", r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{"number":1,"html_url":"https://github.com/example-org/example-service/issues/1"}`))
	}))
	defer upstream.Close()
	m := loadGitHubConnection(t, upstream.URL, map[string]any{"appId": "1234", "installationId": 77, "privateKeyFile": keyFile, "repositories": []string{"example-service"}, "permissions": map[string]string{"issues": "write", "metadata": "read"}})
	args := json.RawMessage(`{"owner":"example-org","repo":"example-service","number":"1"}`)
	for range 2 {
		if _, err := m.Call(t.Context(), "github", "get_issue", args, ""); err != nil {
			t.Fatal(err)
		}
	}
	if exchanges.Load() != 1 {
		t.Fatal("installation token not cached", exchanges.Load())
	}
	if m.LearningInputSafe("github", "ghs_fixture1") {
		t.Fatal("installation token eligible for evidence capture")
	}
	// A revoked or expired token is replaced on the next call, never retried silently.
	reject.Store(true)
	if _, err := m.Call(t.Context(), "github", "get_issue", args, ""); err == nil {
		t.Fatal("rejected call reported success")
	}
	if _, err := m.Call(t.Context(), "github", "get_issue", args, ""); err != nil || exchanges.Load() != 2 {
		t.Fatal("rejected token was not replaced", err, exchanges.Load())
	}
}

func TestHTTPGitHubAppExchangeFailureDoesNotLeakOrCall(t *testing.T) {
	_, keyFile := githubAppFixture(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/77/access_tokens" {
			calls.Add(1)
		}
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer upstream.Close()
	m := loadGitHubConnection(t, upstream.URL, map[string]any{"appId": "1234", "installationId": 77, "privateKeyFile": keyFile})
	_, err := m.Call(t.Context(), "github", "get_issue", json.RawMessage(`{"owner":"example-org","repo":"example-service","number":"1"}`), "")
	if err == nil || !strings.Contains(err.Error(), "installation") || strings.Contains(err.Error(), "BEGIN") || calls.Load() != 0 {
		t.Fatal("exchange failure", err, calls.Load())
	}
}

func TestHTTPGitHubAppRejectsUnsafeConfiguration(t *testing.T) {
	_, keyFile := githubAppFixture(t)
	valid := func() map[string]any {
		return map[string]any{"id": "github", "kind": "http", "endpoint": "https://api.github.com", "githubApp": map[string]any{"appId": "1234", "installationId": 77, "privateKeyFile": keyFile}, "operations": []any{map[string]any{"name": "issue", "readOnly": true, "path": "/issue"}}}
	}
	for name, change := range map[string]func(map[string]any){
		"valid":           nil,
		"secret too":      func(c map[string]any) { c["secretEnv"] = "GITHUB_TOKEN" },
		"relative key":    func(c map[string]any) { c["githubApp"].(map[string]any)["privateKeyFile"] = "app.pem" },
		"no installation": func(c map[string]any) { delete(c["githubApp"].(map[string]any), "installationId") },
		"named app":       func(c map[string]any) { c["githubApp"].(map[string]any)["appId"] = "teambot" },
		"owner in repository": func(c map[string]any) {
			c["githubApp"].(map[string]any)["repositories"] = []string{"example-org/example-service"}
		},
		"admin permission": func(c map[string]any) {
			c["githubApp"].(map[string]any)["permissions"] = map[string]string{"issues": "admin"}
		},
		"other auth header": func(c map[string]any) { c["authHeader"] = "X-Token" },
		"not http":          func(c map[string]any) { c["kind"] = "email" },
	} {
		c := valid()
		if change != nil {
			change(c)
		}
		raw, _ := json.Marshal(map[string]any{"version": 1, "connections": []any{c}})
		file := filepath.Join(t.TempDir(), "connections.json")
		os.WriteFile(file, raw, 0600)
		m, err := Load(file)
		if err == nil {
			m.Close()
		}
		if (err == nil) != (change == nil) {
			t.Fatal(name, err)
		}
	}
}

func TestGitHubIssuesExampleConnectionLoadsAndRoutes(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/integrations/data-connections.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var example struct {
		Connections []map[string]any `json:"connections"`
	}
	if err = json.Unmarshal(raw, &example); err != nil {
		t.Fatal(err)
	}
	var github map[string]any
	for _, c := range example.Connections {
		if c["id"] == "github" {
			github = c
		}
	}
	if github == nil {
		t.Fatal("example has no GitHub connection")
	}
	_, keyFile := githubAppFixture(t)
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/77/access_tokens" {
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"token": "ghs_example", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
			return
		}
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	github["endpoint"] = upstream.URL
	github["githubApp"].(map[string]any)["appId"] = "1234"
	github["githubApp"].(map[string]any)["installationId"] = 77
	github["githubApp"].(map[string]any)["privateKeyFile"] = keyFile
	raw, _ = json.Marshal(map[string]any{"version": 1, "connections": []any{github}})
	file := filepath.Join(t.TempDir(), "connections.json")
	os.WriteFile(file, raw, 0600)
	m, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	// Chat and Slack runs only see connections that allow external AI processing.
	if info, _ := m.Info("github"); !info.ExternalProcessing {
		t.Fatal("chat cannot use the example: externalProcessing is off")
	}
	for _, call := range []struct{ op, args string }{
		{"get_issue", `{"owner":"example-org","repo":"example-service","number":12}`},
		{"comment_on_issue", `{"owner":"example-org","repo":"example-service","number":12,"comment":{"body":"Seen."}}`},
	} {
		if _, err := m.Call(t.Context(), "github", call.op, json.RawMessage(call.args), ""); err != nil {
			t.Fatal(call.op, err)
		}
	}
	if _, err := m.Call(t.Context(), "github", "get_issue", json.RawMessage(`{"owner":"another-org","repo":"x","number":1}`), ""); err == nil {
		t.Fatal("example reached another organization")
	}
	if strings.Join(paths, ",") != "GET /repos/example-org/example-service/issues/12,POST /repos/example-org/example-service/issues/12/comments" {
		t.Fatal(paths)
	}
}
