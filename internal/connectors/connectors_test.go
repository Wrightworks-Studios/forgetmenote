package connectors

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const argsSchema = `{"type":"object","properties":{"term":{"type":"string"}},"required":["term"],"additionalProperties":false}`

// An instance without data connections, or with the module off, has no
// manager. What its callers ask of one answers "not available" instead of
// panicking: a stored learning automation may still name a connection.
func TestAMissingManagerAnswersNotAvailable(t *testing.T) {
	var m *Manager
	if m.LearningInputSafe("warehouse", "select 1") {
		t.Fatal("input is safe to capture without a connection")
	}
	if _, ok := m.Info("warehouse"); ok {
		t.Fatal("a connection exists without a manager")
	}
	if list := m.List(); len(list) != 0 {
		t.Fatal(list)
	}
	m.Close()
}

func TestConfiguredOperationPinMatchesDiscoveredContract(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answer":"found"}`))
	}))
	defer upstream.Close()
	cfg := Config{Version: 1, Connections: []Connection{{ID: "search", Kind: "http", Endpoint: upstream.URL, Operations: []Operation{{Name: "find", InputSchema: json.RawMessage(argsSchema), ReadOnly: true, Method: "GET", Path: "/search"}}}}}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ops, err := m.Tools(t.Context(), "search")
	if err != nil {
		t.Fatal(err)
	}
	pin := ops[0].SchemaHash
	cfg.Connections[0].Operations[0].SchemaHash = pin
	pinned, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	if _, err = pinned.Call(t.Context(), "search", "find", json.RawMessage(`{"term":"hello"}`), pin); err != nil {
		t.Fatal("an unchanged pinned contract was rejected", err)
	}
	cfg.Connections[0].Operations[0].Description = "Changed contract"
	changed, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer changed.Close()
	if _, err = changed.Call(t.Context(), "search", "find", json.RawMessage(`{"term":"hello"}`), ""); err == nil {
		t.Fatal("configured pin did not block a changed contract")
	}
}

func TestConnectionContractsHaveOneOwner(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" || r.URL.Query().Get("q") != "hello" {
			t.Errorf("runtime routing changed through caller memory: %s", r.URL)
		}
		w.Write([]byte(`{"answer":"found"}`))
	}))
	defer upstream.Close()
	cfg := Config{Version: 1, Connections: []Connection{{ID: "search", Kind: "http", Endpoint: upstream.URL, Operations: []Operation{{Name: "find", InputSchema: json.RawMessage(argsSchema), ReadOnly: true, Method: "GET", Path: "/search", Query: map[string]string{"q": "{term}"}}}}}}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ops, err := m.Tools(t.Context(), "search")
	if err != nil {
		t.Fatal(err)
	}
	pin := ops[0].SchemaHash
	for i := range ops[0].InputSchema {
		ops[0].InputSchema[i] = ' '
	}
	policy, ok := m.ConfiguredOperation("search", "find")
	if !ok {
		t.Fatal("missing configured operation")
	}
	for i := range policy.InputSchema {
		policy.InputSchema[i] = ' '
	}
	cfg.Connections[0].Operations[0].Path = "/changed"
	cfg.Connections[0].Operations[0].Query["q"] = "changed"
	if _, err = m.Call(t.Context(), "search", "find", json.RawMessage(`{"term":"hello"}`), pin); err != nil {
		t.Fatal("caller mutated the runtime contract", err)
	}
}

func TestHTTPConnectionSchemaRotationAndRedaction(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "token")
	os.WriteFile(secret, []byte("test-runtime-secret-one"), 0600)
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Get("q") != "a&b" {
			t.Errorf("query escaping: %s", r.URL.RawQuery)
		}
		json.NewEncoder(w).Encode(map[string]string{"authorization": r.Header.Get("Authorization"), "answer": "found"})
	}))
	defer upstream.Close()
	m, err := New(Config{Version: 1, Connections: []Connection{{ID: "search", Label: "Search", Kind: "http", Endpoint: upstream.URL, SecretFile: secret, Operations: []Operation{{Name: "find", Description: "Search", InputSchema: json.RawMessage(argsSchema), ReadOnly: true, Method: "GET", Path: "/search", Query: map[string]string{"q": "{term}"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, key := range []string{"test-runtime-secret-one", "test-runtime-secret-two"} {
		os.WriteFile(secret, []byte(key), 0600)
		r, err := m.Call(context.Background(), "search", "find", json.RawMessage(`{"term":"a&b"}`), "")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(r)
		if strings.Contains(string(raw), key) || !strings.Contains(string(raw), "found") || !r.Receipt.Complete || r.Receipt.SchemaHash == "" {
			t.Fatalf("result %s", raw)
		}
	}
	if requests != 2 {
		t.Fatalf("requests: %d", requests)
	}
	if _, err = m.Call(context.Background(), "search", "find", json.RawMessage(`{"term":5}`), ""); err == nil {
		t.Fatal("invalid arguments accepted")
	}
	if requests != 2 {
		t.Fatal("invalid arguments reached upstream")
	}
}

func TestHTTPConnectionRefusesRedirectsAndOversizedResults(t *testing.T) {
	otherCalls := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { otherCalls++ }))
	defer other.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, other.URL, 302)
			return
		}
		w.Write([]byte(strings.Repeat("x", 2048)))
	}))
	defer upstream.Close()
	m, err := New(Config{Version: 1, Connections: []Connection{{ID: "test", Kind: "http", Endpoint: upstream.URL, MaxBytes: 1024, Operations: []Operation{
		{Name: "redirect", Description: "redirect", Method: "GET", Path: "/redirect", ReadOnly: true},
		{Name: "large", Description: "large", Method: "GET", Path: "/large", ReadOnly: true},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"redirect", "large"} {
		if _, err = m.Call(context.Background(), "test", name, json.RawMessage(`{}`), ""); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	if otherCalls != 0 {
		t.Fatal("followed redirect")
	}
}

func TestHTTPPathArgumentsCannotEscapeConfiguredRoute(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.Write([]byte(`{"ok":true}`)) }))
	defer upstream.Close()
	m, err := New(Config{Version: 1, Connections: []Connection{{ID: "paths", Kind: "http", Endpoint: upstream.URL, Operations: []Operation{{Name: "read", ReadOnly: true, Method: "GET", Path: "/safe/{term}", InputSchema: json.RawMessage(argsSchema)}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, term := range []string{"..", "../item", "a/b", `a\b`, "%2e%2e", "%252fsecret"} {
		raw, _ := json.Marshal(map[string]string{"term": term})
		if _, err = m.Call(t.Context(), "paths", "read", raw, ""); err == nil {
			t.Fatal("unsafe path reached upstream", term)
		}
	}
	if requests != 0 {
		t.Fatal("invalid route contacted upstream", requests)
	}
	if _, err = m.Call(t.Context(), "paths", "read", json.RawMessage(`{"term":"record 7"}`), ""); err != nil || requests != 1 {
		t.Fatal("ordinary path rejected", err, requests)
	}
}
