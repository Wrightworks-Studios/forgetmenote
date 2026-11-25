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

func TestProviderNativeStreams(t *testing.T) {
	cases := []struct{ format, body string }{
		{"responses", `event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"Hel"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"lo"}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc1","call_id":"call_1","name":"read_note","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"path\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"note.md\"}"}

event: response.completed
data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning","id":"rs1","encrypted_content":"opaque"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]},{"type":"function_call","id":"fc1","call_id":"call_1","name":"read_note","arguments":"{\"path\":\"note.md\"}"}],"usage":{"input_tokens":10,"output_tokens":5}}}

`},
		{"messages", `event: message_start
data: {"type":"message_start","message":{"role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"read_note","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"note.md\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`},
		{"chat-completions", `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Hello","reasoning_content":"private","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_note","arguments":"{\"path\":"}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"note.md\"}"}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}

data: [DONE]

`},
	}
	for _, tt := range cases {
		t.Run(tt.format, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, fragment := range strings.SplitAfter(tt.body, "\n") {
					fmt.Fprint(w, fragment)
					w.(http.Flusher).Flush()
				}
			}))
			defer srv.Close()
			conn := providerTestConnection(srv.URL, tt.format)
			conn.Capabilities.Stream = true
			conn.Capabilities.ReasoningContent = tt.format == "chat-completions"
			p, err := NewProvider(conn)
			if err != nil {
				t.Fatal(err)
			}
			var text string
			calls := 0
			result, err := p.Generate(context.Background(), Request{Model: "test-model", Messages: []Message{{Role: "user", Content: "hello"}}, Tools: []Tool{{Name: "read_note", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)}}}, func(e Event) error {
				if e.Type == "text_delta" {
					text += e.Text
				}
				if e.Type == "tool_call" {
					calls++
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if text != "Hello" || result.Message.Content != "Hello" || len(result.Message.ToolCalls) != 1 || calls != 1 || result.Usage.InputTokens != 10 || result.Usage.OutputTokens != 5 {
				t.Fatalf("stream lost data: text=%q calls=%d result=%#v", text, calls, result)
			}
			if string(result.Message.ToolCalls[0].Arguments) != `{"path":"note.md"}` {
				t.Fatalf("bad partial input: %s", result.Message.ToolCalls[0].Arguments)
			}
		})
	}
}
func TestProviderMalformedToolsAndRedirect(t *testing.T) {
	bodies := []string{
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"read_note","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"read_note","arguments":"{"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"truncated"},"finish_reason":"length"}]}`,
	}
	for _, body := range bodies {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		p, err := NewProvider(providerTestConnection(srv.URL, "chat-completions"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = p.Generate(context.Background(), Request{Model: "test-model", Messages: []Message{{Role: "user", Content: "hello"}}, Tools: []Tool{{Name: "read_note", Schema: json.RawMessage(`{"type":"object"}`)}}}, nil); err == nil {
			t.Errorf("accepted malformed/incomplete output %s", body)
		}
		srv.Close()
	}
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer origin.Close()
	p, _ := NewProvider(providerTestConnection(origin.URL, "responses"))
	if _, err := p.Generate(context.Background(), Request{Model: "test-model", Messages: []Message{{Role: "user", Content: "hi"}}}, nil); err == nil || redirected {
		t.Fatalf("redirect err=%v followed=%v", err, redirected)
	}
}

func TestProviderStreamHasNoAggregateWireLimit(t *testing.T) {
	// A long reasoning response can spend most of its wire bytes on repeated
	// event metadata. None of that metadata is accumulated as answer content.
	frame := `data: {"metadata":"` + strings.Repeat("x", 64<<10) + `","choices":[{"index":0,"delta":{"reasoning_content":"private"},"finish_reason":null}]}` + "\n\n"
	body := strings.Repeat(frame, 160) + `data: {"choices":[{"index":0,"delta":{"content":"Complete answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":200}}` + "\n\ndata: [DONE]\n\n"
	var visible strings.Builder
	got, err := reviewGenerateStream(t, "chat-completions", body, true, func(e Event) error {
		visible.WriteString(e.Text)
		return nil
	})
	if err != nil || got.Message.Content != "Complete answer" || visible.String() != "Complete answer" || got.Usage.Estimated || got.Usage.OutputTokens != 200 {
		t.Fatalf("long valid stream did not reach its answer and final usage: answer=%q usage=%+v err=%v", got.Message.Content, got.Usage, err)
	}
}
