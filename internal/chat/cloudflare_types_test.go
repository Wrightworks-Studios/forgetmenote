package chat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCloudflareStructuredCriteriaAndNativeTypes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Questions map[string]struct {
				Type         string `json:"type"`
				Instructions string `json:"instructions"`
				Criteria     any    `json:"criteria"`
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		answers := map[string]any{}
		for name, q := range body.Questions {
			switch q.Type {
			case "choice":
				if _, ok := q.Criteria.(map[string]any)["red"].(map[string]any); !ok {
					t.Error("object criteria lost")
				}
				answers[name] = map[string]any{"type": "choice", "choice": "red", "confidence": .8, "probabilities": map[string]float64{"red": .9, "blue": .1}}
			case "noul":
				answers[name] = map[string]any{"type": "noul", "noul": .7}
			case "score":
				answers[name] = map[string]any{"type": "score", "score": 1.6, "confidence": .4, "probabilities": map[string]float64{"0": .1, "1": .2, "2": .7}, "legend": map[string]string{"0": "low", "1": "middle", "2": "high"}}
			}
			if q.Instructions == "refuse" {
				answers[name] = map[string]any{"type": "refusal"}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{"model": "clef", "answers": answers, "usage": map[string]int{"input_tokens": 20, "output_tokens": 0}}})
	}))
	defer server.Close()
	dir := t.TempDir()
	secret := filepath.Join(dir, "token")
	account := filepath.Join(dir, "account")
	os.WriteFile(secret, []byte("synthetic"), 0600)
	os.WriteFile(account, []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), 0600)
	p, err := NewProvider(Connection{ID: "cf", Label: "test", Format: "cloudflare-mistral", BaseURL: server.URL, Local: true, Capabilities: Capabilities{Images: true}, SecretFile: secret, Models: []string{"@cf/mistralai/mistral-small-3.1-24b-instruct"}, DefaultModel: "@cf/mistralai/mistral-small-3.1-24b-instruct", Cloudflare: &CloudflareConfig{AccountIDFile: account, DecisionModel: "@cf/cloudflare/clef", GatewayID: "test", SearchProvider: "ceramic"}})
	if err != nil {
		t.Fatal(err)
	}
	questions := []DecisionQuestion{{Type: "choice", Name: "color", Instructions: "color?", Choices: []DecisionChoice{{Value: "red", Description: map[string]any{"meaning": "red"}}, {Value: "blue", Description: "blue"}}}, {Type: "noul", Name: "present", Instructions: "present?"}, {Type: "score", Name: "level", Instructions: "level?", Criteria: []any{"low", "middle", "high"}}, {Type: "noul", Name: "refused", Instructions: "refuse"}}
	result, err := p.Decide(t.Context(), DecisionsRequest{Prompt: "red", Questions: questions})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Answers) != 4 || result.Answers[0].Choice != "red" || result.Answers[1].Noul == nil || *result.Answers[1].Noul != .7 || result.Answers[2].Score == nil || *result.Answers[2].Score != 1.6 || result.Answers[3].Type != "refusal" || result.Batches != 1 {
		t.Fatal(result)
	}
}
