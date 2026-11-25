package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
)

func TestScriptCloudflareBatchesRecheckNotebookEvidence(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"notebooks.read", "ai.decisions"}, nil, map[string]string{"policy": "read"})
	if err := app.catalog.Create(catalog.Bootstrap, "policy", "Policy"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "policy", e.run.Spec.Actor, "read"); err != nil {
		t.Fatal(err)
	}
	e.run.Spec.ReadProjects = []string{"policy"}
	e.run.Spec.Script.Notebooks["policy"] = "policy"
	store, err := app.catalog.Access(catalog.Bootstrap, "policy", "edit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(t.Context(), "approved.md", []byte("Approved policy"), "", catalog.Bootstrap); err != nil {
		t.Fatal(err)
	}
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if err := app.catalog.Member(catalog.Bootstrap, "policy", e.run.Spec.Actor, ""); err != nil {
			t.Error(err)
		}
		var body struct {
			Questions map[string]any `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		answers := map[string]any{}
		for name := range body.Questions {
			answers[name] = map[string]any{"type": "choice", "choice": "pass", "confidence": 1, "probabilities": map[string]int{"pass": 1, "uncertain": 0}}
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"model": "clef", "answers": answers, "usage": map[string]int{"input_tokens": 1000, "output_tokens": 0}}})
	}))
	defer upstream.Close()
	token := filepath.Join(t.TempDir(), "token")
	os.WriteFile(token, []byte("synthetic"), 0600)
	cfg := map[string]any{"defaultConnection": "cf", "connections": []any{map[string]any{"id": "cf", "label": "Cloudflare", "format": "cloudflare-moondream", "local": true, "baseURL": upstream.URL + "/ai", "secretFile": token, "models": []string{"@cf/moondream/moondream3.1-9B-A2B"}, "capabilities": map[string]bool{"images": true}, "cloudflare": map[string]string{"accountIDFile": token, "decisionModel": "@cf/cloudflare/clef", "gatewayID": "default", "searchProvider": "ceramic"}}}}
	raw, _ := json.Marshal(cfg)
	config := filepath.Join(t.TempDir(), "chat.json")
	os.WriteFile(config, raw, 0600)
	if err = app.ConfigureChat(config); err != nil {
		t.Fatal(err)
	}
	if app.scriptAnalysisProvider(e.run.Spec) != nil {
		t.Fatal("global default falsely declares an explicit moderation provider")
	}
	e.run.Spec.AIConnection = "cf"
	if err = refreshScriptContract(t, app, e); err != nil {
		t.Fatal(err)
	}
	if _, err = hostStep(t, app, e, "policy", "notebooks.read", map[string]any{"notebook": "policy", "path": "approved.md"}); err != nil {
		t.Fatal(err)
	}
	questions := []any{}
	for i := 0; i < 38; i++ {
		questions = append(questions, map[string]any{"type": "choice", "name": fmt.Sprintf("rule_%d", i), "instructions": strings.Repeat("Evaluate all original evidence. ", 30), "choices": []any{map[string]string{"value": "pass", "description": "Supported"}, map[string]string{"value": "uncertain", "description": "Unknown"}}})
	}
	args := map[string]any{"prompt": strings.Repeat("Original business evidence. ", 2100), "questions": questions}
	if value, err := hostStep(t, app, e, "judgments", "ai.decisions", args); err == nil || value != nil || calls != 1 {
		t.Fatal("revocation did not stop later batches", value, err, calls)
	}
	if _, err := hostStep(t, app, e, "judgments", "ai.decisions", args); err == nil || calls != 1 {
		t.Fatal("failed paid call was repurchased", err, calls)
	}
}

func TestScriptCloudflareSearchDiagnosticsPreserveEmptySearchEvidence(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"web.search"}, nil, nil)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("CF-Ray", "abcdef1234567890-SJC")
		w.Write([]byte(`{"items":[],"description":"never retain this snippet"}`))
	}))
	defer upstream.Close()
	token := filepath.Join(t.TempDir(), "token")
	os.WriteFile(token, []byte("synthetic"), 0600)
	cfg := map[string]any{"defaultConnection": "cf", "research": map[string]any{"enabled": true, "format": "brave", "baseURL": "https://search.example.org/", "secretFile": token, "maxSearches": 2, "maxFetches": 4}, "connections": []any{map[string]any{"id": "cf", "label": "Cloudflare", "format": "cloudflare-moondream", "local": true, "baseURL": upstream.URL + "/ai", "secretFile": token, "models": []string{"@cf/moondream/moondream3.1-9B-A2B"}, "capabilities": map[string]bool{"images": true}, "cloudflare": map[string]string{"accountIDFile": token, "decisionModel": "@cf/cloudflare/clef", "gatewayID": "default", "searchProvider": "ceramic"}}}}
	raw, _ := json.Marshal(cfg)
	config := filepath.Join(t.TempDir(), "chat.json")
	os.WriteFile(config, raw, 0600)
	if err := app.ConfigureChat(config); err != nil {
		t.Fatal(err)
	}
	e.run.Spec.AIConnection = "cf"
	if err := refreshScriptContract(t, app, e); err != nil {
		t.Fatal(err)
	}
	result, err := hostStep(t, app, e, "search", "web.search", map[string]any{"query": "Strap Tank Springville", "limit": 3, "diagnostics": true})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(result)
	if !strings.Contains(string(raw), `"rawItems":0`) || !strings.Contains(string(raw), `"status":200`) || strings.Contains(string(raw), "snippet") {
		t.Fatal(string(raw))
	}
}
