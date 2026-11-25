package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestProviderRefusesContinuationFromAnotherWireFormat(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"type":"message","role":"assistant","content":[{"type":"text","text":"accepted"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()
	responses, err := NewProvider(providerTestConnection(server.URL, "responses"))
	if err != nil {
		t.Fatal(err)
	}
	original, err := responses.parseResponse([]byte(`{"status":"completed","output":[{"type":"reasoning","encrypted_content":"private"},{"type":"message","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	// A durable round trip must retain the native history's provenance.
	raw, err := json.Marshal(original.Message)
	if err != nil {
		t.Fatal(err)
	}
	var restored Message
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	messages, err := NewProvider(providerTestConnection(server.URL, "messages"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = messages.Generate(context.Background(), Request{Messages: []Message{{Role: "user", Content: "question"}, restored}}, nil)
	if !errors.Is(err, ErrProviderContract) || calls.Load() != 0 {
		t.Fatalf("native history escaped to another format: calls=%d err=%v", calls.Load(), err)
	}
}

func TestProviderRefusesSavedReasoningWhenReplayCapabilityIsDisabled(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"accepted"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer server.Close()
	connection := providerTestConnection(server.URL, "chat-completions")
	connection.Capabilities.ReasoningContent = true
	provider, err := NewProvider(connection)
	if err != nil {
		t.Fatal(err)
	}
	original, err := provider.parseResponse([]byte(`{"choices":[{"message":{"role":"assistant","content":"thinking","reasoning_content":"private signed state","tool_calls":[{"id":"call","type":"function","function":{"name":"read_note","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(original.Message)
	var restored Message
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	connection.Capabilities.ReasoningContent = false
	provider, err = NewProvider(connection)
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Generate(t.Context(), Request{Messages: []Message{{Role: "user", Content: "question"}, restored, {Role: "tool", ToolCallID: "call", Content: "found"}}}, nil)
	if !errors.Is(err, ErrProviderContract) || calls.Load() != 0 {
		t.Fatalf("saved reasoning discarded: calls=%d err=%v", calls.Load(), err)
	}
}
