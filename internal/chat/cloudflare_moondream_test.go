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

func TestCloudflareMoondreamNativeQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/ai/run/@cf/moondream/moondream3.1-9B-A2B" || body["stream"] != false || body["reasoning"] != false || body["task"] != "query" || body["image"] == nil || body["question"] != "Return JSON.\n\nExtract business." || body["messages"] != nil {
			t.Error("incorrect native Moondream query", r.URL.Path, body)
		}
		w.Write([]byte(`{"success":true,"result":{"result":{"answer":"{\"business\":\"Your Sign Maker\"}","finish_reason":"stop","metrics":{"input_tokens":741,"output_tokens":10}},"usage":{"prompt_tokens":741,"completion_tokens":10}}}`))
	}))
	defer server.Close()
	secret := filepath.Join(t.TempDir(), "token")
	os.WriteFile(secret, []byte("synthetic-token"), 0600)
	c := providerTestConnection(server.URL+"/ai", "cloudflare-moondream")
	c.SecretFile = secret
	c.Models = []string{"@cf/moondream/moondream3.1-9B-A2B"}
	c.DefaultModel = c.Models[0]
	c.Capabilities = Capabilities{Images: true}
	c.Cloudflare = &CloudflareConfig{AccountIDFile: secret, DecisionModel: "@cf/cloudflare/clef", GatewayID: "default", SearchProvider: "ceramic"}
	p, err := NewProvider(c)
	if err != nil {
		t.Fatal(err)
	}
	pixels := image.NewNRGBA(image.Rect(0, 0, 768, 768))
	random := rand.New(rand.NewPCG(17, 41))
	for i := range pixels.Pix {
		pixels.Pix[i] = byte(random.Uint32())
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, pixels); err != nil {
		t.Fatal(err)
	}
	inline, err := InlineImage(encoded.Bytes())
	if err != nil || len(inline) <= p.inputBytes() {
		t.Fatal("fixture must exceed text wire budget", err)
	}
	got, err := p.Generate(t.Context(), Request{System: "Return JSON.", Messages: []Message{{Role: "user", Content: "Extract business.", Images: []string{inline}}}}, nil)
	if err != nil || got.Message.Content != `{"business":"Your Sign Maker"}` || got.Usage.Estimated || got.Usage.InputTokens != 741 || got.Usage.OutputTokens != 10 {
		t.Fatal(got, err)
	}
}

func TestCloudflareMoondreamRejectsEmptyAndTruncatedOutput(t *testing.T) {
	for _, raw := range []string{
		`{"success":true,"result":{}}`,
		`{"success":true,"result":{"result":{"answer":"{}","finish_reason":"length","metrics":{"input_tokens":1,"output_tokens":1}},"usage":{"prompt_tokens":1,"completion_tokens":1}}}`,
		`{"success":true,"result":{"result":{"answer":"{}","finish_reason":"stop","metrics":{"input_tokens":1,"output_tokens":1}}}}`,
		`{"success":true,"result":{"result":{"answer":"{}","finish_reason":"stop","metrics":{"input_tokens":2,"output_tokens":1}},"usage":{"prompt_tokens":1,"completion_tokens":1}}}`,
	} {
		if _, err := (moondreamFormat{}).decode(nil, []byte(raw)); err == nil {
			t.Fatal("invalid native extraction accepted", raw)
		}
	}
}
