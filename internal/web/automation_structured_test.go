package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestScriptMoondreamCorrectsJSONOnceAndRetainsUsage(t *testing.T) {
	for _, model := range []struct{ format, name string }{
		{"cloudflare-moondream", "@cf/moondream/moondream3.1-9B-A2B"},
		{"cloudflare-mistral", "@cf/mistralai/mistral-small-3.1-24b-instruct"},
	} {
		for _, scenario := range []struct {
			name, first, second string
			success             bool
			calls               int32
		}{
			{"correction", `{"type":"object","properties":{"value":{"type":"integer"}}}`, `{"value":7}`, true, 2},
			{"schema-echo", `{"type":"object"}`, `{"type":"object"}`, false, 2},
			{"json-fence", "```json\n{\"value\":7}\n```", "", true, 1},
			{"prose-fence", "Answer:\n```json\n{\"value\":7}\n```", "invalid", false, 2},
			{"invalid-fenced-type", "```json\n{\"value\":\"7\"}\n```", "invalid", false, 2},
			{"multiple-fences", "```json\n{\"value\":7}\n```\n```json\n{\"value\":8}\n```", "invalid", false, 2},
		} {
			t.Run(model.format+"/"+scenario.name, func(t *testing.T) {
				app, e := scriptFixture(t, "def main(host): return {}", []string{"ai.structured", "ai.images"}, nil, nil)
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					// Count only this connection's inference calls. Another
					// process can reach a loopback port, such as a browser
					// test harness whose app's port this server was given.
					if r.Method != http.MethodPost || r.URL.Path != "/ai/run/"+model.name || r.Header.Get("Authorization") != "Bearer synthetic-token" {
						http.NotFound(w, r)
						return
					}
					n := calls.Add(1)
					answer := scenario.first
					if n == 2 {
						answer = scenario.second
					}
					result := map[string]any{"result": map[string]any{"answer": answer, "finish_reason": "stop", "metrics": map[string]int{"input_tokens": 10, "output_tokens": 5}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5}}
					if model.format == "cloudflare-mistral" {
						var request map[string]any
						if json.NewDecoder(r.Body).Decode(&request) != nil || request["guided_json"] == nil {
							t.Error("caller schema did not reach Mistral")
						}
						result = map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": answer}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5}}
					}
					raw, _ := json.Marshal(map[string]any{"success": true, "result": result})
					w.Write(raw)
				}))
				defer upstream.Close()
				secret := filepath.Join(t.TempDir(), "token")
				os.WriteFile(secret, []byte("synthetic-token"), 0600)
				cfg := map[string]any{"defaultConnection": "local", "connections": []any{map[string]any{"id": "local", "label": "Fixture", "format": model.format, "baseURL": upstream.URL + "/ai", "models": []string{model.name}, "defaultModel": model.name, "local": true, "secretFile": secret, "capabilities": map[string]bool{"images": true}, "limits": map[string]int{"contextTokens": 32768}, "cloudflare": map[string]string{"accountIDFile": secret, "decisionModel": "@cf/cloudflare/clef", "gatewayID": "default", "searchProvider": "ceramic"}}}}
				raw, _ := json.Marshal(cfg)
				file := filepath.Join(t.TempDir(), "chat.json")
				os.WriteFile(file, raw, 0600)
				if err := app.ConfigureChat(file); err != nil {
					t.Fatal(err)
				}
				e.run.Spec.AIConnection = "local"
				var contractErr error
				e.run.Script.Contract, contractErr = app.automationrun.ScriptContract(e.run.Spec)
				if contractErr != nil {
					t.Fatal(contractErr)
				}
				args := map[string]any{"prompt": "Extract synthetic visible values only.", "schema": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "integer"}}, "required": []string{"value"}, "additionalProperties": false}, "images": []string{"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl2nGAAAAAASUVORK5CYII="}}
				_, err := hostStep(t, app, e, "extract", "ai.structured", args)
				if (err == nil) != scenario.success || calls.Load() != scenario.calls || e.run.InputTokens != int(scenario.calls)*10 || e.run.OutputTokens != int(scenario.calls)*5 {
					t.Fatal("correction must be bounded, validated and fully accounted", calls.Load(), e.run.InputTokens, e.run.OutputTokens, err)
				}
				_, repeated := hostStep(t, app, e, "extract", "ai.structured", args)
				if (repeated == nil) != scenario.success || calls.Load() != scenario.calls || e.run.Script.Uncertain() {
					t.Fatal("settled analysis replay must not repeat inference or require manual reconciliation", calls.Load(), repeated)
				}
				if !scenario.success && !strings.Contains(err.Error(), "JSON") {
					t.Fatal("missing output diagnostic", err)
				}
			})
		}
	}
}
