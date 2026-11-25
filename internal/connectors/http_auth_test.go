package connectors

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func loadHTTPAuth(t *testing.T, endpoint, auth string) *Manager {
	t.Helper()
	file := filepath.Join(t.TempDir(), "connections.json")
	raw := `{"version":1,"connections":[{"id":"source","kind":"http","endpoint":` + strconvJSON(endpoint) + `,` + auth + `,"operations":[{"name":"read","readOnly":true,"method":"POST","path":"/search","body":true,"inputSchema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}}]}]}`
	if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func strconvJSON(value string) string { b, _ := json.Marshal(value); return string(b) }

func TestHTTPSignsAWSRequest(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "TESTACCESSKEY")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-signing-key")
	t.Setenv("AWS_SESSION_TOKEN", "test-session-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sum := sha256.Sum256([]byte(`{"query":"count"}`))
		if !strings.Contains(r.Header.Get("Authorization"), "/us-east-1/es/aws4_request") || r.Header.Get("X-Amz-Security-Token") != "test-session-token" || r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) {
			t.Error("request was not signed for the configured service and body")
		}
		json.NewEncoder(w).Encode(map[string]string{"authorization": r.Header.Get("Authorization"), "token": r.Header.Get("X-Amz-Security-Token"), "answer": "ok"})
	}))
	defer server.Close()
	m := loadHTTPAuth(t, server.URL, `"aws":{"region":"us-east-1","service":"es"}`)
	result, err := m.Call(t.Context(), "source", "read", json.RawMessage(`{"query":"count"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "test-session-token") || strings.Contains(string(raw), "TESTACCESSKEY") {
		t.Fatal("AWS credentials escaped")
	}
}

func TestHTTPLoginCachesRotatesAndFailsWithoutRetry(t *testing.T) {
	t.Setenv("SOURCE_USER", "test-user")
	t.Setenv("SOURCE_PASSWORD", "password-one")
	var logins, reads atomic.Int32
	var reject atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			logins.Add(1)
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["username"] != "test-user" || body["password"] != os.Getenv("SOURCE_PASSWORD") || body["provider"] != "db" {
				t.Error("login did not use mounted credentials and fixed fields")
			}
			json.NewEncoder(w).Encode(map[string]string{"access_token": "session-" + os.Getenv("SOURCE_PASSWORD")})
			return
		}
		reads.Add(1)
		if reject.Load() {
			http.Error(w, "private-password-one", 401)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"answer": "ok", "reflected": r.Header.Get("Authorization")})
	}))
	defer server.Close()
	m := loadHTTPAuth(t, server.URL, `"secretEnv":"SOURCE_PASSWORD","login":{"path":"/login","usernameEnv":"SOURCE_USER","fields":{"provider":"db","refresh":true},"tokenField":"access_token"}`)
	call := func() error {
		result, err := m.Call(t.Context(), "source", "read", json.RawMessage(`{"query":"count"}`), "")
		if raw, _ := json.Marshal(result); strings.Contains(string(raw), "session-password") {
			t.Fatal("login token escaped")
		}
		return err
	}
	for range 2 {
		if err := call(); err != nil {
			t.Fatal(err)
		}
	}
	if logins.Load() != 1 {
		t.Fatal("login was not reused")
	}
	t.Setenv("SOURCE_PASSWORD", "password-two")
	if err := call(); err != nil {
		t.Fatal(err)
	}
	if logins.Load() != 2 {
		t.Fatal("credential rotation did not refresh login")
	}
	reject.Store(true)
	if err := call(); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("upstream failure was hidden or leaked")
	}
	if logins.Load() != 2 || reads.Load() != 4 {
		t.Fatal("failed request was retried")
	}
	reject.Store(false)
	if err := call(); err != nil {
		t.Fatal(err)
	}
	if logins.Load() != 3 {
		t.Fatal("rejected token was not invalidated")
	}
	if m.LearningInputSafe("source", "session-password-two") {
		t.Fatal("learning retained a login secret")
	}
}

func TestHTTPBasicUsesSeparateExistingSecrets(t *testing.T) {
	t.Setenv("SOURCE_USER", "existing-user")
	t.Setenv("SOURCE_PASSWORD", "existing-password")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "existing-user" || p != "existing-password" {
			t.Error("invalid Basic credentials")
		}
		w.Write([]byte(`{"answer":"ok"}`))
	}))
	defer server.Close()
	m := loadHTTPAuth(t, server.URL, `"authScheme":"Basic","secretEnv":"SOURCE_PASSWORD","usernameEnv":"SOURCE_USER"`)
	if _, err := m.Call(t.Context(), "source", "read", json.RawMessage(`{"query":"count"}`), ""); err != nil {
		t.Fatal(err)
	}
}
