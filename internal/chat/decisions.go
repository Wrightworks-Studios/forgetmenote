package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
)

type DecisionChoice struct {
	Value       string `json:"value"`
	Description any    `json:"description"`
}
type DecisionQuestion struct {
	Type         string           `json:"type"`
	Name         string           `json:"name"`
	Instructions string           `json:"instructions"`
	Choices      []DecisionChoice `json:"choices,omitempty"`
	Criteria     []any            `json:"criteria,omitempty"`
}
type DecisionsRequest struct {
	Check     func(context.Context) error `json:"-"`
	Model     string                      `json:"-"`
	Prompt    string                      `json:"prompt"`
	Images    []string                    `json:"images,omitempty"`
	Questions []DecisionQuestion          `json:"questions"`
}
type DecisionProbability struct {
	Value       string  `json:"value"`
	Probability float64 `json:"probability"`
}
type DecisionAnswer struct {
	Type          string                `json:"type"`
	Name          string                `json:"name"`
	Choice        string                `json:"choice,omitempty"`
	Confidence    *float64              `json:"confidence,omitempty"`
	Probabilities []DecisionProbability `json:"probabilities,omitempty"`
	Noul          *float64              `json:"noul,omitempty"`
	Score         *float64              `json:"score,omitempty"`
	Legend        map[string]any        `json:"legend,omitempty"`
}
type DecisionsResponse struct {
	Model   string           `json:"model"`
	Answers []DecisionAnswer `json:"answers"`
	Usage   Usage            `json:"usage"`
	Batches int              `json:"batches,omitempty"`
}

func (p *Provider) prepareDecisions(r DecisionsRequest) ([]byte, int, error) {
	if p.connection.Cloudflare != nil {
		return p.prepareCloudflareDecisions(r)
	}
	if r.Model == "" {
		r.Model = p.connection.DefaultModel
	}
	if p.connection.Format != "responses" || r.Model != "gpt-6-luna" || !slices.Contains(p.connection.Models, r.Model) {
		return nil, 0, providerContractError("Decisions requires configured gpt-6-luna Responses connection")
	}
	if strings.TrimSpace(r.Prompt) == "" || len(r.Questions) < 1 || len(r.Questions) > 64 {
		return nil, 0, providerContractError("Decisions requires evidence and 1–64 choice questions")
	}
	if err := validateImagesLimit(Message{Role: "user", Images: r.Images}, p.connection, 128); err != nil {
		return nil, 0, err
	}
	names := map[string]bool{}
	for _, q := range r.Questions {
		if q.Type != "choice" || !configID.MatchString(q.Name) || names[q.Name] || strings.TrimSpace(q.Instructions) == "" || len(q.Choices) < 2 || len(q.Choices) > 32 {
			return nil, 0, providerContractError("invalid or duplicate decision question")
		}
		names[q.Name] = true
		values := map[string]bool{}
		for _, v := range q.Choices {
			description, valid := v.Description.(string)
			if v.Value == "" || len(v.Value) > 128 || values[v.Value] || !valid || strings.TrimSpace(description) == "" {
				return nil, 0, providerContractError("invalid or duplicate decision choice")
			}
			values[v.Value] = true
		}
	}
	content := []any{map[string]string{"type": "input_text", "text": r.Prompt}}
	for _, img := range r.Images {
		content = append(content, map[string]string{"type": "input_image", "image_url": img})
	}
	body, err := json.Marshal(map[string]any{"model": r.Model, "input": []any{map[string]any{"role": "user", "content": content}}, "questions": r.Questions})
	if err != nil || len(body) > openAIImagePayloadLimit {
		return nil, 0, errProviderLimit
	}
	size := len(body)
	for _, img := range r.Images {
		size -= len(img)
		size += 8192 * 3
	}
	estimate := (size + 2) / 3
	if estimate > p.connection.Limits.ContextTokens {
		return nil, 0, providerContractError("Decisions context capacity reached")
	}
	return body, estimate, nil
}
func (p *Provider) EstimateDecisions(r DecisionsRequest) (int, error) {
	if p.connection.Cloudflare != nil {
		_, n, err := p.cloudflareDecisionBatches(r)
		return n, err
	}
	_, n, e := p.prepareDecisions(r)
	return n, e
}

