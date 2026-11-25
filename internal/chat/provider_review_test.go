package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReviewMessagesInterruptedUsageIsUncertain(t *testing.T) {
	body := "data: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"usage\":{\"input_tokens\":8,\"output_tokens\":1}}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + strings.Repeat("substantial emitted response ", 100) + "\"}}\n\n"
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
	got, err := p.Generate(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, nil)
	if err == nil {
		t.Fatal("unexpected successful incomplete stream")
	}
	if !got.Usage.Estimated || got.Usage.OutputTokens <= 1 {
		t.Fatalf("partial stream advertised initial usage as complete despite %d output bytes: %+v; err=%v", len(got.Message.Content), got.Usage, err)
	}
}

func TestReviewResponsesInterruptedToolUsage(t *testing.T) {
	body := "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc1\",\"call_id\":\"c1\",\"name\":\"read_note\",\"arguments\":\"\"}}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{\\\"path\\\":\\\"" + strings.Repeat("long-", 100) + "note.md\\\"}\"}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	c := providerTestConnection(srv.URL, "responses")
	c.Capabilities.Stream = true
	p, err := NewProvider(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Generate(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hello"}}, Tools: []Tool{{Name: "read_note", Schema: json.RawMessage(`{"type":"object"}`)}}}, nil)
	if err == nil {
		t.Fatal("unexpected successful incomplete stream")
	}
	if got.Usage.OutputTokens == 0 {
		t.Fatalf("provider tool output disappeared from partial usage: %+v; err=%v", got.Usage, err)
	}
}

func TestReviewMessagesTotalIncludesCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":8,"cache_creation_input_tokens":5,"cache_read_input_tokens":300,"output_tokens":4}}`)
	}))
	defer srv.Close()
	p, err := NewProvider(providerTestConnection(srv.URL, "messages"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Generate(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var total Accounting
	total.Add(got.Usage)
	if total.Tokens() != 317 {
		t.Fatalf("total excludes Messages cache tokens: got=%d usage=%+v want=317", total.Tokens(), got.Usage)
	}
}

func TestReviewMessagesStreamTotalIncludesCache(t *testing.T) {
	body := "data: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"usage\":{\"input_tokens\":8,\"output_tokens\":1,\"cache_read_input_tokens\":300,\"cache_creation_input_tokens\":5}}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"ok\"}}\n\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	got, err := reviewGenerateStream(t, "messages", body, false, nil)
	if err != nil || got.Usage.InputTokens != 313 || got.Usage.OutputTokens != 4 || got.Usage.CachedTokens != 300 || got.Usage.CacheWriteTokens != 5 || got.Usage.Estimated {
		t.Fatalf("stream total=%+v err=%v", got.Usage, err)
	}
}

func reviewGenerateStream(t *testing.T, format, body string, reasoning bool, emit func(Event) error) (Response, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	c := providerTestConnection(srv.URL, format)
	c.Capabilities.Stream = true
	c.Capabilities.ReasoningContent = reasoning
	p, err := NewProvider(c)
	if err != nil {
		t.Fatal(err)
	}
	return p.Generate(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hello"}}, Tools: []Tool{{Name: "read_note", Schema: json.RawMessage(`{"type":"object"}`)}}}, emit)
}

func TestReviewInterruptedPrivateOutputIsCounted(t *testing.T) {
	private := strings.Repeat("private-material ", 100)
	tests := []struct {
		name, format, body string
		reasoning          bool
	}{
		{"messages tool", "messages", `data: {"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":8,"output_tokens":1}}}` + "\n\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c1","name":"read_note","input":{}}}` + "\n\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"` + private + `"}}` + "\n\n", false},
		{"messages thinking", "messages", `data: {"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":8,"output_tokens":1}}}` + "\n\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}` + "\n\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"` + private + `"}}` + "\n\n", false},
		{"responses reasoning", "responses", `data: {"type":"response.reasoning_text.delta","output_index":0,"delta":"` + private + `"}` + "\n\n", false},
		{"completions reasoning without replay", "chat-completions", `data: {"choices":[{"index":0,"delta":{"reasoning_content":"` + private + `"},"finish_reason":null}],"usage":{"prompt_tokens":8,"completion_tokens":1}}` + "\n\n", false},
		{"completions partial tool", "chat-completions", `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"read_note","arguments":"` + private + `"}}]},"finish_reason":null}],"usage":{"prompt_tokens":8,"completion_tokens":1}}` + "\n\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var emitted strings.Builder
			got, err := reviewGenerateStream(t, tt.format, tt.body, tt.reasoning, func(e Event) error { emitted.WriteString(e.Text); return nil })
			if err == nil || !got.Usage.Estimated || got.Usage.OutputTokens < int64(len(private)/3) {
				t.Fatalf("private output unaccounted: %+v err=%v", got.Usage, err)
			}
			if strings.Contains(emitted.String(), "private-material") || strings.Contains(got.Message.Content, "private-material") {
				t.Fatal("private accounting material exposed as answer")
			}
		})
	}
}

func TestReviewOnlyFinalUsageIsAuthoritative(t *testing.T) {
	text := strings.Repeat("completed output ", 50)
	cases := []struct{ format, body string }{
		{"responses", `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"` + text + `"}]}],"usage":{"input_tokens":8,"output_tokens":4}}}` + "\n\n"},
		{"messages", `data: {"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":8,"output_tokens":1}}}` + "\n\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"` + text + `"}}` + "\n\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}` + "\n\n" + `data: {"type":"message_stop"}` + "\n\n"},
		{"chat-completions", `data: {"choices":[{"index":0,"delta":{"content":"` + text + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":4}}` + "\n\n" + "data: [DONE]\n\n"},
	}
	for _, tt := range cases {
		t.Run(tt.format, func(t *testing.T) {
			got, err := reviewGenerateStream(t, tt.format, tt.body, false, nil)
			if err != nil || got.Usage.InputTokens != 8 || got.Usage.OutputTokens != 4 || got.Usage.Estimated {
				t.Fatalf("overrode final usage: %+v err=%v", got.Usage, err)
			}
		})
	}
	// Initial usage followed by a successful finish is still not a final total.
	body := `data: {"choices":[{"index":0,"delta":{"content":"` + text + `"},"finish_reason":null}],"usage":{"prompt_tokens":8,"completion_tokens":1}}` + "\n\n" + `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" + "data: [DONE]\n\n"
	got, err := reviewGenerateStream(t, "chat-completions", body, false, nil)
	if err != nil || !got.Usage.Estimated || got.Usage.OutputTokens <= 1 {
		t.Fatalf("initial usage treated as final: %+v err=%v", got.Usage, err)
	}
}

func TestReviewPartialInputUsageRemainsLowerBound(t *testing.T) {
	for _, tt := range []struct{ format, body string }{
		{"chat-completions", `data: {"choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}],"usage":{"prompt_tokens":10000}}` + "\n\n"},
		{"responses", `data: {"type":"response.in_progress","response":{"status":"in_progress","usage":{"input_tokens":10000,"output_tokens":1}}}` + "\n\n" + `data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n"},
	} {
		t.Run(tt.format, func(t *testing.T) {
			got, err := reviewGenerateStream(t, tt.format, tt.body, false, nil)
			if err == nil || !got.Usage.Estimated || got.Usage.InputTokens < 10000 {
				t.Fatalf("discarded native lower bound: %+v err=%v", got.Usage, err)
			}
		})
	}
}

func TestInvalidProviderUsageCannotReduceAccounting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":-10000,"completion_tokens":5}}`)
	}))
	defer srv.Close()
	p, err := NewProvider(providerTestConnection(srv.URL, "chat-completions"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Generate(t.Context(), Request{Model: "test-model", Messages: []Message{{Role: "user", Content: "Synthetic request"}}}, nil)
	if err == nil || result.Usage.InputTokens < 1 || !result.Usage.Estimated {
		t.Fatalf("invalid native totals escaped as a accounting credit: %+v %v", result.Usage, err)
	}
}
