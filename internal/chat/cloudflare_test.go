package chat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloudflareModerationDecisionsAndSearch(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Error("missing private credential")
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/ai/websearch/":
			if r.Header.Get("cf-aig-collect-log") != "false" || r.Header.Get("cf-aig-skip-cache") != "true" {
				t.Error("transient search logging/cache controls missing")
			}
			w.Write([]byte(`{"items":[{"url":"https://signs.example.com/","title":"transient","description":"must not persist"}]}`))
		case "/ai/run/@cf/cloudflare/clef":
			if r.Header.Get("cf-aig-gateway-id") != "moderation-credits" {
				t.Error("Clef must use the configured prepaid gateway")
			}
			if body["model"] != "clef" || body["state"] != "Original evidence" {
				t.Error("wrong native decision contract")
			}
			w.Write([]byte(`{"success":true,"result":{"model":"clef","answers":{"identity":{"type":"choice","choice":"pass","confidence":0.9,"probabilities":{"pass":0.95,"uncertain":0.05}}},"usage":{"input_tokens":100,"output_tokens":0}}}`))
		default:
			t.Error("unexpected provider request", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	secret := filepath.Join(t.TempDir(), "token")
	os.WriteFile(secret, []byte("synthetic-token"), 0600)
	account := filepath.Join(t.TempDir(), "account")
	os.WriteFile(account, []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), 0600)
	raw := `{"id":"cloudflare","label":"Moderation","format":"cloudflare-moondream","baseURL":"` + server.URL + `/ai","local":true,"models":["@cf/moondream/moondream3.1-9B-A2B"],"defaultModel":"@cf/moondream/moondream3.1-9B-A2B","secretFile":"` + secret + `","capabilities":{"images":true},"cloudflare":{"accountIDFile":"` + account + `","decisionModel":"@cf/cloudflare/clef","gatewayID":"moderation-credits","searchProvider":"ceramic"}}`
	var connection Connection
	if err := json.Unmarshal([]byte(raw), &connection); err != nil {
		t.Fatal(err)
	}
	p, err := NewProvider(connection)
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Decide(t.Context(), DecisionsRequest{Prompt: "Original evidence", Questions: []DecisionQuestion{{Type: "choice", Name: "identity", Instructions: "Matches?", Choices: []DecisionChoice{{Value: "pass", Description: "Matches"}, {Value: "uncertain", Description: "Unknown"}}}}})
	if err != nil || result.Model != "@cf/cloudflare/clef" || result.Answers[0].Choice != "pass" || result.Usage.Estimated || result.Usage.InputTokens != 100 || calls != 1 {
		t.Fatal(result, err, calls)
	}
	found, err := p.CloudflareSearch(t.Context(), "Signs official website", 1)
	if err != nil || len(found) != 1 || found[0].URL != "https://signs.example.com/" || found[0].Title != "" || found[0].Description != "" || calls != 2 {
		t.Fatal(found, err, calls)
	}

}

