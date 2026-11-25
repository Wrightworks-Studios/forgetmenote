package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestProviderFinalAnswerNativeContracts(t *testing.T) {
	cases := []struct {
		format, path, initial, final, history, choice string
		usage                                         Usage
	}{
		{
			format: "responses", path: "/v1/responses", choice: `"none"`,
			initial: `{"status":"completed","output":[{"type":"reasoning","id":"rs_1","encrypted_content":"opaque","summary":[]},{"type":"function_call","id":"fc_1","call_id":"one","name":"read_note","arguments":"{\"path\":\"one.md\"}"},{"type":"function_call","id":"fc_2","call_id":"two","name":"read_note","arguments":"{\"path\":\"two.md\"}"}],"usage":{"input_tokens":12,"output_tokens":6,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}`,
			final:   `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Both notes agree."}]}],"usage":{"input_tokens":12,"output_tokens":6,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}`,
			history: `[{"role":"user","content":"Compare notes"},{"type":"reasoning","id":"rs_1","encrypted_content":"opaque","summary":[]},{"type":"function_call","id":"fc_1","call_id":"one","name":"read_note","arguments":"{\"path\":\"one.md\"}"},{"type":"function_call","id":"fc_2","call_id":"two","name":"read_note","arguments":"{\"path\":\"two.md\"}"},{"type":"function_call_output","call_id":"one","output":"first complete note"},{"type":"function_call_output","call_id":"two","output":"second complete note"}]`,
			usage:   Usage{InputTokens: 12, OutputTokens: 6, CachedTokens: 3, ReasoningTokens: 2},
		},
		{
			format: "messages", path: "/v1/messages", choice: `{"type":"none"}`,
			initial: `{"type":"message","role":"assistant","content":[{"type":"thinking","thinking":"private","signature":"sig"},{"type":"tool_use","id":"one","name":"read_note","input":{"path":"one.md"}},{"type":"tool_use","id":"two","name":"read_note","input":{"path":"two.md"}}],"stop_reason":"tool_use","usage":{"input_tokens":12,"output_tokens":6,"cache_read_input_tokens":3,"cache_creation_input_tokens":4}}`,
			final:   `{"type":"message","role":"assistant","content":[{"type":"text","text":"Both notes agree."}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":6,"cache_read_input_tokens":3,"cache_creation_input_tokens":4}}`,
			history: `[{"role":"user","content":[{"type":"text","text":"Compare notes"}]},{"role":"assistant","content":[{"type":"thinking","thinking":"private","signature":"sig"},{"type":"tool_use","id":"one","name":"read_note","input":{"path":"one.md"}},{"type":"tool_use","id":"two","name":"read_note","input":{"path":"two.md"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"one","content":"first complete note"},{"type":"tool_result","tool_use_id":"two","content":"second complete note"}]}]`,
			usage:   Usage{InputTokens: 19, OutputTokens: 6, CachedTokens: 3, CacheWriteTokens: 4},
		},
		{
			format: "chat-completions", path: "/v1/chat/completions",
			initial: `{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"private","tool_calls":[{"id":"one","type":"function","function":{"name":"read_note","arguments":"{\"path\":\"one.md\"}"}},{"id":"two","type":"function","function":{"name":"read_note","arguments":"{\"path\":\"two.md\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":6,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`,
			final:   `{"choices":[{"message":{"role":"assistant","content":"Both notes agree."},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":6,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`,
			history: `[{"role":"system","content":"Use the notes"},{"role":"user","content":"Compare notes"},{"role":"assistant","content":"","reasoning_content":"private","tool_calls":[{"id":"one","type":"function","function":{"name":"read_note","arguments":"{\"path\":\"one.md\"}"}},{"id":"two","type":"function","function":{"name":"read_note","arguments":"{\"path\":\"two.md\"}"}}]},{"role":"tool","tool_call_id":"one","content":"first complete note"},{"role":"tool","tool_call_id":"two","content":"second complete note"}]`,
			usage:   Usage{InputTokens: 12, OutputTokens: 6, CachedTokens: 3, ReasoningTokens: 2},
		},
	}
	for _, tt := range cases {
		t.Run(tt.format, func(t *testing.T) {
			var received []map[string]json.RawMessage
			var wireSizes []int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tt.path {
					t.Errorf("path = %s, want %s", r.URL.Path, tt.path)
				}
				var body map[string]json.RawMessage
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				wireSizes = append(wireSizes, len(raw))
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Error(err)
				}
				received = append(received, body)
				w.Header().Set("Content-Type", "application/json")
				if len(received) == 2 {
					fmt.Fprint(w, tt.final)
				} else {
					fmt.Fprint(w, tt.initial)
				}
			}))
			defer srv.Close()
			conn := providerTestConnection(srv.URL+"/v1", tt.format)
			conn.Capabilities.StrictTools = true
			switch tt.format {
			case "responses":
				conn.Capabilities.StoreFalse = true
				conn.Capabilities.EncryptedReasoning = true
				conn.Capabilities.ReasoningEffort = "low"
			case "messages":
				conn.Capabilities.CacheControl = true
				conn.Capabilities.Thinking = "enabled"
				conn.Capabilities.ThinkingBudget = 1024
			case "chat-completions":
				conn.Capabilities.ReasoningContent = true
				conn.Capabilities.Thinking = "enabled"
				conn.Capabilities.OutputTokenParameter = "max_completion_tokens"
			}
			p, err := NewProvider(conn)
			if err != nil {
				t.Fatal(err)
			}
			req := Request{System: "Use the notes", Messages: []Message{{Role: "user", Content: "Compare notes"}}, MaxOutputTokens: 1500, Tools: []Tool{{Name: "read_note", Description: "Read note", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)}}}
			estimate, err := p.EstimateRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			initial, err := p.Generate(t.Context(), req, nil)
			if err != nil || len(initial.Message.ToolCalls) != 2 || len(initial.Message.Continuation) == 0 {
				t.Fatalf("initial native calls/reasoning missing: %+v, %v", initial, err)
			}
			if estimate != (wireSizes[0]+2)/3 {
				t.Fatalf("initial estimate %d differs from wire size %d", estimate, wireSizes[0])
			}
			req.Messages = append(req.Messages, initial.Message, Message{Role: "tool", ToolCallID: "one", Content: "first complete note"}, Message{Role: "tool", ToolCallID: "two", Content: "second complete note"})
			req.FinalAnswer = true
			before, _ := json.Marshal(req)
			estimate, err = p.EstimateRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			final, err := p.Generate(t.Context(), req, nil)
			if err != nil || final.Message.Content != "Both notes agree." || final.Usage != tt.usage {
				t.Fatalf("final answer = %+v, %v", final, err)
			}
			if estimate != (wireSizes[1]+2)/3 {
				t.Fatalf("final estimate %d differs from wire size %d", estimate, wireSizes[1])
			}
			after, _ := json.Marshal(req)
			if !bytes.Equal(before, after) {
				t.Fatal("provider mutated caller history or tool definitions")
			}
			got := received[1]
			if tt.choice == "" {
				if got["tool_choice"] != nil || got["tools"] != nil {
					t.Errorf("compatible final answer must omit tools and tool_choice: %s / %s", got["tools"], got["tool_choice"])
				}
			} else {
				assertFinalJSON(t, got["tool_choice"], tt.choice)
				if !bytes.Equal(got["tools"], received[0]["tools"]) || got["tools"] == nil {
					t.Error("native final answer changed tool definitions")
				}
			}
			field, output := "messages", "max_completion_tokens"
			allowed := map[string]bool{"model": true, "messages": true, "max_completion_tokens": true, "thinking": true}
			switch tt.format {
			case "responses":
				field, output = "input", "max_output_tokens"
				allowed = map[string]bool{"model": true, "input": true, "max_output_tokens": true, "instructions": true, "store": true, "include": true, "reasoning": true, "tools": true, "tool_choice": true}
				assertFinalJSON(t, got["store"], `false`)
				assertFinalJSON(t, got["include"], `["reasoning.encrypted_content"]`)
				assertFinalJSON(t, got["reasoning"], `{"effort":"low"}`)
			case "messages":
				output = "max_tokens"
				allowed = map[string]bool{"model": true, "messages": true, "max_tokens": true, "system": true, "thinking": true, "tools": true, "tool_choice": true}
				assertFinalJSON(t, got["thinking"], `{"type":"enabled","budget_tokens":1024}`)
				assertFinalJSON(t, got["system"], `[{"type":"text","text":"Use the notes","cache_control":{"type":"ephemeral"}}]`)
			case "chat-completions":
				assertFinalJSON(t, got["thinking"], `{"type":"enabled"}`)
			}
			assertFinalJSON(t, got[field], tt.history)
			assertFinalJSON(t, got[output], `1500`)
			for key := range got {
				if !allowed[key] {
					t.Errorf("unsupported final-answer field %q", key)
				}
			}
			if received[0]["tool_choice"] != nil || received[0]["tools"] == nil {
				t.Error("normal exploration request changed")
			}
			toolEvents := 0
			rejected, err := p.Generate(t.Context(), req, func(e Event) error {
				if e.Type == "tool_call" {
					toolEvents++
				}
				return nil
			})
			if !errors.Is(err, errProviderData) || toolEvents != 0 {
				t.Errorf("unexpected final-answer tools escaped validation: err=%v tool events=%d", err, toolEvents)
			}
			if rejected.Usage != tt.usage || !reflect.DeepEqual(rejected.Message, initial.Message) {
				t.Errorf("rejected response lost authoritative usage or native history: %+v", rejected)
			}
		})
	}
}

