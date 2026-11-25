package chat

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

const mistralModel = "@cf/mistralai/mistral-small-3.1-24b-instruct"

// The native inference endpoint accepts original inline images and guided_json.
// Its envelope contains standard completion choices and authoritative usage.
type mistralFormat struct{}

func (mistralFormat) encode(req Request, caps Capabilities, body map[string]any) (map[string]any, string, error) {
	if req.Model != mistralModel || len(req.Tools) != 0 || req.ReasoningEffort != "" || req.JSONOutput {
		return nil, "", providerContractError("unsupported native Mistral extraction")
	}
	images := 0
	for _, message := range req.Messages {
		if message.Role != "user" || len(message.ToolCalls) != 0 || len(message.Continuation) != 0 {
			return nil, "", providerContractError("Mistral extraction requires original user evidence without chat history")
		}
		images += len(message.Images)
	}
	if images != 1 {
		return nil, "", providerContractError("Mistral extraction requires exactly one image")
	}
	encoded, _, err := (completionFormat{}).encode(req, caps, body)
	if err != nil {
		return nil, "", err
	}
	delete(encoded, "model")
	encoded["stream"], encoded["temperature"] = false, 0
	if len(req.Schema) > 0 {
		var schema map[string]any
		if json.Unmarshal(req.Schema, &schema) != nil || schema["type"] != "object" {
			return nil, "", providerContractError("Mistral extraction requires an object output contract")
		}
		encoded["guided_json"] = schema
	}
	return encoded, "/run/" + mistralModel, nil
}

func (mistralFormat) authorize(header http.Header, credential string) {
	(moondreamFormat{}).authorize(header, credential)
}

func (mistralFormat) decode(p *Provider, raw []byte) (Response, error) {
	var completion json.RawMessage
	if err := cloudflareResult(raw, &completion); err != nil {
		return Response{}, err
	}
	out, err := (completionFormat{}).decode(p, completion)
	if err != nil {
		return out, err
	}
	if !out.usageFinal || out.Finish != "stop" || len(out.Message.ToolCalls) != 0 || strings.TrimSpace(out.Message.Content) == "" {
		return out, errProviderData
	}
	return out, nil
}

func (mistralFormat) stream(_ *Provider, _ io.Reader, _ func(Event) error) (Response, error) {
	return Response{}, providerContractError("Mistral extraction uses non-streaming native inference")
}