// nativeJSON shares the configured credential, transport, timeout and concurrency
// boundary with generation. It never retries a paid request or exposes its body.
func (p *Provider) nativeJSON(ctx context.Context, path string, body []byte, checks ...func(context.Context) error) ([]byte, error) {
	return p.nativeJSONObserved(ctx, path, body, nil, checks...)
}

func (p *Provider) nativeJSONObserved(ctx context.Context, path string, body []byte, observe func(*http.Response), checks ...func(context.Context) error) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.connection.Limits.RunSeconds)*time.Second)
	defer cancel()
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	base := strings.TrimRight(p.connection.BaseURL, "/")
	if p.connection.Cloudflare != nil && strings.HasPrefix(path, "/run/") {
		base = strings.TrimSuffix(base, "/v1")
	}
	if p.connection.Cloudflare != nil && path == "/websearch/" {
		base = strings.TrimSuffix(base, "/v1")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+path, bytes.NewReader(body))
	if err != nil {
		return nil, providerContractError("invalid provider request")
	}
	req.Header.Set("Content-Type", "application/json")
	if p.connection.Cloudflare != nil {
		req.Header.Set("cf-aig-collect-log", "false")
		req.Header.Set("cf-aig-skip-cache", "true")
		if strings.HasPrefix(path, "/run/") {
			req.Header.Set("cf-aig-gateway-id", p.connection.Cloudflare.GatewayID)
		}
	}
	if p.credential != "" {
		req.Header.Set("Authorization", "Bearer "+p.credential)
	}
	var check func(context.Context) error
	if len(checks) > 0 {
		check = checks[0]
	}
	response, err := p.providerHTTP(req, check)
	if err != nil {
		if ctx.Err() != nil && !ProviderOutcomeSettled(err) {
			return nil, ctx.Err()
		}
		return nil, err
	}
	defer response.Body.Close()
	if observe != nil {
		observe(response)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, int64(p.outputBytes())+1))
	if err != nil {
		return nil, &providerFailure{ErrProviderRequest, "provider response was interrupted"}
	}
	if len(raw) > p.outputBytes() {
		return nil, errProviderLimit
	}
	return raw, nil
}
func (p *Provider) Decide(ctx context.Context, r DecisionsRequest) (DecisionsResponse, error) {
	if p.connection.Cloudflare != nil {
		return p.cloudflareDecide(ctx, r)
	}
	body, n, err := p.prepareDecisions(r)
	if err != nil {
		return DecisionsResponse{}, err
	}
	out := DecisionsResponse{Usage: Usage{InputTokens: int64(n), Estimated: true}}
	raw, err := p.nativeJSON(ctx, "/decisions", body)
	if err != nil {
		return out, err
	}
	var wire struct {
		Model   string           `json:"model"`
		Answers []DecisionAnswer `json:"answers"`
		Usage   nativeUsage      `json:"usage"`
	}
	if providerJSON(raw, &wire) != nil {
		return out, errProviderData
	}
	if !wire.Usage.hasTotals("responses") || !validProviderUsage(wire.Usage.normalized("responses")) {
		return out, errProviderData
	}
	out.Usage = wire.Usage.normalized("responses")
	model := r.Model
	if model == "" {
		model = p.connection.DefaultModel
	}
	if wire.Model != model || len(wire.Answers) != len(r.Questions) {
		return out, errProviderData
	}
	for i, a := range wire.Answers {
		q := r.Questions[i]
		if a.Name != q.Name {
			return out, errProviderData
		}
		if a.Type == "refusal" {
			if a.Choice != "" || a.Confidence != nil || len(a.Probabilities) != 0 {
				return out, errProviderData
			}
			continue
		}
		if a.Type != "choice" || a.Confidence == nil || !unitProbability(*a.Confidence) || len(a.Probabilities) != len(q.Choices) {
			return out, errProviderData
		}
		allowed := map[string]bool{}
		for _, v := range q.Choices {
			allowed[v.Value] = true
		}
		if !allowed[a.Choice] {
			return out, errProviderData
		}
		seen := map[string]bool{}
		total := 0.0
		for _, v := range a.Probabilities {
			if !allowed[v.Value] || seen[v.Value] || !unitProbability(v.Probability) {
				return out, errProviderData
			}
			seen[v.Value] = true
			total += v.Probability
		}
		if math.Abs(total-1) > 0.00001 {
			return out, errProviderData
		}
	}
	out.Model = wire.Model
	out.Answers = wire.Answers
	return out, nil
}
func unitProbability(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1 }
