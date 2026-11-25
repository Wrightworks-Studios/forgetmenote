package chat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderImageContentAndOptIn(t *testing.T) {
	for _, format := range []string{"responses", "chat-completions"} {
		t.Run(format, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				raw, _ := json.Marshal(body)
				if !strings.Contains(string(raw), "data:image/png;base64,") {
					t.Error("image was omitted from native request")
				}
				if format == "responses" {
					w.Write([]byte(`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"image"}]}],"usage":{"input_tokens":10,"output_tokens":2}}`))
				} else {
					w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"image"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`))
				}
			}))
			defer server.Close()
			conn := providerTestConnection(server.URL, format)
			var msg Message
			if err := json.Unmarshal([]byte(`{"role":"user","content":"Describe","images":["data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/l9sAAAAASUVORK5CYII="]}`), &msg); err != nil {
				t.Fatal(err)
			}
			provider, err := NewProvider(conn)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = provider.Generate(t.Context(), Request{Messages: []Message{msg}}, nil); err == nil || calls != 0 {
				t.Fatal("non-vision connection accepted an image", err, calls)
			}
			json.Unmarshal([]byte(`{"images":true}`), &conn.Capabilities)
			provider, err = NewProvider(conn)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = provider.Generate(t.Context(), Request{Messages: []Message{msg}}, nil); err != nil || calls != 1 {
				t.Fatal(err, calls)
			}
		})
	}
}
