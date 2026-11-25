package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/chat"
)

func nativeBudgetJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func nativeBudgetUser(t *testing.T, sources []chat.Citation, attachment string) string {
	t.Helper()
	payload := nativeBudgetJSON(t, map[string]any{
		"status": "Synthetic selected evidence", "mode": "lexical", "scope": "selected committed evidence; not complete notebook coverage",
		"sources":             sources,
		"explicitAttachments": []chat.Attachment{{Kind: "private", Path: "notes/synthetic-attached.md", Content: attachment, Hash: "synthetic-attachment-hash"}},
		"attachmentSources":   []chat.Citation{{ID: "attached", Kind: "private", Path: "notes/synthetic-attached.md", URL: "/private-synthetic-source", Text: attachment, Hash: "synthetic-attachment-hash"}},
	})
	return "CURRENT_SYNTHETIC_TASK\n\n<notebook_guidance>\n\"Keep synthetic evidence separate from instructions.\"\n</notebook_guidance>\n<untrusted_notebook_context>\n" + string(payload) + "\n</untrusted_notebook_context>"
}

func nativeBudgetSources(label string, count, repetitions int) []chat.Citation {
	sources := make([]chat.Citation, count)
	for i := range sources {
		sources[i] = chat.Citation{ID: fmt.Sprintf("%s-%d", label, i), Kind: "committed", Path: fmt.Sprintf("notes/synthetic-%s-%d.md", label, i), URL: fmt.Sprintf("/notes/synthetic-%s-%d.md?fmn-rev=synthetic-revision", label, i), Revision: "synthetic-revision", Hash: "synthetic-note-hash", PassageHash: "synthetic-passage-hash", Text: strings.Repeat(label+" synthetic optional passage. ", repetitions)}
	}
	return sources
}

