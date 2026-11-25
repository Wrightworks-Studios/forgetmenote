package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
)

func TestScriptDecisionsVisionPreviewReplayAndAuthority(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"ai.decisions", "ai.images"}, nil, nil)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/decisions" || body["questions"] == nil || body["input"] == nil || body["tools"] != nil {
			t.Errorf("invalid Decisions request: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"gpt-6-luna","answers":[{"type":"choice","name":"identity","choice":"pass","confidence":0.9,"probabilities":[{"value":"pass","probability":0.95},{"value":"uncertain","probability":0.05}]}],"usage":{"input_tokens":100,"output_tokens":0}}`))
	}))
	defer upstream.Close()
	cfg := map[string]any{"defaultConnection": "local", "connections": []any{map[string]any{"id": "local", "label": "Fixture", "format": "responses", "baseURL": upstream.URL, "models": []string{"gpt-6-luna"}, "defaultModel": "gpt-6-luna", "local": true, "capabilities": map[string]bool{"images": true}}}}
	raw, _ := json.Marshal(cfg)
	file := filepath.Join(t.TempDir(), "chat.json")
	os.WriteFile(file, raw, 0600)
	if err := app.ConfigureChat(file); err != nil {
		t.Fatal(err)
	}
	e.run.Spec.AIConnection = "local"
	e.run.DryRun = true
	e.run.Script.Contract, _ = app.scriptContract(e.run.Spec)
	image, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl2nGAAAAAASUVORK5CYII=")
	args := map[string]any{"prompt": "Synthetic creative evidence", "images": []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(image)}, "questions": []any{map[string]any{"type": "choice", "name": "identity", "instructions": "Is identity visible?", "choices": []any{map[string]string{"value": "pass", "description": "Identity is visible"}, map[string]string{"value": "uncertain", "description": "Identity is unclear"}}}}}
	if _, err := hostStep(t, app, e, "assessment", "ai.decisions", args); err != nil {
		t.Fatal(err)
	}
	if e.run.InputTokens != 100 || e.run.OutputTokens != 0 || !e.run.Script.Steps["assessment"].Effect {
		t.Fatal("paid preview accounting was lost")
	}
	if _, err := hostStep(t, app, e, "assessment", "ai.decisions", args); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("checkpoint replay repeated a paid call")
	}
	e.run.Spec.MaxTokens = 1
	e.run.Script.Contract, _ = app.scriptContract(e.run.Spec)
	if _, err := hostStep(t, app, e, "over-budget", "ai.decisions", args); err == nil || calls != 1 {
		t.Fatal("budget check occurred after paid work")
	}
	e.run.Spec.ExternalProcessing = false
	if _, err := hostStep(t, app, e, "assessment", "ai.decisions", args); err == nil {
		t.Fatal("revoked consent released retained results")
	}
}

// ai.decisions admits its model like every other automation model call:
// instance AI policy, then the run's source scope, its provider, the
// provider's estimate and the run's token budget, all before a model slot is
// reserved. The held slot turns any reservation made before a refusal into a
// wait for the deadline, so each refusal below proves its place in the order.
func TestScriptDecisionsAdmissionOrder(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"ai.decisions"}, nil, nil)
	var calls atomic.Int32
	var reserved atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		app.chat.Mu.Lock()
		for id := range app.chat.Active {
			if strings.HasPrefix(id, "automation-") {
				reserved.Store(true)
			}
		}
		app.chat.Mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model":"gpt-6-luna","answers":[{"type":"choice","name":"identity","choice":"pass","confidence":0.9,"probabilities":[{"value":"pass","probability":0.95},{"value":"uncertain","probability":0.05}]}],"usage":{"input_tokens":100,"output_tokens":0}}`))
	}))
	defer upstream.Close()
	cfg := map[string]any{"limits": map[string]int{"maxConcurrent": 1}, "defaultConnection": "local", "connections": []any{map[string]any{"id": "local", "label": "Fixture", "format": "responses", "baseURL": upstream.URL, "models": []string{"gpt-6-luna", "gpt-6-mini"}, "defaultModel": "gpt-6-luna", "local": true}}}
	raw, _ := json.Marshal(cfg)
	file := filepath.Join(t.TempDir(), "chat.json")
	os.WriteFile(file, raw, 0600)
	if err := app.ConfigureChat(file); err != nil {
		t.Fatal(err)
	}
	snapshot := app.chat.Snapshot()
	connection := snapshot.Config.Connections[0]
	if _, err := app.chat.Admit(snapshot.Config, connection, "occupied", func() {}, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	// Closing the runtime waits for the held slot, so release it on every path.
	var once sync.Once
	release := func() { once.Do(func() { app.chat.Finish("occupied", connection.ID) }) }
	t.Cleanup(release)
	args, _ := json.Marshal(map[string]any{"prompt": "Synthetic evidence", "questions": []any{map[string]any{"type": "choice", "name": "identity", "instructions": "Is identity visible?", "choices": []any{map[string]string{"value": "pass", "description": "Identity is visible"}, map[string]string{"value": "uncertain", "description": "Identity is unclear"}}}}})
	base := e.ctx
	decide := func(step string) error {
		t.Helper()
		ctx, cancel := context.WithTimeout(base, 5*time.Second)
		defer cancel()
		e.ctx = ctx
		_, err := app.scriptDecisions(e, scriptRequest{Step: step, Operation: "ai.decisions", Arguments: args})
		if calls.Load() != 0 {
			t.Fatalf("%s: provider contacted", step)
		}
		return err
	}
	e.run.Spec.AIConnection = "local"
	e.run.Spec.ReadProjects = []string{"missing"}
	e.run.Spec.Model = "unlisted"
	e.run.Spec.MaxTokens = 1
	policy, err := app.catalog.SetAISettings(catalog.Bootstrap, catalog.AISettings{})
	if err != nil {
		t.Fatal(err)
	}
	if err := decide("policy"); !errors.Is(err, errInstanceAI) {
		t.Fatalf("policy: %v", err)
	}
	policy.Writing = true
	if _, err = app.catalog.SetAISettings(catalog.Bootstrap, policy); err != nil {
		t.Fatal(err)
	}
	_, scope := app.clientAccess(base, e.run.Spec.Actor, "missing", "read")
	if err := decide("scope"); scope == nil || err == nil || err.Error() != scope.Error() {
		t.Fatalf("scope: %v, want %v", err, scope)
	}
	e.run.Spec.ReadProjects = nil
	if err := decide("provider"); err == nil || err.Error() != "automation model is not configured" {
		t.Fatalf("provider: %v", err)
	}
	e.run.Spec.Model = "gpt-6-mini"
	if err := decide("estimate"); err == nil || err.Error() != "Decisions requires configured gpt-6-luna Responses connection" {
		t.Fatalf("estimate: %v", err)
	}
	e.run.Spec.Model = ""
	if err := decide("budget"); err == nil || err.Error() != "automation token budget reached" {
		t.Fatalf("budget: %v", err)
	}
	release()
	e.run.Spec.MaxTokens = 0
	e.ctx = base
	if _, err := app.scriptDecisions(e, scriptRequest{Step: "decide", Operation: "ai.decisions", Arguments: args}); err != nil {
		t.Fatal(err)
	}
	app.chat.Mu.Lock()
	active := len(app.chat.Active)
	app.chat.Mu.Unlock()
	if calls.Load() != 1 || !reserved.Load() || active != 0 || e.run.InputTokens != 100 {
		t.Fatalf("calls=%d reserved=%v active=%d input=%d", calls.Load(), reserved.Load(), active, e.run.InputTokens)
	}
}
