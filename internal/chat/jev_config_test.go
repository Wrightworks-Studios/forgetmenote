package chat

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigAcceptsExplicitJevOperatorConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.json")
	raw := `{"connections":[{"id":"local","label":"Local","format":"chat-completions","baseURL":"http://localhost/v1","models":["test"],"local":true}],"jev":{"enabled":true,"model":"jev-1.13.0","secretFile":"/run/secrets/typesafe-api-key"}}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("explicit operator Jev configuration should load: %v", err)
	}
}
