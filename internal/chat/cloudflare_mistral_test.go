package chat

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCloudflareMistralNativeExtraction(t *testing.T) {
	pixels := image.NewNRGBA(image.Rect(0, 0, 768, 768))
	random := rand.New(rand.NewPCG(17, 41))
	for i := range pixels.Pix {
		pixels.Pix[i] = byte(random.Uint32())
	}
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, pixels); err != nil {
		t.Fatal(err)
	}
	original, err := InlineImage(pngBytes.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("cf-aig-gateway-id") != "extraction-credits" {
			t.Error("extraction must use the configured prepaid gateway")
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil || r.URL.Path != "/ai/run/@cf/mistralai/mistral-small-3.1-24b-instruct" || body["messages"] == nil || body["guided_json"] == nil || string(body["stream"]) != "false" {
			t.Error("wrong native extraction contract")
		}
		if !bytes.Contains(body["messages"], []byte(original)) || body["image"] != nil {
			t.Error("original image must remain in the native message")
		}
		var schema map[string]any
		if json.Unmarshal(body["guided_json"], &schema) != nil || schema["type"] != "object" {
			t.Error("missing native schema guidance")
		}
		w.Write([]byte(`{"success":true,"result":{"choices":[{"message":{"role":"assistant","content":"{\"value\":7}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5}}}`))
	}))
	defer server.Close()
	secret := filepath.Join(t.TempDir(), "token")
	os.WriteFile(secret, []byte("synthetic-token"), 0600)
	c := providerTestConnection(server.URL+"/ai", "cloudflare-mistral")
	c.SecretFile = secret
	c.Models = []string{"@cf/mistralai/mistral-small-3.1-24b-instruct"}
	c.DefaultModel = c.Models[0]
	c.Capabilities = Capabilities{Images: true}
	c.Cloudflare = &CloudflareConfig{AccountIDFile: secret, DecisionModel: "@cf/cloudflare/clef", GatewayID: "extraction-credits", SearchProvider: "ceramic"}
	p, err := NewProvider(c)
	if err != nil {
		t.Fatal(err)
	}
	request := moondreamReliabilityRequest()
	request.Messages[0].Images = []string{original}
	request.Schema = json.RawMessage(`{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"],"additionalProperties":false}`)
	if len(original) <= p.inputBytes() {
		t.Fatal("fixture must exceed generic text wire limit")
	}
	result, err := p.Generate(t.Context(), request, nil)
	if err != nil || calls != 1 || result.Message.Content != `{"value":7}` || result.Usage.Estimated || result.Usage.InputTokens != 100 || result.Usage.OutputTokens != 5 {
		t.Fatal(calls, result, err)
	}
}

func TestCloudflareMistralRejectsIncompleteOutputWithUsage(t *testing.T) {
	for _, finish := range []string{"length", "stop"} {
		content := ""
		if finish == "length" {
			content = "{\"value\":7}"
		}
		raw, _ := json.Marshal(map[string]any{"success": true, "result": map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": content}, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 5}}})
		result, err := (mistralFormat{}).decode(&Provider{}, raw)
		if err == nil || result.Usage.InputTokens != 100 || result.Usage.OutputTokens != 5 || !result.usageFinal {
			t.Fatal("incomplete content must fail while preserving reported usage", result, err)
		}
	}
	for _, raw := range []string{`{"success":true,"result":{}}`, `{"success":false,"result":{}}`, `{"success":true,"result":{"choices":[{"message":{"role":"assistant","content":"{}"},"finish_reason":"stop"}]}}`} {
		if _, err := (mistralFormat{}).decode(&Provider{}, []byte(raw)); err == nil {
			t.Fatal("invalid native response accepted")
		}
	}
}
