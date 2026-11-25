package chat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAIResearchUsesBoundedConfiguredConnection(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/responses" || body["max_tool_calls"] != float64(1) || body["tool_choice"] != "required" || body["store"] != false {
			t.Error("unbounded search request")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"completed","output":[{"type":"web_search_call","status":"completed","action":{"type":"search","sources":[{"type":"url","url":"https://example.org/original"},{"type":"url","url":"http://127.0.0.1/private"}]}},{"type":"message","content":[{"type":"output_text","text":"Untrusted generated summary","annotations":[{"type":"url_citation","url":"https://example.org/original","title":"Original"}]}]}],"usage":{"input_tokens":90,"output_tokens":10}}`))
	}))
	defer srv.Close()
	p, err := NewProvider(Connection{ID: "fixture", Label: "Fixture", Format: "responses", BaseURL: srv.URL, Models: []string{"gpt-6-luna"}, DefaultModel: "gpt-6-luna", Local: true})
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewConnectionResearch(ResearchConfig{Enabled: true, Format: "openai", Connection: "fixture"}, p)
	if err != nil {
		t.Fatal(err)
	}
	got, usage, err := r.SearchUsage(t.Context(), "synthetic identity", 3)
	if err != nil || len(got) != 1 || got[0].URL != "https://example.org/original" || usage.InputTokens != 90 || usage.OutputTokens != 10 || calls != 1 {
		t.Fatalf("search %+v %+v %v", got, usage, err)
	}
	if got[0].Description == "Untrusted generated summary" {
		t.Fatal("generated prose became evidence")
	}
	if _, _, err = r.SearchUsage(t.Context(), "", 3); err == nil || calls != 1 {
		t.Fatal("invalid query escaped boundary")
	}
}

func TestOpenAIResearchConfigurationRejectsImplicitAndMixedCredentials(t *testing.T) {
	for _, mode := range []string{"valid", "unknown", "wrong-format", "implicit", "mixed", "local-only"} {
		t.Run(mode, func(t *testing.T) {
			c := Config{Connections: []Connection{{ID: "openai", Label: "Fixture", Format: "responses", BaseURL: "https://api.openai.com/v1", Models: []string{"gpt-6-luna"}, DefaultModel: "gpt-6-luna"}}, Research: &ResearchConfig{Enabled: true, Format: "openai", Connection: "openai", MaxSearches: 2, MaxFetches: 4}}
			switch mode {
			case "unknown":
				c.Research.Connection = "other"
			case "wrong-format":
				c.Connections[0].Format = "chat-completions"
			case "implicit":
				c.Research.Connection = ""
			case "mixed":
				c.Research.SecretFile = "/private/key"
			case "local-only":
				c.LocalOnly = true
				c.Connections[0].Local = true
				c.Connections[0].BaseURL = "http://127.0.0.1:9999"
			}
			if err := c.Validate(); (err == nil) != (mode == "valid") {
				t.Fatalf("%s: %v", mode, err)
			}
		})
	}
}
