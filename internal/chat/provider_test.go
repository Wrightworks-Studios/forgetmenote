package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProviderNativeContracts(t *testing.T) {
	cases := []struct{ format, path, body string }{
		{"responses", "/v1/responses", `{"status":"completed","output":[{"type":"reasoning","id":"rs_1","encrypted_content":"opaque"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_note","arguments":"{\"path\":\"note.md\"}"}],"usage":{"input_tokens":12,"output_tokens":6,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}`},
		{"messages", "/v1/messages", `{"type":"message","role":"assistant","content":[{"type":"thinking","thinking":"private","signature":"sig"},{"type":"text","text":"Hello"},{"type":"tool_use","id":"call_1","name":"read_note","input":{"path":"note.md"}}],"stop_reason":"tool_use","usage":{"input_tokens":12,"output_tokens":6,"cache_read_input_tokens":3,"cache_creation_input_tokens":4}}`},
		{"chat-completions", "/v1/chat/completions", `{"choices":[{"message":{"role":"assistant","content":"Hello","reasoning_content":"private","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_note","arguments":"{\"path\":\"note.md\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":6,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`},
	}
	for _, tt := range cases {
		t.Run(tt.format, func(t *testing.T) {
			var received map[string]json.RawMessage
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tt.path {
					t.Errorf("path %s", r.URL.Path)
				}
				if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
					t.Error(err)
				}
				if _, ok := received["stream_options"]; ok {
					t.Error("unsupported stream options")
				}
				if _, ok := received["reasoning_effort"]; ok {
					t.Error("unsupported reasoning effort")
				}
				if _, ok := received["context_tokens"]; ok {
					t.Error("invented context parameter")
				}
				if tt.format == "messages" && r.Header.Get("anthropic-version") != "2023-06-01" {
					t.Error("missing native version")
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()
			conn := providerTestConnection(srv.URL+"/v1", tt.format)
			if tt.format == "chat-completions" {
				conn.Capabilities.ReasoningContent = true
			}
			p, err := NewProvider(conn)
			if err != nil {
				t.Fatal(err)
			}
			req := Request{Model: "test-model", System: "Safe system", Messages: []Message{{Role: "user", Content: "Read this"}}, MaxOutputTokens: 100, Tools: []Tool{{Name: "read_note", Description: "Read", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)}}}
			result, err := p.Generate(context.Background(), req, nil)
			if err != nil {
				t.Fatal(err)
			}
			wantInput := int64(12)
			if tt.format == "messages" {
				wantInput = 19
			}
			if result.Message.Content != "Hello" || len(result.Message.ToolCalls) != 1 || result.Message.ToolCalls[0].ID != "call_1" || result.Usage.InputTokens != wantInput || result.Usage.OutputTokens != 6 || result.Usage.CachedTokens != 3 {
				t.Fatalf("wrong native result: %#v", result)
			}
			if len(result.Message.Continuation) == 0 {
				t.Fatal("provider continuation lost")
			}
			req.Messages = append(req.Messages, result.Message, Message{Role: "tool", ToolCallID: "call_1", Content: "found"})
			if _, err = p.Generate(context.Background(), req, nil); err != nil {
				t.Fatal(err)
			}
			native, _ := json.Marshal(received)
			if tt.format == "chat-completions" && !strings.Contains(string(native), `reasoning_content`) {
				t.Fatalf("reasoning replay lost: %s", native)
			}
			if tt.format == "responses" && !strings.Contains(string(native), `encrypted_content`) {
				t.Fatal("responses opaque reasoning lost")
			}
			if tt.format == "messages" && !strings.Contains(string(native), `signature`) {
				t.Fatal("messages signature lost")
			}
		})
	}
}
func TestProviderRejectsIncompleteAndSanitizes(t *testing.T) {
	for _, body := range []string{`data: {"choices":[{"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n", `data: [DONE]` + "\n\n"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, body)
		}))
		conn := providerTestConnection(srv.URL, "chat-completions")
		conn.Capabilities.Stream = true
		p, err := NewProvider(conn)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = p.Generate(context.Background(), Request{Model: "test-model", Messages: []Message{{Role: "user", Content: "hello"}}}, nil); err == nil {
			t.Error("accepted missing explicit successful terminal response")
		}
		srv.Close()
	}
	dir := t.TempDir()
	secret := filepath.Join(dir, "token")
	if err := os.WriteFile(secret, []byte("SUPER_SECRET"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer SUPER_SECRET" {
			t.Error("missing configured credential")
		}
		w.WriteHeader(401)
		fmt.Fprint(w, "SUPER_SECRET private prompt endpoint")
	}))
	defer srv.Close()
	conn := providerTestConnection(srv.URL, "responses")
	conn.SecretFile = secret
	p, err := NewProvider(conn)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Generate(context.Background(), Request{Model: "test-model", Messages: []Message{{Role: "user", Content: "hello"}}}, nil)
	if err == nil || strings.Contains(err.Error(), "SUPER_SECRET") || strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("unsafe error: %v", err)
	}
}
func TestProviderCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	p, err := NewProvider(providerTestConnection(srv.URL, "responses"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err = p.Generate(ctx, Request{Model: "test-model", Messages: []Message{{Role: "user", Content: "hello"}}}, nil); err == nil {
		t.Fatal("ignored cancellation")
	}
	if time.Since(start) > time.Second {
		t.Fatal("slow cancellation")
	}
}
