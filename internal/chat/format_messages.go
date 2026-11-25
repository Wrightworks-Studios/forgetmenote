package chat

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
)

type messagesFormat struct{}

func (messagesFormat) encode(req Request, caps Capabilities, body map[string]any) (map[string]any, string, error) {
	messages := []any{}
	for _, m := range req.Messages {
		role := m.Role
		blocks := []any{}
		if role == "tool" {
			role = "user"
			blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content})
		} else if len(m.Continuation) > 0 {
			var native []json.RawMessage
			if json.Unmarshal(m.Continuation, &native) != nil {
				return nil, "", errProviderData
			}
			for _, b := range native {
				blocks = append(blocks, b)
			}
		} else {
			if m.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, c := range m.ToolCalls {
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": c.Arguments})
			}
		}
		// Messages requires simultaneous tool results in one user content array.
		if len(messages) > 0 && messages[len(messages)-1].(map[string]any)["role"] == role {
			prior := messages[len(messages)-1].(map[string]any)
			prior["content"] = append(prior["content"].([]any), blocks...)
		} else {
			messages = append(messages, map[string]any{"role": role, "content": blocks})
		}
	}
	body["messages"] = messages
	if req.System != "" {
		if caps.CacheControl {
			body["system"] = []any{map[string]any{"type": "text", "text": req.System, "cache_control": map[string]any{"type": "ephemeral"}}}
		} else {
			body["system"] = req.System
		}
	}
	if caps.Thinking != "" {
		thinking := map[string]any{"type": caps.Thinking}
		if caps.Thinking == "enabled" {
			thinking["budget_tokens"] = caps.ThinkingBudget
		}
		body["thinking"] = thinking
	}
	tools := []any{}
	for _, t := range req.Tools {
		v := map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.Schema}
		if caps.StrictTools {
			v["strict"] = true
		}
		tools = append(tools, v)
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if req.FinalAnswer {
		body["tool_choice"] = map[string]any{"type": "none"}
	}
	return body, "/messages", nil

}
func (messagesFormat) authorize(header http.Header, credential string) {
	header.Set("anthropic-version", "2023-06-01")
	if credential != "" {
		header.Set("x-api-key", credential)
	}
}
func (messagesFormat) decode(p *Provider, raw []byte) (Response, error) {
	var v messagesWire
	if err := providerJSON(raw, &v); err != nil {
		return Response{}, err
	}
	return p.parseMessages(v)
}
func (messagesFormat) stream(p *Provider, reader io.Reader, emit func(Event) error) (Response, error) {
	return p.streamMessages(reader, emit)
}

