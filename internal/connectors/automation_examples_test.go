package connectors

import (
	"encoding/json"
	"os"
	"testing"
)

func TestAutomationConnectionExamplesValidateOffline(t *testing.T) {
	raw, err := os.ReadFile("testdata/automation-http.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err = json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Connections {
		cfg.Connections[i].CAFile = ""
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
}
