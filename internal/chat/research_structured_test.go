package chat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStructuredResearchRequiresCompletedSearchAndRetainsPaidUsage(t *testing.T) {
	for _, mode := range []string{"valid", "missing-search", "too-many", "incomplete", "refusal", "invalid-json", "missing-usage", "private-open"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				search := map[string]any{"type": "web_search_call", "status": "completed", "action": map[string]any{"type": "search", "sources": []any{map[string]string{"url": "https://official.example.org"}, map[string]string{"url": "http://127.0.0.1/private"}}}}
				message := map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": `{"name":"Shop"}`}}}
				body := map[string]any{"status": "completed", "usage": map[string]int{"input_tokens": 120, "output_tokens": 10}, "output": []any{search, message}}
				switch mode {
				case "missing-search":
					body["output"] = []any{message}
				case "too-many":
					body["output"] = []any{search, search, message}
				case "incomplete":
					body["status"] = "incomplete"
				case "refusal":
					message["content"] = []any{map[string]string{"type": "refusal", "refusal": "Refused"}}
				case "invalid-json":
					message["content"] = []any{map[string]string{"type": "output_text", "text": "not JSON"}}
				case "missing-usage":
					delete(body, "usage")
				case "private-open":
					body["output"] = []any{search, map[string]any{"type": "web_search_call", "status": "completed", "action": map[string]string{"type": "open_page", "url": "http://127.0.0.1/private"}}, message}
				}
				json.NewEncoder(w).Encode(body)
			}))
			defer upstream.Close()
			p, err := NewProvider(Connection{ID: "fixture", Label: "Fixture", Format: "responses", BaseURL: upstream.URL, Models: []string{"gpt-6-luna"}, DefaultModel: "gpt-6-luna", Local: true, Capabilities: Capabilities{Images: true}, Limits: Limits{ContextTokens: 65536}})
			if err != nil {
				t.Fatal(err)
			}
			research, err := NewConnectionResearch(ResearchConfig{Enabled: true, Format: "openai", Connection: "fixture"}, p)
			if err != nil {
				t.Fatal(err)
			}
			result, err := research.StructuredUsage(t.Context(), StructuredResearchRequest{Prompt: "Return JSON.", Schema: json.RawMessage(`{"type":"object"}`), Images: []string{"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/l9sAAAAASUVORK5CYII="}, MaxSearches: 1})
			if mode == "valid" {
				if err != nil || len(result.Sources) != 1 || result.SearchCalls != 1 || string(result.Value) != `{"name":"Shop"}` {
					t.Fatal(result, err)
				}
			} else if err == nil {
				t.Fatal("invalid response accepted", result)
			}
			if calls != 1 {
				t.Fatal("provider request repeated", calls)
			}
			if mode == "missing-usage" {
				if !result.Usage.Estimated || result.Usage.InputTokens < 16000 {
					t.Fatal("unknown usage lost its reservation", result.Usage)
				}
			} else if result.Usage.Estimated || result.Usage.InputTokens != 120 || result.Usage.OutputTokens != 10 {
				t.Fatal("paid usage lost after rejected result", result.Usage)
			}
		})
	}
}