func assertFinalJSON(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var actual, expected any
	if err := json.Unmarshal(got, &actual); err != nil {
		t.Errorf("invalid JSON %q: %v; want %s", got, err, want)
		return
	}
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Errorf("JSON = %s, want %s", got, want)
	}
}

func TestProviderFinalAnswerCompatibleEstimateOmitsSchemas(t *testing.T) {
	var received map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Bounded answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":6}}`)
	}))
	defer srv.Close()
	conn := providerTestConnection(srv.URL, "chat-completions")
	conn.Capabilities.StrictTools = true
	p, err := NewProvider(conn)
	if err != nil {
		t.Fatal(err)
	}
	for _, schema := range []string{
		`{"type":"object","properties":{},"additionalProperties":false}`,
		`{"type":"object","properties":{}}`,
	} {
		req := Request{FinalAnswer: true, Messages: []Message{{Role: "user", Content: "Answer now"}}, MaxOutputTokens: 100, Tools: []Tool{{Name: "read_note", Description: strings.Repeat("omitted schema context ", 5000), Schema: json.RawMessage(schema)}}}
		without := req
		without.Tools = nil
		want, err := p.EstimateRequest(without)
		if err != nil {
			t.Fatal(err)
		}
		got, err := p.EstimateRequest(req)
		if err != nil || got != want {
			t.Errorf("omitted schema affected final-answer estimate: got %d, want %d, err=%v", got, want, err)
		}
		if _, err = p.Generate(t.Context(), req, nil); err != nil {
			t.Errorf("omitted schema affected context admission: %v", err)
		}
		if received["tools"] != nil || received["tool_choice"] != nil {
			t.Error("omitted schema was sent to compatible provider")
		}
		if len(req.Tools) != 1 || len(req.Tools[0].Description) == 0 || string(req.Tools[0].Schema) != schema {
			t.Fatal("final-answer normalization mutated caller tools")
		}
	}
}

