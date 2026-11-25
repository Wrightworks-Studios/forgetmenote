package chat

import (
	"encoding/json"
	"io"
	"net/http"
)

type responsesFormat struct{}

func (responsesFormat) encode(req Request, caps Capabilities, body map[string]any) (map[string]any, string, error) {
	if req.JSONOutput {
		body["text"] = map[string]any{"format": map[string]string{"type": "json_object"}}
	}
	input := []any{}
	for _, m := range req.Messages {
		if m.Role == "tool" {
			input = append(input, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": m.Content})
			continue
		}
		if len(m.Continuation) > 0 {
			var items []json.RawMessage
			if json.Unmarshal(m.Continuation, &items) != nil {
				return nil, "", errProviderData
			}
			for _, item := range items {
				input = append(input, item)
			}
			continue
		}
		if m.Content != "" || len(m.Images) > 0 {
			var content any = m.Content
			if len(m.Images) > 0 {
				blocks := []any{map[string]any{"type": "input_text", "text": m.Content}}
				for _, data := range m.Images {
					blocks = append(blocks, map[string]any{"type": "input_image", "image_url": data, "detail": "auto"})
				}
				content = blocks
			}
			input = append(input, map[string]any{"role": m.Role, "content": content})
		}
		for _, c := range m.ToolCalls {
			input = append(input, map[string]any{"type": "function_call", "call_id": c.ID, "name": c.Name, "arguments": string(c.Arguments)})
		}
	}
	body["input"] = input
	if req.System != "" {
		body["instructions"] = req.System
	}
	if caps.StoreFalse {
		body["store"] = false
	}
	if caps.EncryptedReasoning {
		body["include"] = []string{"reasoning.encrypted_content"}
	}
	if caps.ReasoningEffort != "" {
		body["reasoning"] = map[string]any{"effort": caps.ReasoningEffort}
	}
	tools := []any{}
	for _, t := range req.Tools {
		v := map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": t.Schema}
		if caps.StrictTools {
			v["strict"] = true
		}
		tools = append(tools, v)
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if req.FinalAnswer {
		body["tool_choice"] = "none"
	}
	return body, "/responses", nil

}
func (responsesFormat) authorize(header http.Header, credential string) {
	if credential != "" {
		header.Set("Authorization", "Bearer "+credential)
	}
}
func (responsesFormat) decode(p *Provider, raw []byte) (Response, error) {
	var v responsesWire
	if err := providerJSON(raw, &v); err != nil {
		return Response{}, err
	}
	return p.parseResponses(v)
}
func (responsesFormat) stream(p *Provider, reader io.Reader, emit func(Event) error) (Response, error) {
	return p.streamResponses(reader, emit)
}

type responsesWire struct {
	Status string            `json:"status"`
	Output []json.RawMessage `json:"output"`
	Usage  nativeUsage       `json:"usage"`
}

func (p *Provider) parseResponses(v responsesWire) (Response, error) {
	r := Response{Message: Message{Role: "assistant"}, Usage: v.Usage.normalized("responses"), Finish: v.Status}
	r.usageFinal = v.Usage.hasTotals("responses") && (v.Status == "completed" || v.Status == "incomplete" || v.Status == "failed")
	r.Message.Continuation, _ = json.Marshal(v.Output)
	for _, raw := range v.Output {
		var block nativeBlock
		if err := providerJSON(raw, &block); err != nil {
			return r, err
		}
		switch block.Type {
		case "message":
			for _, c := range block.Content {
				if c.Type == "output_text" {
					r.Message.Content += c.Text
				}
				if c.Type == "refusal" {
					return r, providerContractError("provider declined the request")
				}
			}
		case "function_call":
			r.Message.ToolCalls = append(r.Message.ToolCalls, ToolCall{ID: block.CallID, Name: block.Name, Arguments: json.RawMessage(block.Arguments)})
		case "reasoning":
		default:
			return r, errProviderData
		}
	}
	if v.Status != "completed" {
		return r, providerContractError("provider response did not complete")
	}
	if len(v.Output) == 0 {
		return r, errProviderData
	}
	return r, nil
}
func (p *Provider) streamResponses(reader io.Reader, emit func(Event) error) (result Response, streamErr error) {
	result = Response{Message: Message{Role: "assistant"}}
	observed := providerObservation{limit: p.outputBytes()}
	defer func() { observed.apply(&result) }()
	arguments := map[int]string{}
	err := scanProviderSSE(reader, p.outputBytes(), func(raw []byte) (bool, error) {
		var e struct {
			Type     string          `json:"type"`
			Delta    string          `json:"delta"`
			Index    int             `json:"output_index"`
			Item     json.RawMessage `json:"item"`
			Response responsesWire   `json:"response"`
		}
		if err := providerJSON(raw, &e); err != nil {
			return false, err
		}
		switch e.Type {
		case "response.created", "response.in_progress":
			result.Usage = providerUsageLowerBound(result.Usage, e.Response.Usage.normalized("responses"))
		case "response.output_text.delta":
			if err := observed.add(e.Delta, false); err != nil {
				return false, err
			}
			result.Message.Content += e.Delta
			if len(result.Message.Content) > p.outputBytes() {
				return false, errProviderLimit
			}
			return false, emit(Event{Type: "text_delta", Text: e.Delta})
		case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			return false, observed.add(e.Delta, true)
		case "response.output_item.done":
			var block nativeBlock
			if json.Unmarshal(e.Item, &block) != nil {
				return false, errProviderData
			}
			if block.Type == "reasoning" {
				return false, observed.add(string(e.Item), true)
			}
		case "response.output_item.added":
			if e.Index < 0 || e.Index > 1024 {
				return false, errProviderData
			}
			var block nativeBlock
			if json.Unmarshal(e.Item, &block) != nil {
				return false, errProviderData
			}
			if block.Type == "function_call" {
				if err := observed.add(block.Arguments, false); err != nil {
					return false, err
				}
				arguments[e.Index] = block.Arguments
			}
			if block.Type == "reasoning" {
				return false, observed.add(string(e.Item), true)
			}
		case "response.function_call_arguments.delta":
			if err := observed.add(e.Delta, false); err != nil {
				return false, err
			}
			prior, ok := arguments[e.Index]
			if !ok {
				return false, errProviderData
			}
			if len(prior)+len(e.Delta) > p.outputBytes() {
				return false, errProviderLimit
			}
			arguments[e.Index] = prior + e.Delta
		case "response.completed":
			parsed, err := p.parseResponses(e.Response)
			if !parsed.usageFinal {
				parsed.Usage = providerUsageLowerBound(parsed.Usage, result.Usage)
			}
			if result.Message.Content != "" && result.Message.Content != parsed.Message.Content {
				return false, errProviderData
			}
			emitCompletedText := result.Message.Content == "" && parsed.Message.Content != ""
			result = parsed
			if emitCompletedText {
				if err := emit(Event{Type: "text_delta", Text: parsed.Message.Content}); err != nil {
					return false, err
				}
			}
			return true, err
		case "response.incomplete", "response.failed", "error":
			if e.Response.Status != "" || len(e.Response.Output) > 0 {
				parsed, _ := p.parseResponses(e.Response)
				if !parsed.usageFinal {
					parsed.Usage = providerUsageLowerBound(parsed.Usage, result.Usage)
				}
				result = parsed
			}
			return false, errProviderData
		}
		return false, nil
	})
	return result, err
}
