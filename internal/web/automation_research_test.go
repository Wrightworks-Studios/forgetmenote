package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestScriptStructuredResearchUsesImageAndRequiredSearch(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"ai.structured", "ai.images", "web.search"}, nil, nil)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/responses" || body["tools"] == nil || body["tool_choice"] != "required" || body["max_tool_calls"] != float64(1) || body["store"] != false {
			t.Error("image extraction did not require a bounded search")
		}
		format := body["text"].(map[string]any)["format"].(map[string]any)
		if format["type"] != "json_schema" || format["strict"] != false || format["schema"] == nil {
			// The live API rejects JSON mode combined with web search.
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		input := body["input"].([]any)[0].(map[string]any)["content"].([]any)
		image := false
		for _, part := range input {
			image = image || part.(map[string]any)["type"] == "input_image"
		}
		if !image {
			t.Error("search lost the original creative pixels")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "completed", "output": []any{
			map[string]any{"type": "web_search_call", "status": "completed", "action": map[string]any{"type": "search", "query": "Fixture official identity", "sources": []any{map[string]string{"type": "url", "url": "https://example.org/original"}}}},
			map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": `{"identity":"fixture"}`}}},
		}, "usage": map[string]int{"input_tokens": 100, "output_tokens": 10}})
	}))
	defer upstream.Close()
	cfg := map[string]any{"defaultConnection": "local", "connections": []any{map[string]any{"id": "local", "label": "Fixture", "format": "responses", "baseURL": upstream.URL, "models": []string{"gpt-6-luna"}, "defaultModel": "gpt-6-luna", "local": true, "capabilities": map[string]bool{"images": true}, "limits": map[string]int{"contextTokens": 65536}}}, "research": map[string]any{"enabled": true, "format": "openai", "connection": "local", "maxSearches": 1, "maxFetches": 4}}
	raw, _ := json.Marshal(cfg)
	file := filepath.Join(t.TempDir(), "chat.json")
	os.WriteFile(file, raw, 0600)
	if err := app.ConfigureChat(file); err != nil {
		t.Fatal(err)
	}
	e.run.Spec.AIConnection = "local"
	e.run.DryRun = true
	e.run.Script.Contract, _ = app.automationrun.ScriptContract(e.run.Spec)
	args := map[string]any{"prompt": "Extract visible identity and search for matching original sources. Return JSON.", "schema": map[string]any{"type": "object", "properties": map[string]any{"identity": map[string]string{"type": "string"}}, "required": []string{"identity"}, "additionalProperties": false}, "images": []string{"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/l9sAAAAASUVORK5CYII="}, "research": map[string]int{"maxSearches": 1}}
	got, err := hostStep(t, app, e, "discover", "ai.structured", args)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	var result struct {
		Value   map[string]string `json:"value"`
		Sources []struct {
			URL string `json:"url"`
		} `json:"sources"`
		SearchCalls int `json:"searchCalls"`
	}
	json.Unmarshal(b, &result)
	if result.Value["identity"] != "fixture" || result.SearchCalls != 1 || len(result.Sources) != 1 || result.Sources[0].URL != "https://example.org/original" || e.run.InputTokens != 100 || e.run.OutputTokens != 10 {
		t.Fatalf("structured research lost value, sources or paid usage: %s", b)
	}
	if _, err = hostStep(t, app, e, "discover", "ai.structured", args); err != nil || calls != 1 {
		t.Fatal("paid preview replay repeated research", calls, err)
	}
	if _, err = hostStep(t, app, e, "exhausted", "web.search", map[string]any{"query": "identity", "limit": 1}); err == nil || calls != 1 {
		t.Fatal("combined research bypassed the shared search budget", calls, err)
	}
	e.run.Spec.Script.Capabilities = []string{"ai.structured", "ai.images"}
	if _, err = hostStep(t, app, e, "discover", "ai.structured", args); err == nil {
		t.Fatal("revoked web search grant released the retained result")
	}
	e.run.Spec.Script.Capabilities = []string{"ai.structured", "ai.images", "web.search"}
	delete(cfg, "research")
	raw, _ = json.Marshal(cfg)
	os.WriteFile(file, raw, 0600)
	if err = app.ConfigureChat(file); err != nil {
		t.Fatal(err)
	}
	if _, err = hostStep(t, app, e, "discover", "ai.structured", args); err == nil {
		t.Fatal("disabled research released a retained research result")
	}
}

