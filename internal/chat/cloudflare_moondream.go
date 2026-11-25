package chat

import (
	"io"
	"net/http"
	"strings"
)

const moondreamModel = "@cf/moondream/moondream3.1-9B-A2B"

// Moondream's native query is a single-image extraction boundary, not a chat
// completion. Provider still owns admission, transport and credential handling.
type moondreamFormat struct{}

func (moondreamFormat) encode(req Request, _ Capabilities, _ map[string]any) (map[string]any, string, error) {
	if req.Model != moondreamModel || req.MaxOutputTokens > 28672 || len(req.Tools) != 0 || req.JSONOutput || req.ReasoningEffort != "" {
		return nil, "", providerContractError("unsupported native Moondream query")
	}
	parts := []string{}
	if req.System != "" {
		parts = append(parts, req.System)
	}
	images := []string{}
	for _, message := range req.Messages {
		if message.Role != "user" || len(message.ToolCalls) != 0 || len(message.Continuation) != 0 {
			return nil, "", providerContractError("Moondream extraction requires user evidence without chat history")
		}
		parts = append(parts, message.Content)
		images = append(images, message.Images...)
	}
	if len(images) != 1 {
		return nil, "", providerContractError("Moondream extraction requires exactly one image")
	}
	// Omitting stream returned a successful empty result in live Cloudflare
	// probes despite its documented false default. Both flags are explicit.
	return map[string]any{"task": "query", "image": images[0], "question": strings.Join(parts, "\n\n"), "stream": false, "reasoning": false, "max_tokens": req.MaxOutputTokens}, "/run/" + moondreamModel, nil
}

func (moondreamFormat) authorize(header http.Header, credential string) {
	completionFormat{}.authorize(header, credential)
	header.Set("cf-aig-collect-log", "false")
	header.Set("cf-aig-skip-cache", "true")
}

func (moondreamFormat) decode(_ *Provider, raw []byte) (Response, error) {
	var result struct {
		Result struct {
			Answer  *string `json:"answer"`
			Finish  string  `json:"finish_reason"`
			Metrics struct {
				Input  *int64 `json:"input_tokens"`
				Output *int64 `json:"output_tokens"`
			} `json:"metrics"`
		} `json:"result"`
		Usage nativeUsage `json:"usage"`
	}
	if err := cloudflareResult(raw, &result); err != nil {
		return Response{}, err
	}
	u := result.Usage.normalized("chat-completions")
	if !result.Usage.hasTotals("chat-completions") || !validProviderUsage(u) || result.Result.Metrics.Input == nil || result.Result.Metrics.Output == nil || *result.Result.Metrics.Input != u.InputTokens || *result.Result.Metrics.Output != u.OutputTokens {
		return Response{}, errProviderData
	}
	out := Response{Usage: u, usageFinal: true}
	if result.Result.Answer == nil || strings.TrimSpace(*result.Result.Answer) == "" || result.Result.Finish != "stop" {
		return out, errProviderData
	}
	out.Message = Message{Role: "assistant", Content: *result.Result.Answer}
	out.Finish = "stop"
	return out, nil
}

func (moondreamFormat) stream(_ *Provider, _ io.Reader, _ func(Event) error) (Response, error) {
	return Response{}, providerContractError("Moondream extraction uses non-streaming native queries")
}