type messagesWire struct {
	Type       string            `json:"type"`
	Role       string            `json:"role"`
	Content    []json.RawMessage `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      nativeUsage       `json:"usage"`
}

func (p *Provider) parseMessages(v messagesWire) (Response, error) {
	r := Response{Message: Message{Role: "assistant"}, Usage: v.Usage.normalized("messages"), Finish: v.StopReason}
	r.usageFinal = v.Usage.hasTotals("messages") && v.Type == "message" && v.StopReason != ""
	r.Message.Continuation, _ = json.Marshal(v.Content)
	for _, raw := range v.Content {
		var block nativeBlock
		if err := providerJSON(raw, &block); err != nil {
			return r, err
		}
		switch block.Type {
		case "text":
			r.Message.Content += block.Text
		case "tool_use":
			r.Message.ToolCalls = append(r.Message.ToolCalls, ToolCall{ID: block.ID, Name: block.Name, Arguments: block.Input})
		case "thinking", "redacted_thinking":
		default:
			return r, errProviderData
		}
	}
	if v.Type != "message" || v.Role != "assistant" || len(v.Content) == 0 {
		return r, errProviderData
	}
	if v.StopReason != "end_turn" && v.StopReason != "tool_use" && v.StopReason != "stop_sequence" {
		return r, providerContractError("provider response did not complete")
	}
	if v.StopReason == "tool_use" && len(r.Message.ToolCalls) == 0 {
		return r, errProviderData
	}
	return r, nil
}
func (p *Provider) streamMessages(reader io.Reader, emit func(Event) error) (result Response, streamErr error) {
	result = Response{Message: Message{Role: "assistant"}}
	observed := providerObservation{limit: p.outputBytes()}
	defer func() { observed.apply(&result) }()
	wire := messagesWire{Type: "message", Role: "assistant"}
	started := false
	outputUsageAtDelta := false
	terminalReceived := false
	blocks := map[int]map[string]json.RawMessage{}
	partial := map[int]string{}
	closed := map[int]bool{}
	collect := func() {
		keys := make([]int, 0, len(blocks))
		for i := range blocks {
			keys = append(keys, i)
		}
		sort.Ints(keys)
		wire.Content = nil
		for _, i := range keys {
			raw, _ := json.Marshal(blocks[i])
			wire.Content = append(wire.Content, raw)
		}
		result, _ = p.parseMessages(wire)
	}
	err := scanProviderSSE(reader, p.outputBytes(), func(raw []byte) (bool, error) {
		var e struct {
			Type         string                     `json:"type"`
			Index        int                        `json:"index"`
			Message      messagesWire               `json:"message"`
			ContentBlock map[string]json.RawMessage `json:"content_block"`
			Delta        map[string]json.RawMessage `json:"delta"`
			Usage        json.RawMessage            `json:"usage"`
		}
		if err := providerJSON(raw, &e); err != nil {
			return false, err
		}
		switch e.Type {
		case "ping":
			return false, nil
		case "message_start":
			if started || e.Message.Role != "assistant" {
				return false, errProviderData
			}
			started = true
			wire.Usage = e.Message.Usage
			result.Usage = wire.Usage.normalized("messages")
		case "content_block_start":
			if !started || e.Index < 0 || e.Index > 1024 || blocks[e.Index] != nil || e.ContentBlock == nil {
				return false, errProviderData
			}
			blocks[e.Index] = e.ContentBlock
			var kind, text string
			json.Unmarshal(e.ContentBlock["type"], &kind)
			switch kind {
			case "thinking":
				var thought, signature string
				json.Unmarshal(e.ContentBlock["thinking"], &thought)
				json.Unmarshal(e.ContentBlock["signature"], &signature)
				if err := observed.add(thought, true); err != nil {
					return false, err
				}
				if err := observed.add(signature, false); err != nil {
					return false, err
				}
			case "redacted_thinking":
				if err := observed.add(string(e.ContentBlock["data"]), true); err != nil {
					return false, err
				}
			case "tool_use":
				if err := observed.add(string(e.ContentBlock["input"]), false); err != nil {
					return false, err
				}
			}
			if kind == "text" {
				json.Unmarshal(e.ContentBlock["text"], &text)
				if err := observed.add(text, false); err != nil {
					return false, err
				}
				if text != "" {
					result.Message.Content += text
					return false, emit(Event{Type: "text_delta", Text: text})
				}
			}
		case "content_block_delta":
			block := blocks[e.Index]
			if block == nil || closed[e.Index] {
				return false, errProviderData
			}
			var typ string
			if json.Unmarshal(e.Delta["type"], &typ) != nil {
				return false, errProviderData
			}
			var field, source string
			switch typ {
			case "text_delta":
				field = "text"
				source = "text"
			case "thinking_delta":
				field = "thinking"
				source = "thinking"
			case "signature_delta":
				field = "signature"
				source = "signature"
			case "input_json_delta":
				var fragment string
				if json.Unmarshal(e.Delta["partial_json"], &fragment) != nil {
					return false, errProviderData
				}
				if err := observed.add(fragment, false); err != nil {
					return false, err
				}
				partial[e.Index] += fragment
				if len(partial[e.Index]) > p.outputBytes() {
					return false, errProviderLimit
				}
				return false, nil
			default:
				return false, errProviderData
			}
			var before, delta string
			json.Unmarshal(block[field], &before)
			if json.Unmarshal(e.Delta[source], &delta) != nil {
				return false, errProviderData
			}
			if err := observed.add(delta, typ == "thinking_delta"); err != nil {
				return false, err
			}
			if len(before)+len(delta) > p.outputBytes() {
				return false, errProviderLimit
			}
			block[field], _ = json.Marshal(before + delta)
			if typ == "text_delta" {
				result.Message.Content += delta
				return false, emit(Event{Type: "text_delta", Text: delta})
			}
		case "content_block_stop":
			block := blocks[e.Index]
			if block == nil || closed[e.Index] {
				return false, errProviderData
			}
			if text, ok := partial[e.Index]; ok {
				var input map[string]json.RawMessage
				if json.Unmarshal([]byte(text), &input) != nil || input == nil {
					return false, errProviderData
				}
				block["input"] = json.RawMessage(text)
			}
			closed[e.Index] = true
		case "message_delta":
			if !started {
				return false, errProviderData
			}
			if value, ok := e.Delta["stop_reason"]; ok {
				if json.Unmarshal(value, &wire.StopReason) != nil {
					return false, errProviderData
				}
			}
			if len(e.Usage) > 0 {
				var u map[string]json.RawMessage
				if json.Unmarshal(e.Usage, &u) != nil {
					return false, errProviderData
				}
				for key, dst := range map[string]*int64{"output_tokens": &wire.Usage.OutputTokens, "input_tokens": &wire.Usage.InputTokens, "cache_creation_input_tokens": &wire.Usage.CacheWrite, "cache_read_input_tokens": &wire.Usage.CacheRead} {
					if raw, ok := u[key]; ok {
						if json.Unmarshal(raw, dst) != nil {
							return false, errProviderData
						}
						if key == "output_tokens" && string(raw) != "null" {
							wire.Usage.outputPresent = true
							outputUsageAtDelta = true
						}
						if key == "input_tokens" && string(raw) != "null" {
							wire.Usage.inputPresent = true
						}
					}
				}
				result.Usage = wire.Usage.normalized("messages")
			}
		case "message_stop":
			if !started || len(blocks) == 0 {
				return false, errProviderData
			}
			for i := range blocks {
				if !closed[i] {
					return false, errProviderData
				}
			}
			terminalReceived = true
			collect()
			var err error
			result, err = p.parseMessages(wire)
			result.usageFinal = result.usageFinal && outputUsageAtDelta
			return true, err
		case "error":
			return false, errProviderData
		default:
			return false, errProviderData
		}
		return false, nil
	})
	if err != nil {
		collect()
		result.usageFinal = terminalReceived && outputUsageAtDelta && wire.Usage.hasTotals("messages")
	}
	return result, err
}
