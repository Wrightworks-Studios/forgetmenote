package connectors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperatorConfigCannotHideTrailingJSONBeyondReadLimit(t *testing.T) {
	for _, test := range []struct {
		name, body string
		limit      int
		load       func(string) error
	}{
		{"connections", `{"version":1,"connections":[]}`, 2 << 20, func(path string) error {
			manager, err := Load(path)
			if manager != nil {
				manager.Close()
			}
			return err
		}},
		{"personal", `{"version":1}`, 1 << 20, func(path string) error { _, err := LoadPersonalConfig(path); return err }},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			raw := test.body + strings.Repeat(" ", test.limit-len(test.body)) + `{"unknown":true}`
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if err := test.load(path); err == nil {
				t.Fatal("oversized operator config silently truncated trailing JSON")
			}
		})
	}
}
