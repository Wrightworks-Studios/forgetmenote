package decision

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperatorConfigurationIsExplicitAndCloudOnly(t *testing.T) {
	for _, mutate := range []struct {
		name string
		f    func(*Config)
	}{
		{"missing model", func(c *Config) { c.Model = "" }},
		{"http", func(c *Config) { c.Endpoint = "http://localhost/v1/systemone" }},
		{"credentials", func(c *Config) { c.Endpoint = "https://secret@example.test/v1/systemone" }},
		{"query", func(c *Config) { c.Endpoint = "https://example.test/v1/systemone?key=private" }},
		{"path", func(c *Config) { c.Endpoint = "https://example.test/v1/chat/completions" }},
		{"relative secret", func(c *Config) { c.SecretFile = "key" }},
		{"excess retries", func(c *Config) { c.Limits.Retries = 3 }},
		{"excess input", func(c *Config) { c.Limits.RequestBytes = 65537 }},
		{"negative limit", func(c *Config) { c.Limits.TimeoutSeconds = -1 }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			c := Config{Enabled: true, Model: "jev-1.13.0"}
			mutate.f(&c)
			if err := c.Validate(false); err == nil {
				t.Fatal("unsafe operator configuration accepted")
			}
		})
	}
	c := Config{Enabled: true, Model: "jev-1.13.0"}
	if !errors.Is(c.Validate(true), ErrLocalOnly) {
		t.Fatal("local-only accepted")
	}
	if err := c.Validate(false); err != nil {
		t.Fatal(err)
	}
	if c.Endpoint != "https://api.typesafe.ai/v1/systemone" || c.SecretFile != "/run/secrets/typesafe-api-key" || c.Limits.Retries != 0 {
		t.Fatal("unsafe defaults")
	}
	var absent *Config
	if err := absent.Validate(true); err != nil {
		t.Fatal(err)
	}
}
func TestMountedSecretIsBoundedAndAbsentWhenDisabled(t *testing.T) {
	for _, secret := range []string{"", "\n", strings.Repeat("x", 4097), "key\r\ninjection"} {
		path := filepath.Join(t.TempDir(), "key")
		if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := New(Config{Enabled: true, Model: "jev-1.13.0", SecretFile: path}, &Ledger{}, false)
		if err == nil || strings.Contains(err.Error(), path) {
			t.Fatalf("unsafe credential handling: %v", err)
		}
	}
	if _, err := New(Config{SecretFile: "/nonexistent"}, nil, false); !errors.Is(err, ErrDisabled) {
		t.Fatal("disabled service accessed secret")
	}
}

func TestStandaloneConfigFileIsStrictAndDoesNotReadDisabledSecret(t *testing.T) {
	for _, test := range []struct {
		name, body string
		valid      bool
	}{
		{"valid", `{"enabled":true,"model":"jev-1.13.0"}`, true},
		{"explicit provider", `{"provider":"typesafe","enabled":true,"model":"jev-1.13.0"}`, true},
		{"unsupported provider", `{"provider":"unconfigured","enabled":true,"model":"small-decision-model"}`, false},
		{"disabled", `{"enabled":false,"secretFile":"/nonexistent"}`, true},
		{"null", `null`, false},
		{"trailing JSON", `{} {}`, false},
		{"unknown field", `{"enabled":false,"token":"private-value"}`, false},
		{"oversized", strings.Repeat(" ", 65536) + `{}`, false},
		{"missing model", `{"enabled":true}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jev.json")
			if err := os.WriteFile(path, []byte(test.body), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if (err == nil) != test.valid || err != nil && strings.Contains(err.Error(), "private-value") {
				t.Fatalf("configuration result: %v", err)
			}
		})
	}
}
