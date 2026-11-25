package automation

import (
	"encoding/json"
	"strings"
	"testing"
)

func scriptSpec(t *testing.T) Spec {
	t.Helper()
	raw := `{"id":"example","kind":"script","actor":"robot","project":"notes","disabled":true,"script":{"sourceProject":"packages","sourceID":"source","packagePath":"automations/example","revision":"` + strings.Repeat("a", 40) + `","digest":"` + strings.Repeat("b", 64) + `","runtime":"python-v1","entrypoint":"main.py","environment":"` + strings.Repeat("c", 64) + `","capabilities":["checkpoint"]},"timeoutSeconds":60}`
	var spec Spec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}
func TestScriptDefinitionAcceptedWithoutAI(t *testing.T) {
	spec := scriptSpec(t)
	if err := spec.ValidateConfigured(); err != nil {
		t.Fatal(err)
	}
	if spec.UsesAI() {
		t.Fatal("deterministic package unexpectedly requires a provider")
	}
}

func TestScriptSlackPostsRequireSharedChannelAndNoRetry(t *testing.T) {
	spec := scriptSpec(t)
	spec.Script.Capabilities = []string{"connectors.read", "messages.post"}
	if err := spec.ValidateConfigured(); err == nil {
		t.Fatal("messages.post accepted without a shared notification channel")
	}
	spec.NotifyChannel = "Cposts"
	if err := spec.ValidateConfigured(); err != nil {
		t.Fatal(err)
	}
	spec.Retry = &RetryPolicy{MaxAttempts: 2, DelaySeconds: 5}
	if err := spec.ValidateConfigured(); err == nil {
		t.Fatal("automatic retries permitted for Slack posts")
	}
}

func TestScriptRetriesRequireDeterministicReadCapabilities(t *testing.T) {
	spec := scriptSpec(t)
	spec.Script.Capabilities = []string{"connectors.read", "incidents.report"}
	spec.Retry = &RetryPolicy{MaxAttempts: 2, DelaySeconds: 5}
	if err := spec.ValidateConfigured(); err != nil {
		t.Fatal(err)
	}
	for _, capability := range []string{"ai.structured", "ai.decisions", "decision.evaluate", "web.search", "notebooks.publish", "connectors.submit"} {
		spec.Script.Capabilities = []string{capability}
		if err := spec.ValidateConfigured(); err == nil {
			t.Fatalf("automatic retries permitted for %s", capability)
		}
	}
}

// Definitions saved before the sandbox existed have no sandbox field and run
// sandboxed; only an explicit false in the grant runs package code unconfined.
func TestScriptSandboxIsDefaultAndOptOutIsExplicit(t *testing.T) {
	spec := scriptSpec(t)
	if !spec.Script.Sandboxed() {
		t.Fatal("definition without a sandbox field runs unconfined")
	}
	for raw, sandboxed := range map[string]bool{`{"sandbox":false}`: false, `{"sandbox":true}`: true} {
		var script Script
		if err := json.Unmarshal([]byte(raw), &script); err != nil {
			t.Fatal(err)
		}
		saved, _ := json.Marshal(script)
		if script.Sandboxed() != sandboxed || !strings.Contains(string(saved), raw[1:len(raw)-1]) {
			t.Fatalf("%s: sandboxed %v, saved as %s", raw, script.Sandboxed(), saved)
		}
	}
}
