package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderMessagesUsageMetadata(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"usage\":{\"input_tokens\":8,\"cache_read_input_tokens\":3,\"cache_creation_input_tokens\":5}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4,\"cache_creation\":{\"ephemeral_5m_input_tokens\":5}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	c := providerTestConnection(srv.URL, "messages")
	c.Capabilities.Stream = true
	p, err := NewProvider(c)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Generate(context.Background(), Request{Model: "test-model", Messages: []Message{{Role: "user", Content: "hello"}}}, nil)
	if err != nil || r.Usage.InputTokens != 16 || r.Usage.OutputTokens != 4 || r.Usage.CachedTokens != 3 || r.Usage.CacheWriteTokens != 5 {
		t.Fatalf("native metadata broke usage: %#v %v", r, err)
	}
}
func TestProviderCapabilitiesAreExplicit(t *testing.T) {
	for _, format := range []string{"responses", "messages", "chat-completions"} {
		t.Run(format, func(t *testing.T) {
			var got map[string]json.RawMessage
			body := `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
			if format == "responses" {
				body = `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`
			}
			if format == "messages" {
				body = `{"type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewDecoder(r.Body).Decode(&got)
				fmt.Fprint(w, body)
			}))
			defer srv.Close()
			c := providerTestConnection(srv.URL, format)
			c.Capabilities.StrictTools = true
			switch format {
			case "responses":
				c.Capabilities.StoreFalse = true
				c.Capabilities.EncryptedReasoning = true
				c.Capabilities.ReasoningEffort = "low"
			case "messages":
				c.Capabilities.CacheControl = true
				c.Capabilities.Thinking = "enabled"
				c.Capabilities.ThinkingBudget = 1024
			case "chat-completions":
				c.Capabilities.Thinking = "disabled"
				c.Capabilities.OutputTokenParameter = "max_completion_tokens"
			}
			p, err := NewProvider(c)
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.Generate(context.Background(), Request{Model: "test-model", System: "system", Messages: []Message{{Role: "user", Content: "hi"}}, Tools: []Tool{{Name: "read_note", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(got)
			if !strings.Contains(string(raw), `"strict":true`) {
				t.Fatal("strict tools omitted")
			}
			if format == "responses" && (string(got["store"]) != "false" || !strings.Contains(string(got["include"]), "reasoning.encrypted_content") || !strings.Contains(string(got["reasoning"]), "low")) {
				t.Fatalf("missing Responses capabilities: %s", raw)
			}
			if format == "messages" && (!strings.Contains(string(got["system"]), "cache_control") || !strings.Contains(string(got["thinking"]), "1024")) {
				t.Fatalf("missing Messages capabilities: %s", raw)
			}
			if format == "chat-completions" && (got["max_completion_tokens"] == nil || got["max_tokens"] != nil || !strings.Contains(string(got["thinking"]), "disabled")) {
				t.Fatalf("wrong completion capabilities: %s", raw)
			}
		})
	}
}
func TestProviderRejectsOversizedToolsAndHonorsAdmission(t *testing.T) {
	body := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_note","arguments":"` + strings.Repeat(" ", encodedLimit(defaultLimits().OutputTokens)+1) + `"}}]},"finish_reason":null}]}` + "\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, body)
	}))
	c := providerTestConnection(srv.URL, "chat-completions")
	c.Capabilities.Stream = true
	p, err := NewProvider(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Generate(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, nil); err == nil {
		t.Fatal("accepted oversized tool input")
	}
	srv.Close()
	var active, peak atomic.Int32
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		entered <- struct{}{}
		select {
		case <-release:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	c = providerTestConnection(srv.URL, "chat-completions")
	c.Limits.ConnectionConcurrent = 1
	p, err = NewProvider(c)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() {
		_, err := p.Generate(context.Background(), Request{Messages: []Message{{Role: "user", Content: "one"}}}, nil)
		done <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	go func() {
		_, err := p.Generate(ctx, Request{Messages: []Message{{Role: "user", Content: "two"}}}, nil)
		done <- err
	}()
	if err := <-done; err == nil {
		t.Fatal("queued request ignored deadline")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 1 {
		t.Fatalf("request concurrency exceeded: %d", peak.Load())
	}
}

func TestProviderCancellationRetainsPartialUsage(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	c := providerTestConnection(srv.URL, "chat-completions")
	c.Capabilities.Stream = true
	p, err := NewProvider(c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	r, err := p.Generate(ctx, Request{Messages: []Message{{Role: "user", Content: "hello"}}}, nil)
	if err == nil || !r.Usage.Estimated || r.Usage.InputTokens == 0 || r.Message.Content != "partial" {
		t.Fatalf("cancelled stream lost partial accounting: %#v %v", r, err)
	}
}

func TestProviderGroupsNativeToolResults(t *testing.T) {
	for _, format := range []string{"responses", "messages", "chat-completions"} {
		t.Run(format, func(t *testing.T) {
			var got map[string]json.RawMessage
			body := `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
			if format == "responses" {
				body = `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`
			}
			if format == "messages" {
				body = `{"type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewDecoder(r.Body).Decode(&got)
				fmt.Fprint(w, body)
			}))
			defer srv.Close()
			p, err := NewProvider(providerTestConnection(srv.URL, format))
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.Generate(context.Background(), Request{Messages: []Message{{Role: "user", Content: "read"}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "one", Name: "read_note", Arguments: json.RawMessage(`{"path":"one.md"}`)}, {ID: "two", Name: "read_note", Arguments: json.RawMessage(`{"path":"two.md"}`)}}}, {Role: "tool", ToolCallID: "one", Content: "first"}, {Role: "tool", ToolCallID: "two", Content: "second"}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var native []map[string]json.RawMessage
			field := "messages"
			if format == "responses" {
				field = "input"
			}
			if err = json.Unmarshal(got[field], &native); err != nil {
				t.Fatal(err)
			}
			switch format {
			case "messages":
				if len(native) != 3 {
					t.Fatalf("tool results need one user message: %s", got[field])
				}
				var blocks []map[string]any
				json.Unmarshal(native[2]["content"], &blocks)
				if len(blocks) != 2 || blocks[0]["tool_use_id"] != "one" || blocks[1]["tool_use_id"] != "two" {
					t.Fatalf("unpaired tool results: %s", got[field])
				}
			case "responses":
				if len(native) != 5 || string(native[3]["type"]) != `"function_call_output"` || string(native[4]["call_id"]) != `"two"` {
					t.Fatalf("wrong Responses items: %s", got[field])
				}
			case "chat-completions":
				if len(native) != 4 || string(native[2]["tool_call_id"]) != `"one"` || string(native[3]["role"]) != `"tool"` {
					t.Fatalf("wrong completions tool results: %s", got[field])
				}
			}
		})
	}
}

func TestProviderJSONOutputIsRequestedOnlyForStructuredWork(t *testing.T) {
	for _, format := range []string{"chat-completions", "responses"} {
		t.Run(format, func(t *testing.T) {
			var got map[string]json.RawMessage
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = nil
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				if format == "responses" {
					fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"pages\":[],\"findings\":[]}"}]}]}`)
				} else {
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"{\"pages\":[],\"findings\":[]}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
				}
			}))
			defer srv.Close()
			c := providerTestConnection(srv.URL, format)
			c.Capabilities.JSONOutput = true
			p, err := NewProvider(c)
			if err != nil {
				t.Fatal(err)
			}
			req := Request{JSONOutput: true, FinalAnswer: true, System: "Return JSON.", Messages: []Message{{Role: "user", Content: "Document the supplied sources."}}}
			if _, err = p.Generate(t.Context(), req, nil); err != nil {
				t.Fatal(err)
			}
			field, want := "response_format", `{"type":"json_object"}`
			if format == "responses" {
				field, want = "text", `{"format":{"type":"json_object"}}`
			}
			if string(got[field]) != want {
				t.Fatalf("structured generation did not request native JSON output: %s", got[field])
			}
			if _, err = p.Generate(t.Context(), Request{Messages: []Message{{Role: "user", Content: "Normal chat"}}}, nil); err != nil {
				t.Fatal(err)
			}
			if got[field] != nil {
				t.Fatal("ordinary chat was forced to JSON")
			}
			c.Capabilities.JSONOutput = false
			p, err = NewProvider(c)
			if err != nil {
				t.Fatal(err)
			}
			got = nil
			if _, err = p.Generate(t.Context(), req, nil); err == nil || got != nil {
				t.Fatal("unsupported JSON capability was sent to the provider")
			}
		})
	}
}

func TestProviderWorkloadReasoningDoesNotChangeConnectionDefault(t *testing.T) {
	var effort string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		effort = body.Reasoning.Effort
		fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Draft"}]}]}`)
	}))
	defer srv.Close()
	c := providerTestConnection(srv.URL, "responses")
	c.Capabilities.ReasoningEffort = "max"
	p, err := NewProvider(c)
	if err != nil {
		t.Fatal(err)
	}
	req := Request{Messages: []Message{{Role: "user", Content: "Draft a brief reply"}}, ReasoningEffort: "low"}
	if _, err = p.Generate(t.Context(), req, nil); err != nil || effort != "low" {
		t.Fatalf("workload effort=%s error=%v", effort, err)
	}
	req.ReasoningEffort = ""
	if _, err = p.Generate(t.Context(), req, nil); err != nil || effort != "max" {
		t.Fatalf("connection default changed: %s %v", effort, err)
	}
	req.ReasoningEffort = "unbounded"
	if _, err = p.Generate(t.Context(), req, nil); err == nil {
		t.Fatal("invalid effort accepted")
	}
}
