package connectors

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHTTPConfiguredHostReachesVirtualHost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "application.example" || r.URL.Path != "/health/" || r.URL.Query().Get("format") != "json" {
			http.Error(w, "wrong virtual host or health request", http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"database":"OK"}`))
	}))
	defer upstream.Close()
	config := map[string]any{"version": 1, "connections": []any{map[string]any{
		"id": "health", "kind": "http", "endpoint": upstream.URL, "hostHeader": "application.example",
		"operations": []any{map[string]any{"name": "probe", "path": "/health/", "readOnly": true,
			"response": "status", "query": map[string]string{"format": "json"}}},
	}}}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "connections.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	result, err := m.Call(t.Context(), "health", "probe", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result.Data)
	if string(raw) != `{"body":{"database":"OK"},"httpStatus":200}` {
		t.Fatalf("virtual-host probe: %s", raw)
	}
}

func TestHTTPHostConfigurationValidation(t *testing.T) {
	for _, host := range []string{"application.example", "app", "", "bad\r\nHeader: injected", "with space", "https://app.example", "app.example/path", "app.example?query", "app.example#fragment", "user@app.example", "app.example:8080", "-app.example", strings.Repeat("a", 254)} {
		t.Run(url.QueryEscape(host), func(t *testing.T) {
			b, _ := json.Marshal(map[string]any{"version": 1, "connections": []any{map[string]any{
				"id": "test", "kind": "http", "endpoint": "https://upstream.example", "hostHeader": host,
				"operations": []any{map[string]any{"name": "read", "path": "/read", "readOnly": true}},
			}}})
			var cfg Config
			if err := json.Unmarshal(b, &cfg); err != nil {
				t.Fatal(err)
			}
			m, err := New(cfg)
			if err == nil {
				m.Close()
			}
			want := host == "application.example" || host == "app" || host == ""
			if (err == nil) != want {
				t.Fatalf("host %q accepted=%v, want %v", host, err == nil, want)
			}
		})
	}
	_, err := New(Config{Version: 1, Connections: []Connection{{
		ID: "remote", Kind: "mcp", Endpoint: "https://upstream.example", HostHeader: "application.example",
		Operations: []Operation{{Name: "read", ReadOnly: true}},
	}}})
	if err == nil {
		t.Fatal("accepted an HTTP host override for a different connector kind")
	}
}

func TestHTTPHostOverridePreservesTLSEndpointIdentity(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "application.example" {
			t.Errorf("wrong HTTP host: %q", r.Host)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := New(Config{Version: 1, Connections: []Connection{{
		ID: "tls", Kind: "http", Endpoint: upstream.URL, HostHeader: "application.example", CAFile: ca,
		Operations: []Operation{{Name: "read", Path: "/", ReadOnly: true}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err := m.Call(t.Context(), "tls", "read", json.RawMessage(`{}`), ""); err != nil {
		t.Fatalf("TLS must validate the endpoint certificate despite the HTTP host override: %v", err)
	}
}
