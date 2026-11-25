package chat

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecisionsRejectInvalidAnswersAndRetainPaidUsage(t *testing.T) {
	for _, mode := range []string{"valid", "refusal", "missing-usage", "missing-confidence", "wrong-name", "unknown-choice", "duplicate-probability", "negative-probability", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if mode == "redirect" {
					w.Header().Set("Location", "/secret-target")
					w.WriteHeader(307)
					w.Write([]byte("private credential"))
					return
				}
				a := map[string]any{"type": "choice", "name": "rule", "choice": "pass", "confidence": .8, "probabilities": []any{map[string]any{"value": "pass", "probability": .8}, map[string]any{"value": "uncertain", "probability": .2}}}
				switch mode {
				case "refusal":
					a = map[string]any{"type": "refusal", "name": "rule"}
				case "missing-confidence":
					delete(a, "confidence")
				case "wrong-name":
					a["name"] = "other"
				case "unknown-choice":
					a["choice"] = "invented"
				case "duplicate-probability":
					a["probabilities"] = []any{map[string]any{"value": "pass", "probability": .5}, map[string]any{"value": "pass", "probability": .5}}
				case "negative-probability":
					a["probabilities"] = []any{map[string]any{"value": "pass", "probability": 1.2}, map[string]any{"value": "uncertain", "probability": -.2}}
				}
				b := map[string]any{"model": "gpt-6-luna", "answers": []any{a}, "usage": map[string]any{"input_tokens": 123, "output_tokens": 0}}
				if mode == "missing-usage" {
					delete(b, "usage")
				}
				json.NewEncoder(w).Encode(b)
			}))
			defer srv.Close()
			p, err := NewProvider(Connection{ID: "fixture", Label: "Fixture", Format: "responses", BaseURL: srv.URL, Models: []string{"gpt-6-luna"}, DefaultModel: "gpt-6-luna", Local: true})
			if err != nil {
				t.Fatal(err)
			}
			request := DecisionsRequest{Prompt: "Untrusted synthetic pixels", Questions: []DecisionQuestion{{Type: "choice", Name: "rule", Instructions: "Assess rule", Choices: []DecisionChoice{{Value: "pass", Description: "Compliant"}, {Value: "uncertain", Description: "Missing evidence"}}}}}
			out, err := p.Decide(t.Context(), request)
			good := mode == "valid" || mode == "refusal"
			if (err == nil) != good || calls != 1 {
				t.Fatalf("%+v %v calls=%d", out, err, calls)
			}
			if mode != "redirect" && mode != "missing-usage" && out.Usage.InputTokens != 123 {
				t.Fatal("invalid answer lost paid usage")
			}
			if mode == "missing-usage" && (!out.Usage.Estimated || out.Usage.InputTokens == 0) {
				t.Fatal("unknown usage treated as free")
			}
			if err != nil && strings.Contains(err.Error(), "credential") {
				t.Fatal("diagnostic leaked provider content")
			}
			request.Images = []string{"https://example.org/remote.png"}
			if _, err = p.Decide(t.Context(), request); err == nil || calls != 1 {
				t.Fatal("ungranted remote image escaped validation")
			}
		})
	}
}

func TestDecisionsAcceptsWebsiteSectionsWithoutExpandingChatImages(t *testing.T) {
	p, err := NewProvider(Connection{ID: "fixture", Label: "Fixture", Format: "responses", BaseURL: "http://localhost:1234/v1", Models: []string{"gpt-6-luna"}, DefaultModel: "gpt-6-luna", Local: true, Capabilities: Capabilities{Images: true}, Limits: Limits{ContextTokens: 2000000}})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl2nGAAAAAASUVORK5CYII=")
	img, err := InlineImage(data)
	if err != nil {
		t.Fatal(err)
	}
	images := make([]string, 128)
	for i := range images {
		images[i] = img
	}
	req := DecisionsRequest{Prompt: "Original ad and website sections", Images: images, Questions: []DecisionQuestion{{Type: "choice", Name: "landing", Instructions: "Is the landing page clean?", Choices: []DecisionChoice{{Value: "pass", Description: "Clean"}, {Value: "uncertain", Description: "Missing evidence"}}}}}
	if _, err := p.EstimateDecisions(req); err != nil {
		t.Fatal(err)
	}
	if err := validateImages(Message{Role: "user", Images: images}, p.connection); err == nil {
		t.Fatal("ordinary chat image bound expanded")
	}
	req.Images = make([]string, 129)
	for i := range req.Images {
		req.Images[i] = img
	}
	if _, err := p.EstimateDecisions(req); err == nil {
		t.Fatal("website image bound ignored")
	}
}