func TestScriptStructuredResearchRejectsUnpaidBoundaryViolations(t *testing.T) {
	for _, mode := range []string{"token-budget", "search-budget", "missing-image", "cache", "disabled", "connection", "schema", "invalid-result"} {
		t.Run(mode, func(t *testing.T) {
			app, e := scriptFixture(t, "def main(host): return {}", []string{"ai.structured", "ai.images", "web.search"}, nil, nil)
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if mode != "invalid-result" {
					t.Error("invalid research reached the paid boundary")
					return
				}
				w.Write([]byte(`{"status":"completed","usage":{"input_tokens":120,"output_tokens":10},"output":[{"type":"web_search_call","status":"completed","action":{"type":"search","sources":[{"url":"https://official.example.org"}]}},{"type":"message","content":[{"type":"output_text","text":"not JSON"}]}]}`))
			}))
			defer upstream.Close()
			connection := map[string]any{"id": "local", "label": "Fixture", "format": "responses", "baseURL": upstream.URL, "models": []string{"gpt-6-luna"}, "defaultModel": "gpt-6-luna", "local": true, "capabilities": map[string]bool{"images": true}, "limits": map[string]int{"contextTokens": 65536}}
			cfg := map[string]any{"defaultConnection": "local", "connections": []any{connection}, "research": map[string]any{"enabled": true, "format": "openai", "connection": "local", "maxSearches": 1, "maxFetches": 4}}
			if mode == "disabled" {
				delete(cfg, "research")
			}
			if mode == "connection" {
				cfg["connections"] = []any{connection, map[string]any{"id": "other", "label": "Other", "format": "responses", "baseURL": upstream.URL, "models": []string{"gpt-6-luna"}, "defaultModel": "gpt-6-luna", "local": true}}
			}
			raw, _ := json.Marshal(cfg)
			file := filepath.Join(t.TempDir(), "chat.json")
			os.WriteFile(file, raw, 0600)
			if err := app.ConfigureChat(file); err != nil {
				t.Fatal(err)
			}
			e.run.Spec.AIConnection = "local"
			args := map[string]any{"prompt": "Return JSON.", "schema": map[string]any{"type": "object"}, "images": []string{"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/l9sAAAAASUVORK5CYII="}, "research": map[string]int{"maxSearches": 1}}
			switch mode {
			case "token-budget":
				e.run.Spec.MaxTokens = 1
			case "search-budget":
				args["research"] = map[string]int{"maxSearches": 2}
			case "missing-image":
				delete(args, "images")
			case "cache":
				args["cacheKey"] = "reuse"
			case "connection":
				e.run.Spec.AIConnection = "other"
			case "schema":
				args["schema"] = map[string]string{"$ref": "#/missing"}
			}
			e.run.Script.Contract, _ = app.automationrun.ScriptContract(e.run.Spec)
			wantCalls := 0
			if mode == "invalid-result" {
				wantCalls = 1
			}
			if _, err := hostStep(t, app, e, "invalid", "ai.structured", args); err == nil || calls != wantCalls {
				t.Fatal("invalid research accepted", calls, err)
			}
			if mode == "invalid-result" {
				var retained struct {
					SearchCalls int   `json:"searchCalls"`
					Sources     []any `json:"sources"`
				}
				json.Unmarshal(e.run.Script.Steps["invalid"].Result, &retained)
				if retained.SearchCalls != 1 || len(retained.Sources) != 1 || e.run.InputTokens != 120 || e.run.OutputTokens != 10 {
					t.Fatal("rejected research lost its private audit receipt", retained)
				}
				if _, err := hostStep(t, app, e, "invalid", "ai.structured", args); err == nil || calls != 1 {
					t.Fatal("rejected paid search repeated", calls, err)
				}
			}
		})
	}
}