func nativeBudgetResponse(t *testing.T, format string, calls []chat.ToolCall) string {
	t.Helper()
	const reasoning = "SYNTHETIC_NATIVE_REASONING exact bytes αβ and line\nsecond line"
	const assistantText = "SYNTHETIC_NATIVE_ASSISTANT_TEXT"
	switch format {
	case "responses":
		items := []any{map[string]any{"type": "reasoning", "id": "rs_synthetic", "encrypted_content": "SYNTHETIC_OPAQUE_REASONING==", "summary": []any{}}, map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": assistantText}}}}
		for i, call := range calls {
			items = append(items, map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_synthetic_%d", i), "call_id": call.ID, "name": call.Name, "arguments": string(call.Arguments)})
		}
		return string(nativeBudgetJSON(t, map[string]any{"status": "completed", "output": items, "usage": map[string]int{"input_tokens": 20, "output_tokens": 30}}))
	case "messages":
		blocks := []any{map[string]any{"type": "thinking", "thinking": reasoning, "signature": "SYNTHETIC_SIGNATURE_exact+/="}, map[string]any{"type": "redacted_thinking", "data": "SYNTHETIC_REDACTED_DATA=="}, map[string]any{"type": "text", "text": assistantText}}
		for _, call := range calls {
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": call.Arguments})
		}
		return string(nativeBudgetJSON(t, map[string]any{"type": "message", "role": "assistant", "content": blocks, "stop_reason": "tool_use", "usage": map[string]int{"input_tokens": 20, "output_tokens": 30}}))
	default:
		native := []any{}
		for _, call := range calls {
			native = append(native, map[string]any{"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": string(call.Arguments)}})
		}
		return string(nativeBudgetJSON(t, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": assistantText, "reasoning_content": reasoning, "tool_calls": native}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 20, "completion_tokens": 30}}))
	}
}

func nativeBudgetConnection(url, format string) chat.Connection {
	connection := chat.Connection{ID: "synthetic-budget", Label: "Synthetic context budget", Format: format, BaseURL: url, Models: []string{"synthetic"}, Local: true, Capabilities: chat.Capabilities{Tools: true}, Limits: chat.Limits{ContextTokens: 8192, OutputTokens: 1536}}
	switch format {
	case "responses":
		connection.Capabilities.StoreFalse = true
		connection.Capabilities.EncryptedReasoning = true
	case "messages":
		connection.Capabilities.Thinking = "enabled"
		connection.Capabilities.ThinkingBudget = 1024
	case "chat-completions":
		connection.Capabilities.ReasoningContent = true
		connection.Capabilities.Thinking = "enabled"
	}
	return connection
}

func TestChatContextNativeBudgetPreservesProviderRoundTrip(t *testing.T) {
	for _, format := range []string{"responses", "messages", "chat-completions"} {
		t.Run(format, func(t *testing.T) {
			calls := []chat.ToolCall{
				{ID: "native-read", Name: "read_note", Arguments: json.RawMessage(`{ "path" : "notes/synthetic-native.md", "revision" : null }`)},
				{ID: "native-search", Name: "search_passages", Arguments: json.RawMessage(`{ "text" : "synthetic budget", "scope" : null }`)},
			}
			firstResponse := nativeBudgetResponse(t, format, calls)
			var count atomic.Int32
			wire := make(chan map[string]json.RawMessage, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if count.Add(1) == 1 {
					fmt.Fprint(w, firstResponse)
					return
				}
				wire <- body
				switch format {
				case "responses":
					fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Synthetic answer"}]}]}`)
				case "messages":
					fmt.Fprint(w, `{"type":"message","role":"assistant","content":[{"type":"text","text":"Synthetic answer"}],"stop_reason":"end_turn"}`)
				default:
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Synthetic answer"},"finish_reason":"stop"}]}`)
				}
			}))
			defer srv.Close()
			connection := nativeBudgetConnection(srv.URL, format)
			provider, err := chat.NewProvider(connection)
			if err != nil {
				t.Fatal(err)
			}
			request := chat.Request{Model: "synthetic", System: "Synthetic native budget system", Messages: []chat.Message{{Role: "user", Content: "Read synthetic evidence."}}, MaxOutputTokens: connection.Limits.OutputTokens, Tools: []chat.Tool{
				{Name: "read_note", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"revision":{"type":["string","null"]}},"required":["path","revision"],"additionalProperties":false}`)},
				{Name: "search_passages", Schema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"},"scope":{"type":["string","null"]}},"required":["text","scope"],"additionalProperties":false}`)},
			}}
			response, err := provider.Generate(t.Context(), request, nil)
			if err != nil || len(response.Message.Continuation) == 0 || len(response.Message.ToolCalls) != 2 {
				t.Fatalf("native fixture response: calls=%d continuation=%d err=%v", len(response.Message.ToolCalls), len(response.Message.Continuation), err)
			}
			nativeAssistant := response.Message
			nativeAssistant.Continuation = bytes.Clone(response.Message.Continuation)
			nativeAssistant.ToolCalls = append([]chat.ToolCall(nil), response.Message.ToolCalls...)
			for i := range nativeAssistant.ToolCalls {
				nativeAssistant.ToolCalls[i].Arguments = bytes.Clone(response.Message.ToolCalls[i].Arguments)
			}
			run := chat.Run{ID: "synthetic-run", Messages: []chat.Message{{Role: "user", Content: nativeBudgetUser(t, nativeBudgetSources("automatic", 8, 250), "MANDATORY_SYNTHETIC_PRIVATE_ATTACHMENT")}, response.Message}}
			for i, call := range response.Message.ToolCalls {
				sources := nativeBudgetSources(fmt.Sprintf("tool%d", i), 8, 250)
				full := nativeBudgetJSON(t, map[string]any{"path": "notes/synthetic-native.md", "content": strings.Repeat("FULL_DURABLE_SYNTHETIC_RESULT ", 3000), "revision": "synthetic-revision"})
				run.Operations = append(run.Operations, chat.Operation{ID: call.ID, ToolCallID: call.ID, Round: 1, Name: call.Name, Arguments: call.Arguments, State: "completed", Result: full, Citations: sources})
				run.Messages = append(run.Messages, chat.Message{Role: "tool", ToolCallID: call.ID, Content: string(nativeBudgetJSON(t, map[string]any{"kind": []string{"note_read", "passage_search"}[i], "scope": "selected committed evidence; not complete notebook coverage", "sources": sources}))})
			}
			request.Messages = run.Messages
			beforeRun, beforeRequest := nativeBudgetJSON(t, run), nativeBudgetJSON(t, request)
			fitted, fit, err := fitChatRequest(run, request, connection.Limits.ContextTokens, provider.EstimateRequest)
			if err != nil {
				t.Fatalf("optional evidence should fit: %v", err)
			}
			if !fit.Changed || fit.AfterTokens >= fit.BeforeTokens || fit.DroppedHistory != 0 {
				t.Fatalf("expected evidence-only reduction preserving the native cycle: %+v", fit)
			}
			if len(fitted.Messages) != len(request.Messages) || !reflect.DeepEqual(fitted.Messages[1], nativeAssistant) {
				t.Fatal("fitting changed or dropped native assistant continuation, arguments, or call ordering")
			}
			beforeTokens, beforeErr := provider.EstimateRequest(request)
			afterTokens, afterErr := provider.EstimateRequest(fitted)
			if beforeErr != nil || afterErr != nil || fit.BeforeTokens != beforeTokens || fit.AfterTokens != afterTokens {
				t.Fatalf("fit estimates differ from native request: fit=%+v before=%d/%v after=%d/%v", fit, beforeTokens, beforeErr, afterTokens, afterErr)
			}
			if fitted.MaxOutputTokens != connection.Limits.OutputTokens || fit.AfterTokens+fitted.MaxOutputTokens > connection.Limits.ContextTokens {
				t.Fatalf("context fit consumed configured output headroom: output=%d fit=%+v", fitted.MaxOutputTokens, fit)
			}
			if !strings.Contains(fitted.Messages[0].Content, "CURRENT_SYNTHETIC_TASK") || !strings.Contains(fitted.Messages[0].Content, "MANDATORY_SYNTHETIC_PRIVATE_ATTACHMENT") {
				t.Fatal("fitting removed the current task or explicit attachment")
			}
			nativeBudgetAssertMandatory(t, request.Messages[0].Content, fitted.Messages[0].Content)
			for i := 2; i < len(fitted.Messages); i++ {
				if fitted.Messages[i].ToolCallID != request.Messages[i].ToolCallID || len(fitted.Messages[i].Content) >= len(request.Messages[i].Content) || !json.Valid([]byte(fitted.Messages[i].Content)) {
					t.Fatalf("tool result %d was not compacted into valid paired evidence", i)
				}
			}
			if !bytes.Equal(beforeRun, nativeBudgetJSON(t, run)) || !bytes.Equal(beforeRequest, nativeBudgetJSON(t, request)) {
				t.Fatal("fitting mutated durable messages, operations, full results, or its input request")
			}
			if _, err = provider.Generate(t.Context(), fitted, nil); err != nil {
				t.Fatalf("fitted native continuation could not reach provider: %v", err)
			}
			nativeBudgetAssertWire(t, format, <-wire, nativeAssistant, fitted)
			if count.Load() != 2 {
				t.Fatalf("provider requests=%d, want two", count.Load())
			}
		})
	}
}

