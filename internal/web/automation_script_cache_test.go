package web

import (
	"encoding/json"
	"forgetmenote/internal/automation"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestScriptAnalysisCacheAcrossRunsAndConsent(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"ai.structured"}, nil, nil)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"value\":7}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()
	cfg := map[string]any{"defaultConnection": "local", "connections": []any{map[string]any{"id": "local", "label": "Fixture", "format": "chat-completions", "baseURL": upstream.URL, "models": []string{"synthetic", "other"}, "defaultModel": "synthetic", "local": true}}}
	raw, _ := json.Marshal(cfg)
	file := filepath.Join(t.TempDir(), "chat.json")
	os.WriteFile(file, raw, 0600)
	if err := app.ConfigureChat(file); err != nil {
		t.Fatal(err)
	}
	e.run.Spec.AIConnection = "local"
	e.run.Spec.Script.RetentionHours = 1
	prepare := func(id string) {
		t.Helper()
		e.run.ID = automation.Hash(id)
		e.run.Script.Steps = map[string]*automation.ScriptStep{}
		var err error
		e.run.Script.Contract, err = app.scriptContract(e.run.Spec)
		if err != nil {
			t.Fatal(err)
		}
		e.ctx, err = app.automationContext(t.Context(), e.run.Spec)
		if err != nil {
			t.Fatal(err)
		}
	}
	args := map[string]any{"cacheKey": "source-content-policy-schema", "prompt": "Synthetic only", "schema": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "integer"}}, "required": []string{"value"}, "additionalProperties": false}}
	for _, id := range []string{"one", "two"} {
		prepare(id)
		if _, err := hostStep(t, app, e, "new-step", "ai.structured", args); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("unchanged analysis repeated", calls.Load())
	}
	e.run.Spec.Model = "other"
	prepare("changed-model")
	if _, err := hostStep(t, app, e, "model", "ai.structured", args); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("model configuration did not invalidate cache")
	}
	e.run.Spec.ExternalProcessing = false
	if _, err := hostStep(t, app, e, "blocked", "ai.structured", args); err == nil {
		t.Fatal("consent change released cached analysis")
	}
}
