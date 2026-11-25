package chat

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
)

type completionFormat struct{}

func (completionFormat) encode(req Request, caps Capabilities, body map[string]any) (map[string]any, string, error) {
	messages := []any{}
	if req.System != "" {
		messages = append(messages, map[string]any{"role": "system", "content": req.System})
	}
	for _, m := range req.Messages {
		if len(m.Continuation) > 0 && !caps.ReasoningContent {
			return nil, "", providerContractError("saved reasoning requires the configured provider's continuation capability")
		}
		v := map[string]any{"role": m.Role, "content": m.Content}
		if len(m.Images) > 0 {
			blocks := []any{map[string]any{"type": "text", "text": m.Content}}
			for _, data := range m.Images {
				blocks = append(blocks, map[string]any{"type": "image_url", "image_url": map[string]string{"url": data, "detail": "auto"}})
			}
			v["content"] = blocks
		}
		if m.Role == "tool" {
			v["tool_call_id"] = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			calls := []any{}
			for _, c := range m.ToolCalls {
				calls = append(calls, map[string]any{"id": c.ID, "type": "function", "function": map[string]any{"name": c.Name, "arguments": string(c.Arguments)}})
			}
			v["tool_calls"] = calls
		}
		if len(m.Continuation) > 0 && caps.ReasoningContent {
			var cont map[string]json.RawMessage
			if json.Unmarshal(m.Continuation, &cont) != nil {
				return nil, "", errProviderData
			}
			if raw, ok := cont["reasoning_content"]; ok {
				var text string
				if json.Unmarshal(raw, &text) != nil {
					return nil, "", errProviderData
				}
				v["reasoning_content"] = text
			}
		}
		messages = append(messages, v)
	}
	body["messages"] = messages
	if req.JSONOutput {
		body["response_format"] = map[string]string{"type": "json_object"}
	}
	if caps.StreamUsage {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if caps.Thinking != "" {
		body["thinking"] = map[string]any{"type": caps.Thinking}
	}
	if caps.ReasoningEffort != "" {
		body["reasoning_effort"] = caps.ReasoningEffort
	}
	tools := []any{}
	for _, t := range req.Tools {
		f := map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Schema}
		if caps.StrictTools {
			f["strict"] = true
		}
		tools = append(tools, map[string]any{"type": "function", "function": f})
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	return body, "/chat/completions", nil
}
func (completionFormat) authorize(header http.Header, credential string) {
	if credential != "" {
		header.Set("Authorization", "Bearer "+credential)
	}
}
func (completionFormat) decode(p *Provider, raw []byte) (Response, error) {
	var v completionWire
	if err := providerJSON(raw, &v); err != nil {
		return Response{}, err
	}
	if len(v.Choices) != 1 || v.Choices[0].Message.Role != "assistant" {
		return Response{}, errProviderData
	}
	r, err := p.parseCompletion(v.Choices[0].Message, v.Choices[0].Finish)
	r.Usage = v.Usage.normalized("chat-completions")
	r.usageFinal = v.Usage.hasTotals("chat-completions") && v.Choices[0].Finish != ""
	return r, err
}
func (completionFormat) stream(p *Provider, reader io.Reader, emit func(Event) error) (Response, error) {
	return p.streamCompletion(reader, emit)
}

type chatWireMessage struct {
	Role      string  `json:"role"`
	Content   *string `json:"content"`
	Reasoning *string `json:"reasoning_content"`
	ToolCalls []struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}
type completionWire struct {
	Choices []struct {
		Index   int             `json:"index"`
		Message chatWireMessage `json:"message"`
		Delta   chatWireMessage `json:"delta"`
		Finish  string          `json:"finish_reason"`
	} `json:"choices"`
	Usage nativeUsage `json:"usage"`
}

func (p *Provider) parseCompletion(m chatWireMessage, finish string) (Response, error) {
	r := Response{Message: Message{Role: "assistant"}, Finish: finish}
	if m.Content != nil {
		r.Message.Content = *m.Content
	}
	for _, c := range m.ToolCalls {
		if c.Type != "function" {
			return r, errProviderData
		}
		r.Message.ToolCalls = append(r.Message.ToolCalls, ToolCall{ID: c.ID, Name: c.Function.Name, Arguments: json.RawMessage(c.Function.Arguments)})
	}
	if m.Reasoning != nil {
		r.observedReasoningBytes = len(*m.Reasoning)
		r.observedOutputBytes = len(r.Message.Content) + r.observedReasoningBytes
		for _, call := range r.Message.ToolCalls {
			r.observedOutputBytes += len(call.Arguments)
		}
		if p.connection.Capabilities.ReasoningContent {
			r.Message.Continuation, _ = json.Marshal(map[string]string{"reasoning_content": *m.Reasoning})
		} else if len(m.ToolCalls) > 0 && *m.Reasoning != "" {
			return r, providerContractError("provider requires reasoning continuation that is not configured")
		}
	}
	if finish != "stop" && finish != "tool_calls" {
		return r, providerContractError("provider response did not complete")
	}
	if finish == "tool_calls" && len(r.Message.ToolCalls) == 0 {
		return r, errProviderData
	}
	if m.Content == nil && len(m.ToolCalls) == 0 {
		return r, errProviderData
	}
	return r, nil
}
func (p *Provider) streamCompletion(reader io.Reader, emit func(Event) error) (result Response, streamErr error) {
	result = Response{Message: Message{Role: "assistant"}}
	observed := providerObservation{limit: p.outputBytes()}
	defer func() { observed.apply(&result) }()
	content, reasoning := "", ""
	reasoningSeen := false
	calls := map[int]ToolCall{}
	finish := ""
	seen := false
	finalUsageReceived := false
	collect := func() {
		result.Message.Content = content
		result.Finish = finish
		result.Message.ToolCalls = nil
		indexes := []int{}
		for i := range calls {
			indexes = append(indexes, i)
		}
		sort.Ints(indexes)
		for _, i := range indexes {
			result.Message.ToolCalls = append(result.Message.ToolCalls, calls[i])
		}
		if reasoningSeen && p.connection.Capabilities.ReasoningContent {
			result.Message.Continuation, _ = json.Marshal(map[string]string{"reasoning_content": reasoning})
		}
	}
	err := scanProviderSSE(reader, p.outputBytes(), func(raw []byte) (bool, error) {
		if string(raw) == "[DONE]" {
			collect()
			result.usageFinal = finalUsageReceived && finish != ""
			if !seen || (finish != "stop" && finish != "tool_calls") || (finish == "tool_calls" && len(calls) == 0) {
				return false, errProviderData
			}
			if reasoning != "" && len(calls) > 0 && !p.connection.Capabilities.ReasoningContent {
				return false, errProviderData
			}
			return true, nil
		}
		var e completionWire
		if err := providerJSON(raw, &e); err != nil {
			return false, err
		}
		if len(e.Choices) > 1 {
			return false, errProviderData
		}
		usageAtTerminal := finish != ""
		for _, choice := range e.Choices {
			if choice.Finish != "" {
				usageAtTerminal = true
			}
		}
		reported := e.Usage.normalized("chat-completions")
		if e.Usage.hasTotals("chat-completions") && usageAtTerminal {
			result.Usage = reported
			finalUsageReceived = true
		} else {
			result.Usage = providerUsageLowerBound(result.Usage, reported)
		}
		for _, choice := range e.Choices {
			if choice.Index != 0 || finish != "" {
				return false, errProviderData
			}
			seen = true
			if choice.Delta.Content != nil {
				delta := *choice.Delta.Content
				if err := observed.add(delta, false); err != nil {
					return false, err
				}
				content += delta
				if len(content) > p.outputBytes() {
					return false, errProviderLimit
				}
			}
			if choice.Delta.Reasoning != nil {
				if err := observed.add(*choice.Delta.Reasoning, true); err != nil {
					return false, err
				}
				reasoningSeen = true
				reasoning += *choice.Delta.Reasoning
				if len(reasoning) > p.outputBytes() {
					return false, errProviderLimit
				}
			}
			for _, delta := range choice.Delta.ToolCalls {
				if err := observed.add(delta.Function.Arguments, false); err != nil {
					return false, err
				}
				if delta.Index < 0 || delta.Index > 1024 {
					return false, errProviderData
				}
				call := calls[delta.Index]
				if delta.ID != "" {
					if call.ID != "" && call.ID != delta.ID {
						return false, errProviderData
					}
					call.ID = delta.ID
				}
				if delta.Type != "" && delta.Type != "function" {
					return false, errProviderData
				}
				call.Name += delta.Function.Name
				call.Arguments = append(call.Arguments, []byte(delta.Function.Arguments)...)
				if len(call.Arguments) > p.outputBytes() || len(call.Name) > 64 {
					return false, errProviderLimit
				}
				calls[delta.Index] = call
			}
			if choice.Finish != "" {
				finish = choice.Finish
			}
			if choice.Delta.Content != nil {
				if err := emit(Event{Type: "text_delta", Text: *choice.Delta.Content}); err != nil {
					return false, err
				}
			}
		}
		return false, nil
	})
	collect()
	return result, err
}
