package chat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func providerTestConnection(endpoint, format string) Connection {
	return Connection{ID: "test", Label: "Test", Format: format, BaseURL: endpoint, Models: []string{"test-model"}, DefaultModel: "test-model", Local: true, Capabilities: Capabilities{Tools: true}}
}
func TestConfigStrictAndPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.json")
	raw := `{"connections":[{"id":"test","label":"Test","format":"chat-completions","baseURL":"http://localhost:1234/v1","models":["test-model"],"defaultModel":"test-model","local":true,"secretFile":"/run/secrets/private-token","capabilities":{"tools":true}}],"defaultConnection":"test"}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := c.Resolve("test", "test-model")
	if err != nil || conn.Limits.ContextTokens != 16384 {
		t.Fatalf("resolve/default limits: %#v %v", conn, err)
	}
	public, _ := json.Marshal(c.PublicConnections())
	if !strings.Contains(string(public), "test-model") || strings.Contains(string(public), "localhost") || strings.Contains(string(public), "private-token") || strings.Contains(string(public), "secretFile") {
		t.Fatalf("unsafe/incomplete public configuration: %s", public)
	}
	if _, err = c.Resolve("test", "arbitrary-model"); err == nil {
		t.Fatal("accepted a model outside allowlist")
	}
	if err = os.WriteFile(path, []byte(strings.Replace(raw, `"tools":true`, `"tools":true,"madeUp":true`, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadConfig(path); err == nil {
		t.Fatal("accepted unknown field")
	}
	if err = os.WriteFile(path, []byte(raw+`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadConfig(path); err == nil {
		t.Fatal("accepted trailing document")
	}
}
func TestConfigRefusesUnsafeSelections(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"url credential", func(c *Config) { c.Connections[0].BaseURL = "http://user:secret@localhost/v1" }},
		{"url query", func(c *Config) { c.Connections[0].BaseURL = "http://localhost/v1?key=secret" }},
		{"cloud http", func(c *Config) { c.Connections[0].Local = false }},
		{"local cloud", func(c *Config) {
			c.LocalOnly = true
			c.Connections[0].Local = false
			c.Connections[0].BaseURL = "https://example.com/v1"
		}},
		{"local research", func(c *Config) { c.LocalOnly = true; c.Research = &ResearchConfig{Enabled: true} }},
		{"strict without tools", func(c *Config) { c.Connections[0].Capabilities = Capabilities{StrictTools: true} }},
		{"JSON output on another format", func(c *Config) { c.Connections[0].Format = "messages"; c.Connections[0].Capabilities.JSONOutput = true }},
		{"thinking without replay", func(c *Config) { c.Connections[0].Capabilities.Thinking = "enabled" }},
		{"invalid limits", func(c *Config) { c.Limits.OutputTokens = -1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{Connections: []Connection{providerTestConnection("http://localhost/v1", "chat-completions")}, DefaultConnection: "test"}
			tt.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}

func TestModelCapacitiesAreNotClampedByGlobalDefaults(t *testing.T) {
	connection := providerTestConnection("http://localhost/v1", "chat-completions")
	connection.Limits = Limits{ContextTokens: 1000000, OutputTokens: 393216, RunTokens: 1, DailyTokens: 1}
	other := providerTestConnection("http://localhost/other", "chat-completions")
	other.ID = "other"
	cfg := Config{Connections: []Connection{connection, other}, DefaultConnection: "test", Limits: Limits{RunTokens: 1, DailyTokens: 1}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	got := cfg.Connections[0].Limits
	if got.ContextTokens != 1000000 || got.OutputTokens != 393216 {
		t.Fatalf("model capacities clamped: %+v", got)
	}
	if cfg.Connections[1].Limits.ContextTokens != 16384 || cfg.Connections[1].Limits.OutputTokens != 2048 {
		t.Fatal("unrelated provider inherited another provider's capacity")
	}
	raw, _ := json.Marshal(cfg.PublicConnections())
	if strings.Contains(string(raw), "runTokens") || strings.Contains(string(raw), "dailyTokens") {
		t.Fatalf("obsolete quotas advertised: %s", raw)
	}
}

// Hosted source processing is gone. A chat configuration written for an
// earlier release may still carry its sources block, which loads unchecked and
// configures nothing, so an upgraded instance starts.
func TestRetiredSourceConfigurationStillLoads(t *testing.T) {
	connection := providerTestConnection("http://localhost:1234/v1", "responses")
	raw, err := json.Marshal(map[string]any{"connections": []Connection{connection}, "sources": map[string]any{"connection": "unknown", "model": "retired-model", "transcriptionModel": "bad\nmodel"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "chat.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatal(err)
	}
}
