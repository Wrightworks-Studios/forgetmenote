package chat

import (
	"encoding/json"
	"io"
	"net/http"
)

// providerWire owns one supported external protocol. Provider retains credentials,
// admission, cancellation, capabilities and usage validation for all formats.
type providerWire interface {
	encode(Request, Capabilities, map[string]any) (map[string]any, string, error)
	authorize(http.Header, string)
	decode(*Provider, []byte) (Response, error)
	stream(*Provider, io.Reader, func(Event) error) (Response, error)
}

func selectProviderWire(format string) (providerWire, error) {
	switch format {
	case "responses":
		return responsesFormat{}, nil
	case "messages":
		return messagesFormat{}, nil
	case "chat-completions":
		return completionFormat{}, nil
	case "cloudflare-moondream":
		return moondreamFormat{}, nil
	case "cloudflare-mistral":
		return mistralFormat{}, nil
	default:
		return nil, providerContractError("unsupported configured provider format")
	}
}

// Existing transcripts persist opaque continuations alongside semantic text and
// tool calls. Their native block kinds identify the protocol without rewriting
// private history or introducing another persisted schema. Never reinterpret one
// protocol's encrypted reasoning or signed thinking as another's input.
func validateContinuation(raw json.RawMessage, format string) error {
	if len(raw) == 0 {
		return nil
	}
	if format == "chat-completions" {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return errProviderData
		}
		var reasoning string
		if value, ok := fields["reasoning_content"]; !ok || json.Unmarshal(value, &reasoning) != nil {
			return errProviderData
		}
		return nil
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &blocks) != nil || blocks == nil {
		return errProviderData
	}
	for _, block := range blocks {
		supported := false
		if format == "responses" {
			supported = block.Type == "reasoning" || block.Type == "message" || block.Type == "function_call"
		} else if format == "messages" {
			supported = block.Type == "text" || block.Type == "thinking" || block.Type == "redacted_thinking" || block.Type == "tool_use"
		}
		if !supported {
			return providerContractError("saved continuation belongs to another provider format")
		}
	}
	return nil
}
