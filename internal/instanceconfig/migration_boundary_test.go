package instanceconfig

import (
	"encoding/json"
	"forgetmenote/internal/automation"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationCannotAcknowledgeUnreadableBoundaryOutput(t *testing.T) {
	t.Setenv("FMN_LIMIT_STATE_BYTES", "67108864")
	a := automation.Config{Version: 1, Jobs: []automation.Spec{{ID: "fixture", Kind: "task", Actor: "operator", Project: "default", Output: "notes/output.md", Instruction: "x", ExternalProcessing: true}}}
	c := Config{Version: 1, Integrations: Integrations{Automations: &a}}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	a.Jobs[0].Instruction = strings.Repeat("x", 1+MaximumBytes-len(raw))
	if err := a.Validate(); err != nil {
		t.Fatalf("invalid fixture: %v", err)
	}
	raw, err = json.MarshalIndent(c, "", "  ")
	if err != nil || len(raw) != MaximumBytes {
		t.Fatalf("wrong boundary fixture: size=%d err=%v", len(raw), err)
	}
	raw = nil
	dir := t.TempDir()
	legacy := filepath.Join(dir, "automations.json")
	source, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(source) > MaximumBytes {
		t.Fatal("legacy file exceeds original budget")
	}
	if err := os.WriteFile(legacy, source, 0600); err != nil {
		t.Fatal(err)
	}
	source = nil
	output := filepath.Join(dir, "instance.json")
	err = Migrate(func(k string) string {
		if k == "AUTOMATIONS_CONFIG_FILE" {
			return legacy
		}
		return ""
	}, dir, output)
	if err != nil {
		if !strings.Contains(err.Error(), "byte budget") {
			t.Fatalf("valid input rejected before the byte boundary: %v", err)
		}
		if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
			t.Fatal("refused migration retained an output file")
		}
		return
	}
	if _, err := Load(output); err != nil {
		info, _ := os.Stat(output)
		t.Fatalf("migration acknowledged output its loader refuses: size=%d error=%v", info.Size(), err)
	}
}