func TestCloudflareClefBatchesPreserveCompleteState(t *testing.T) {
	calls := 0
	failLater := false
	state := strings.Repeat("Ordinary English business evidence. ", 1650)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			State     string         `json:"state"`
			Questions map[string]any `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.State != state {
			t.Error("authoritative state was truncated")
		}
		if failLater && calls == 2 {
			w.Write([]byte(`{"success":true,"result":{}}`))
			return
		}
		answers := map[string]any{}
		for name := range body.Questions {
			answers[name] = map[string]any{"type": "choice", "choice": "pass", "confidence": 1, "probabilities": map[string]int{"pass": 1, "uncertain": 0}}
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"model": "clef", "answers": answers, "usage": map[string]int{"input_tokens": 12000, "output_tokens": 0}}})
	}))
	defer server.Close()
	token := filepath.Join(t.TempDir(), "token")
	os.WriteFile(token, []byte("synthetic"), 0600)
	c := providerTestConnection(server.URL+"/ai", "cloudflare-moondream")
	c.SecretFile = token
	c.Models, c.DefaultModel = []string{moondreamModel}, moondreamModel
	c.Capabilities = Capabilities{Images: true}
	c.Cloudflare = &CloudflareConfig{AccountIDFile: token, DecisionModel: "@cf/cloudflare/clef", GatewayID: "default", SearchProvider: "ceramic"}
	p, err := NewProvider(c)
	if err != nil {
		t.Fatal(err)
	}
	req := DecisionsRequest{Prompt: state}
	for i := 0; i < 38; i++ {
		req.Questions = append(req.Questions, DecisionQuestion{Type: "choice", Name: fmt.Sprintf("rule_%d", i), Instructions: strings.Repeat("Evaluate the complete original evidence. ", 20), Choices: []DecisionChoice{{Value: "pass", Description: "Supported"}, {Value: "uncertain", Description: "Unknown"}}})
	}
	if _, err = p.EstimateDecisions(req); err != nil {
		t.Fatal("default multi-page evidence should fit by batching", err)
	}
	out, err := p.Decide(t.Context(), req)
	if err != nil || calls < 2 || len(out.Answers) != 38 || out.Usage.Estimated || out.Usage.InputTokens != int64(calls*12000) {
		t.Fatal(out, err, calls)
	}
	for i, a := range out.Answers {
		if a.Name != req.Questions[i].Name {
			t.Fatal("question order changed")
		}
	}
	calls, failLater = 0, true
	out, err = p.Decide(t.Context(), req)
	if err == nil || len(out.Answers) != 0 || calls != 2 || !out.Usage.Estimated {
		t.Fatal("partial answers must not escape", out, err, calls)
	}
	calls = 0
	req.Prompt = strings.Repeat("x", 65536)
	if _, err = p.Decide(t.Context(), req); err == nil || calls != 0 {
		t.Fatal("unfit state bought inference", err, calls)
	}
	req.Prompt = "Small state"
	req.Questions[1].Instructions = strings.Repeat("x", 65536)
	if _, err = p.Decide(t.Context(), req); err == nil || calls != 0 {
		t.Fatal("unfit later question bought inference", err, calls)
	}
	req.Questions = make([]DecisionQuestion, 65)
	if _, err = p.Decide(t.Context(), req); err == nil || calls != 0 {
		t.Fatal("outer question bound expanded", err, calls)
	}
}

func TestCloudflareSearchDiagnosticsKeepOnlyOperationalEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("CF-Ray", "abcdef1234567890-SJC")
		w.Write([]byte(`{"items":[],"secret":"must not retain","description":"must not retain"}`))
	}))
	defer server.Close()
	token := filepath.Join(t.TempDir(), "token")
	os.WriteFile(token, []byte("synthetic"), 0600)
	c := providerTestConnection(server.URL+"/ai", "cloudflare-mistral")
	c.SecretFile = token
	c.Capabilities = Capabilities{Images: true}
	c.Models = []string{mistralModel}
	c.DefaultModel = mistralModel
	c.Cloudflare = &CloudflareConfig{AccountIDFile: token, DecisionModel: "@cf/cloudflare/clef", GatewayID: "default", SearchProvider: "ceramic"}
	p, err := NewProvider(c)
	if err != nil {
		t.Fatal(err)
	}
	result, diagnostics, err := p.CloudflareSearchDiagnostics(t.Context(), "Strap Tank Springville", 3)
	if err != nil || len(result) != 0 || diagnostics.Status != 200 || diagnostics.RawItems != 0 || diagnostics.AcceptedURLs != 0 || diagnostics.RequestID != "abcdef1234567890-SJC" {
		t.Fatal(result, diagnostics, err)
	}
	raw, _ := json.Marshal(diagnostics)
	if strings.Contains(string(raw), "must not retain") {
		t.Fatal(string(raw))
	}
}
