package chat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloudflareDecisionModelSelection(t *testing.T) {
	for _, model := range []string{"clef", "clef-omni"} {
		t.Run(model, func(t *testing.T) {
			calls := 0
			responseModel := model
			prompt := "Original evidence"
			picture := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/l9sAAAAASUVORK5CYII="
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/ai/run/@cf/cloudflare/"+model {
					t.Errorf("request used an unselected model endpoint: %s", r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer synthetic-token" || r.Header.Get("cf-aig-gateway-id") != "moderation" {
					t.Error("request lost the configured credential or gateway")
				}
				var body struct {
					Model  string   `json:"model"`
					State  string   `json:"state"`
					Images []string `json:"images"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if body.Model != model || body.State != prompt || len(body.Images) != 1 || body.Images[0] != picture {
					t.Error("native request changed the selected model or original evidence")
				}
				json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{
					"model": responseModel,
					"answers": map[string]any{"identity": map[string]any{"type": "choice", "choice": "pass", "confidence": .9,
						"probabilities": map[string]float64{"pass": .95, "uncertain": .05}}},
					"usage": map[string]int{"input_tokens": 100, "output_tokens": 0},
				}})
			}))
			defer server.Close()
			secret := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(secret, []byte("synthetic-token"), 0600); err != nil {
				t.Fatal(err)
			}
			connection := providerTestConnection(server.URL+"/ai", "cloudflare-mistral")
			connection.SecretFile = secret
			connection.Models, connection.DefaultModel = []string{mistralModel}, mistralModel
			connection.Capabilities = Capabilities{Images: true}
			connection.Cloudflare = &CloudflareConfig{AccountIDFile: secret, DecisionModel: "@cf/cloudflare/" + model, GatewayID: "moderation", SearchProvider: "ceramic"}
			provider, err := NewProvider(connection)
			if err != nil {
				t.Fatal(err)
			}
			request := DecisionsRequest{Prompt: prompt, Images: []string{picture}, Questions: []DecisionQuestion{{Type: "choice", Name: "identity", Instructions: "Does the evidence identify the advertiser?", Choices: []DecisionChoice{{Value: "pass", Description: "Identified"}, {Value: "uncertain", Description: "Not established"}}}}}
			result, err := provider.Decide(t.Context(), request)
			if err != nil || result.Model != connection.Cloudflare.DecisionModel || len(result.Answers) != 1 || result.Answers[0].Choice != "pass" || result.Usage.Estimated || result.Usage.InputTokens != 100 || calls != 1 {
				t.Fatal(result, err, calls)
			}
			responseModel = "clef"
			if model == "clef" {
				responseModel = "clef-omni"
			}
			result, err = provider.Decide(t.Context(), request)
			if err == nil || len(result.Answers) != 0 || calls != 2 {
				t.Fatal("a different model response must fail without a fallback request", result, err, calls)
			}
			responseModel = model
			prompt = strings.Repeat("x", 63500)
			request.Prompt = prompt
			_, estimateErr := provider.EstimateDecisions(request)
			result, err = provider.Decide(t.Context(), request)
			if model == "clef-omni" {
				if estimateErr == nil || err == nil || calls != 2 || len(result.Answers) != 0 {
					t.Fatal("Omni's 64,000-token bound must reject before inference", result, estimateErr, err, calls)
				}
			} else if estimateErr != nil || err != nil || calls != 3 {
				t.Fatal("Clef's existing 65,536-token capacity must remain available", result, estimateErr, err, calls)
			}
			for _, unsupported := range []string{"@cf/cloudflare/clef-flash", "@cf/cloudflare/clef-omni-extra"} {
				connection.Cloudflare.DecisionModel = unsupported
				if _, err := NewProvider(connection); err == nil {
					t.Errorf("unsupported decision model accepted: %s", unsupported)
				}
			}
		})
	}
}
