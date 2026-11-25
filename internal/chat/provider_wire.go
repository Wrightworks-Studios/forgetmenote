package chat

import (
	"encoding/json"
)

func providerJSON(raw []byte, v any) error {
	if len(raw) == 0 || json.Unmarshal(raw, v) != nil {
		return errProviderData
	}
	return nil
}

type nativeUsage struct {
	inputPresent, outputPresent, promptPresent, completionPresent bool
	InputTokens                                                   int64 `json:"input_tokens"`
	OutputTokens                                                  int64 `json:"output_tokens"`
	PromptTokens                                                  int64 `json:"prompt_tokens"`
	CompletionTokens                                              int64 `json:"completion_tokens"`
	CacheRead                                                     int64 `json:"cache_read_input_tokens"`
	CacheWrite                                                    int64 `json:"cache_creation_input_tokens"`
	PromptCacheHit                                                int64 `json:"prompt_cache_hit_tokens"`
	InputDetails                                                  struct {
		Cached     int64 `json:"cached_tokens"`
		CacheWrite int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails struct {
		Reasoning int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	PromptDetails struct {
		Cached int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails struct {
		Reasoning int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// Presence is distinct from a zero count and from provisional usage at stream start.
func (u *nativeUsage) UnmarshalJSON(raw []byte) error {
	type fields nativeUsage
	var v fields
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal(raw, &present); err != nil {
		return err
	}
	*u = nativeUsage(v)
	has := func(key string) bool { value, ok := present[key]; return ok && string(value) != "null" }
	u.inputPresent = has("input_tokens")
	u.outputPresent = has("output_tokens")
	u.promptPresent = has("prompt_tokens")
	u.completionPresent = has("completion_tokens")
	return nil
}
func (u nativeUsage) hasTotals(format string) bool {
	if format == "chat-completions" {
		return u.promptPresent && u.completionPresent
	}
	return u.inputPresent && u.outputPresent
}
func (u nativeUsage) normalized(format string) Usage {
	v := Usage{CacheWriteTokens: u.InputDetails.CacheWrite, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CachedTokens: u.InputDetails.Cached, ReasoningTokens: u.OutputDetails.Reasoning}
	if format == "messages" {
		// Messages input_tokens excludes both cache reads and cache creation.
		v.InputTokens += u.CacheRead + u.CacheWrite
		v.CachedTokens = u.CacheRead
		v.CacheWriteTokens = u.CacheWrite
	}
	if format == "chat-completions" {
		v.InputTokens = u.PromptTokens
		v.OutputTokens = u.CompletionTokens
		v.CachedTokens = u.PromptDetails.Cached
		if v.CachedTokens == 0 {
			v.CachedTokens = u.PromptCacheHit
		}
		v.ReasoningTokens = u.CompletionDetails.Reasoning
	}
	return v
}

// Provisional native counters remain lower bounds even when an event reports
// only one component. Only an explicitly final native total replaces them.
func providerUsageLowerBound(prior, reported Usage) Usage {
	for _, pair := range [][2]*int64{{&prior.InputTokens, &reported.InputTokens}, {&prior.OutputTokens, &reported.OutputTokens}, {&prior.CachedTokens, &reported.CachedTokens}, {&prior.CacheWriteTokens, &reported.CacheWriteTokens}, {&prior.ReasoningTokens, &reported.ReasoningTokens}} {
		if *pair[1] > *pair[0] {
			*pair[0] = *pair[1]
		}
	}
	return prior
}

type nativeBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Text      string          `json:"text"`
	Input     json.RawMessage `json:"input"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (p *Provider) parseResponse(raw []byte) (Response, error) { return p.wire.decode(p, raw) }