// Opaque native history can omit the separately recorded assistant text on the wire.
// The fitter still needs a conservative estimate before it decides what to retain.
func TestProviderEstimatePreservesRawHistoryFloor(t *testing.T) {
	for _, format := range []string{"responses", "messages"} {
		t.Run(format, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				t.Error("oversized or malformed request reached provider")
			}))
			defer server.Close()
			p, err := NewProvider(providerTestConnection(server.URL, format))
			if err != nil {
				t.Fatal(err)
			}
			req := Request{Messages: []Message{{Role: "user", Content: "question"}, {Role: "assistant", Content: strings.Repeat("x", 100000), Continuation: json.RawMessage(`[]`)}}}
			estimate, err := p.EstimateRequest(req)
			if err != nil || estimate < 100000/3 {
				t.Fatalf("raw history floor lost: estimate=%d err=%v", estimate, err)
			}
			if _, err = p.Generate(t.Context(), req, nil); err == nil || !strings.Contains(err.Error(), "context") {
				t.Fatalf("oversized request admitted: %v", err)
			}
			req.Messages[1].Content = "small"
			req.Messages[1].Continuation = json.RawMessage(`{"invalid":"array required"}`)
			if _, err = p.EstimateRequest(req); !errors.Is(err, errProviderData) {
				t.Fatalf("malformed continuation estimate: %v", err)
			}
			if _, err = p.Generate(t.Context(), req, nil); !errors.Is(err, errProviderData) {
				t.Fatalf("malformed continuation generation: %v", err)
			}
			if calls != 0 {
				t.Fatalf("provider called %d times", calls)
			}
		})
	}
}