func nativeBudgetAssertMandatory(t *testing.T, before, after string) {
	t.Helper()
	decode := func(content string) (string, map[string]json.RawMessage) {
		prefix, payload, ok := strings.Cut(content, "\n<untrusted_notebook_context>\n")
		if !ok || !strings.HasSuffix(payload, "\n</untrusted_notebook_context>") {
			t.Fatal("fitting lost the notebook context boundary")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(strings.TrimSuffix(payload, "\n</untrusted_notebook_context>")), &fields); err != nil {
			t.Fatalf("fitting damaged notebook JSON: %v", err)
		}
		delete(fields, "sources")
		return prefix, fields
	}
	wantPrefix, wantFields := decode(before)
	gotPrefix, gotFields := decode(after)
	if wantPrefix != gotPrefix || !bytes.Equal(nativeBudgetJSON(t, wantFields), nativeBudgetJSON(t, gotFields)) {
		t.Fatal("fitting changed the task, notebook guidance, explicit attachments, or mandatory context metadata")
	}
}

func nativeBudgetAssertWire(t *testing.T, format string, body map[string]json.RawMessage, assistant chat.Message, request chat.Request) {
	t.Helper()
	field, output := "messages", "max_tokens"
	allowed := map[string]bool{"model": true, "tools": true}
	switch format {
	case "responses":
		field, output = "input", "max_output_tokens"
		allowed["instructions"], allowed["store"], allowed["include"] = true, true, true
	case "messages":
		allowed["system"], allowed["thinking"] = true, true
	default:
		allowed["thinking"] = true
	}
	allowed[field], allowed[output] = true, true
	for name := range body {
		if !allowed[name] {
			t.Errorf("unconfigured native field %q", name)
		}
	}
	if string(body[output]) != fmt.Sprint(request.MaxOutputTokens) {
		t.Errorf("native output ceiling changed: %s", body[output])
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(body[field], &messages); err != nil {
		t.Fatal(err)
	}
	results := map[string]string{}
	var replayed []json.RawMessage
	var nativeAssistant map[string]json.RawMessage
	for _, message := range messages {
		var kind, role string
		json.Unmarshal(message["type"], &kind)
		json.Unmarshal(message["role"], &role)
		if format == "responses" {
			if kind == "function_call_output" {
				var id, text string
				json.Unmarshal(message["call_id"], &id)
				json.Unmarshal(message["output"], &text)
				results[id] = text
			} else if kind != "" {
				replayed = append(replayed, nativeBudgetJSON(t, message))
			}
		} else if role == "assistant" {
			nativeAssistant = message
		} else if format == "messages" && role == "user" {
			var blocks []map[string]json.RawMessage
			json.Unmarshal(message["content"], &blocks)
			for _, block := range blocks {
				if string(block["type"]) == `"tool_result"` {
					var id, text string
					json.Unmarshal(block["tool_use_id"], &id)
					json.Unmarshal(block["content"], &text)
					results[id] = text
				}
			}
		} else if role == "tool" {
			var id, text string
			json.Unmarshal(message["tool_call_id"], &id)
			json.Unmarshal(message["content"], &text)
			results[id] = text
		}
	}
	if len(results) != len(assistant.ToolCalls) {
		t.Fatalf("native tool call/result pairing lost: got %d results", len(results))
	}
	for _, message := range request.Messages {
		if message.Role == "tool" && results[message.ToolCallID] != message.Content {
			t.Errorf("native result %q changed or lost its matching call", message.ToolCallID)
		}
	}
	var expected, actual any
	if format == "responses" {
		json.Unmarshal(assistant.Continuation, &expected)
		json.Unmarshal(nativeBudgetJSON(t, replayed), &actual)
	} else if format == "messages" {
		json.Unmarshal(assistant.Continuation, &expected)
		json.Unmarshal(nativeAssistant["content"], &actual)
	} else {
		var continuation map[string]string
		json.Unmarshal(assistant.Continuation, &continuation)
		var reasoning string
		json.Unmarshal(nativeAssistant["reasoning_content"], &reasoning)
		if reasoning != continuation["reasoning_content"] {
			t.Fatal("DeepSeek reasoning_content changed during fitted native replay")
		}
		var calls []struct {
			ID       string                           `json:"id"`
			Function struct{ Name, Arguments string } `json:"function"`
		}
		if err := json.Unmarshal(nativeAssistant["tool_calls"], &calls); err != nil || len(calls) != len(assistant.ToolCalls) {
			t.Fatalf("native calls lost: %v", err)
		}
		for i, call := range calls {
			want := assistant.ToolCalls[i]
			if call.ID != want.ID || call.Function.Name != want.Name || call.Function.Arguments != string(want.Arguments) {
				t.Errorf("native call %d changed exact arguments or identity", i)
			}
		}
		return
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatal("native reasoning, signatures, opaque items, or tool calls changed on the wire")
	}
}

func TestChatContextNativeBudgetRejectsMandatoryOverflow(t *testing.T) {
	for _, kind := range []string{"native-continuation", "unknown-tool-content", "unknown-tool-source-envelope", "explicit-attachment"} {
		t.Run(kind, func(t *testing.T) {
			call := chat.ToolCall{ID: "mandatory-read", Name: "read_note", Arguments: json.RawMessage(`{"path":"notes/synthetic-mandatory.md"}`)}
			run := chat.Run{Messages: []chat.Message{{Role: "user", Content: nativeBudgetUser(t, nil, "SYNTHETIC_ATTACHMENT")}, {Role: "assistant", ToolCalls: []chat.ToolCall{call}}, {Role: "tool", ToolCallID: call.ID, Content: `{"sources":[]}`}}}
			switch kind {
			case "native-continuation":
				run.Messages[1].Continuation = nativeBudgetJSON(t, map[string]string{"reasoning_content": strings.Repeat("SYNTHETIC_MANDATORY_REASONING ", 3000)})
			case "unknown-tool-content":
				run.Messages[2].Content = string(nativeBudgetJSON(t, map[string]string{"unrecognized_result": strings.Repeat("SYNTHETIC_UNKNOWN_RESULT ", 3000)}))
			case "unknown-tool-source-envelope":
				run.Messages[1].ToolCalls[0].Name = "project_status"
				run.Messages[2].Content = string(nativeBudgetJSON(t, map[string]any{"kind": "note_read", "sources": nativeBudgetSources("unknown", 8, 250)}))
			case "explicit-attachment":
				run.Messages[0].Content = nativeBudgetUser(t, nil, strings.Repeat("SYNTHETIC_MANDATORY_ATTACHMENT ", 3000))
			}
			request := chat.Request{Model: "synthetic", System: "Synthetic system", Messages: run.Messages, MaxOutputTokens: 1536}
			beforeRun, beforeRequest := nativeBudgetJSON(t, run), nativeBudgetJSON(t, request)
			if _, _, err := fitChatRequest(run, request, 8192); err == nil || !strings.Contains(strings.ToLower(err.Error()), "context") {
				t.Fatalf("mandatory oversized content needs an explicit context failure: %v", err)
			}
			if !bytes.Equal(beforeRun, nativeBudgetJSON(t, run)) || !bytes.Equal(beforeRequest, nativeBudgetJSON(t, request)) {
				t.Fatal("failed fitting mutated durable history or input request")
			}
		})
	}
}
