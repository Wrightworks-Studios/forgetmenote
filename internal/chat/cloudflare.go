package chat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/durable"
	"forgetmenote/internal/sources"
	"image"
	"math"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// CloudflareConfig binds moderation inference and transient discovery to one
// operator-owned account. It supplies no fallback provider or package credentials.
type CloudflareConfig struct {
	AccountIDFile  string `json:"accountIDFile"`
	DecisionModel  string `json:"decisionModel"`
	GatewayID      string `json:"gatewayID"`
	SearchProvider string `json:"searchProvider"`
}

func (c CloudflareConfig) decisionModel() (string, int) {
	switch c.DecisionModel {
	case "@cf/cloudflare/clef":
		return "clef", 65536
	case "@cf/cloudflare/clef-omni":
		return "clef-omni", 64000
	default:
		return "", 0
	}
}

func (c CloudflareConfig) validate(connection Connection) error {
	model, _ := c.decisionModel()
	if !filepath.IsAbs(c.AccountIDFile) || connection.SecretFile == "" || connection.Format != "cloudflare-moondream" && connection.Format != "cloudflare-mistral" || model == "" || !configID.MatchString(c.GatewayID) || c.SearchProvider != "ceramic" || !connection.Local && connection.BaseURL != "" {
		return errors.New("Cloudflare moderation requires private account/token files, Clef or Clef-omni, a gateway and Ceramic")
	}
	return nil
}

func cloudflareAccount(path string) (string, error) {
	value, err := durable.ReadSecret(path, 128)
	if err != nil || !regexp.MustCompile(`^[a-fA-F0-9]{32}$`).MatchString(value) {
		return "", errors.New("invalid private Cloudflare account ID")
	}
	return value, nil
}

func cloudflareResult(raw []byte, result any) error {
	var envelope struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
	}
	if json.Unmarshal(raw, &envelope) != nil || !envelope.Success || len(envelope.Result) == 0 || json.Unmarshal(envelope.Result, result) != nil {
		return errProviderData
	}
	return nil
}

// CloudflareSearch consumes search output transiently. Only candidate links
// leave this boundary; snippets, titles, ranking scores and raw output do not.
type SearchDiagnostics struct {
	Status       int    `json:"status"`
	RequestID    string `json:"requestId,omitempty"`
	RawItems     int    `json:"rawItems"`
	AcceptedURLs int    `json:"acceptedURLs"`
}

func (p *Provider) CloudflareSearch(ctx context.Context, query string, limit int) ([]ResearchResult, error) {
	result, _, err := p.CloudflareSearchDiagnostics(ctx, query, limit)
	return result, err
}

func (p *Provider) CloudflareSearchDiagnostics(ctx context.Context, query string, limit int) ([]ResearchResult, SearchDiagnostics, error) {
	diagnostics := SearchDiagnostics{}
	if p.connection.Cloudflare == nil || strings.TrimSpace(query) == "" || utf8.RuneCountInString(query) > 1024 || strings.ContainsRune(query, 0) || limit < 1 || limit > 10 {
		return nil, diagnostics, providerContractError("invalid Cloudflare discovery request")
	}
	c := p.connection.Cloudflare
	body, _ := json.Marshal(map[string]any{"query": query, "provider": c.SearchProvider, "limit": limit, "options": map[string]any{"gateway": map[string]any{"id": c.GatewayID, "collectLog": false, "skipCache": true}}})
	raw, err := p.nativeJSONObserved(ctx, "/websearch/", body, func(response *http.Response) {
		diagnostics.Status = response.StatusCode
		ray := response.Header.Get("CF-Ray")
		if regexp.MustCompile(`^[a-fA-F0-9]{16,32}-[A-Z0-9]{3,8}$`).MatchString(ray) {
			diagnostics.RequestID = ray
		}
	})
	if err != nil {
		return nil, diagnostics, err
	}
	var result struct {
		Items *[]struct {
			URL string `json:"url"`
		} `json:"items"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Items == nil || len(*result.Items) > limit {
		return nil, diagnostics, errProviderData
	}
	diagnostics.RawItems = len(*result.Items)
	out := []ResearchResult{}
	seen := map[string]bool{}
	for _, item := range *result.Items {
		if sources.ValidateURL(item.URL) != nil {
			return nil, diagnostics, errProviderData
		}
		if !seen[item.URL] {
			seen[item.URL] = true
			out = append(out, ResearchResult{URL: item.URL})
		}
	}
	diagnostics.AcceptedURLs = len(out)
	return out, diagnostics, nil
}

func validClefDescription(value any) bool {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case map[string]any:
		if len(v) == 0 {
			return false
		}
	case []any:
		if len(v) == 0 {
			return false
		}
	default:
		return false
	}
	_, err := json.Marshal(value)
	return err == nil
}

func (p *Provider) prepareCloudflareDecisions(r DecisionsRequest) ([]byte, int, error) {
	if strings.TrimSpace(r.Prompt) == "" || len(r.Questions) < 1 || len(r.Questions) > 64 {
		return nil, 0, providerContractError("Clef requires evidence and 1–64 questions")
	}
	if err := validateImagesLimit(Message{Role: "user", Images: r.Images}, p.connection, 4); err != nil {
		return nil, 0, err
	}
	total := 0
	for _, inline := range r.Images {
		_, encoded, _ := strings.Cut(inline, ",")
		data, _ := base64.StdEncoding.Strict().DecodeString(encoded)
		cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
		total += len(data)
		if err != nil || format != "png" && format != "jpeg" && format != "webp" || len(data) > 4<<20 || int64(cfg.Width)*int64(cfg.Height) > 16000000 || total > 8<<20 {
			return nil, 0, providerContractError("image exceeds Clef provider capacity")
		}
	}
	questions := map[string]any{}
	for _, q := range r.Questions {
		if !configID.MatchString(q.Name) || questions[q.Name] != nil || strings.TrimSpace(q.Instructions) == "" {
			return nil, 0, providerContractError("invalid Clef question")
		}
		question := map[string]any{"type": q.Type, "instructions": q.Instructions}
		switch q.Type {
		case "choice":
			if len(q.Choices) < 2 || len(q.Choices) > 32 || len(q.Criteria) != 0 {
				return nil, 0, providerContractError("invalid Clef choices")
			}
			criteria := map[string]any{}
			for _, choice := range q.Choices {
				if choice.Value == "" || len(choice.Value) > 128 || criteria[choice.Value] != nil || !validClefDescription(choice.Description) {
					return nil, 0, providerContractError("invalid Clef choice")
				}
				criteria[choice.Value] = choice.Description
			}
			question["criteria"] = criteria
		case "noul":
			if len(q.Choices) != 0 || len(q.Criteria) != 0 {
				return nil, 0, providerContractError("noul has no criteria")
			}
		case "score":
			if len(q.Choices) != 0 || len(q.Criteria) < 2 || len(q.Criteria) > 32 {
				return nil, 0, providerContractError("invalid Clef score criteria")
			}
			for _, value := range q.Criteria {
				if !validClefDescription(value) {
					return nil, 0, providerContractError("invalid Clef score level")
				}
			}
			question["criteria"] = q.Criteria
		default:
			return nil, 0, providerContractError("unsupported Clef question type")
		}
		questions[q.Name] = question
	}
	model, contextLimit := p.connection.Cloudflare.decisionModel()
	body, err := json.Marshal(map[string]any{"model": model, "state": r.Prompt, "questions": questions, "images": r.Images})
	text, _ := json.Marshal(questions)
	// A byte per token is a conservative upper bound; never let the provider
	// silently truncate the authoritative state to make room for questions.
	upper := len([]byte(r.Prompt)) + len(text) + len(r.Images)*1027 + 512
	if err != nil || len(body) > 13<<20 || upper > contextLimit {
		return nil, 0, providerContractError("Clef request exceeds provider capacity; state was not truncated")
	}
	return body, upper, nil
}

type cloudflareDecisionBatch struct {
	request  DecisionsRequest
	body     []byte
	estimate int
}

// Preflight all batches before buying inference. Questions may be separated;
// authoritative state and original images are never truncated or substituted.
func (p *Provider) cloudflareDecisionBatches(r DecisionsRequest) ([]cloudflareDecisionBatch, int, error) {
	if len(r.Questions) == 0 || len(r.Questions) > 64 {
		return nil, 0, providerContractError("Clef requires 1–64 decision questions")
	}
	batches := []cloudflareDecisionBatch{}
	names := map[string]bool{}
	current := cloudflareDecisionBatch{request: DecisionsRequest{Prompt: r.Prompt, Images: r.Images, Check: r.Check}}
	total := 0
	for _, q := range r.Questions {
		if names[q.Name] {
			return nil, 0, providerContractError("duplicate Clef question")
		}
		names[q.Name] = true
		candidate := current.request
		candidate.Questions = append(append([]DecisionQuestion{}, current.request.Questions...), q)
		body, estimate, err := p.prepareCloudflareDecisions(candidate)
		if err != nil {
			if len(current.request.Questions) == 0 {
				return nil, 0, err
			}
			// Validate the question alone before accepting the preceding batch.
			candidate.Questions = []DecisionQuestion{q}
			body, estimate, err = p.prepareCloudflareDecisions(candidate)
			if err != nil {
				return nil, 0, err
			}
			batches = append(batches, current)
			total += current.estimate
		}
		current = cloudflareDecisionBatch{request: candidate, body: body, estimate: estimate}
	}
	batches = append(batches, current)
	total += current.estimate
	return batches, total, nil
}

func (p *Provider) cloudflareDecide(ctx context.Context, r DecisionsRequest) (DecisionsResponse, error) {
	batches, _, err := p.cloudflareDecisionBatches(r)
	out := DecisionsResponse{Model: p.connection.Cloudflare.DecisionModel, Batches: len(batches)}
	if err != nil {
		return out, err
	}
	for _, batch := range batches {
		if r.Check != nil {
			if err := r.Check(ctx); err != nil {
				out.Answers = nil
				return out, err
			}
		}
		part, err := p.cloudflareDecideBatch(ctx, batch)
		out.Usage.InputTokens += part.Usage.InputTokens
		out.Usage.OutputTokens += part.Usage.OutputTokens
		out.Usage.CachedTokens += part.Usage.CachedTokens
		out.Usage.Estimated = out.Usage.Estimated || part.Usage.Estimated
		if err != nil {
			out.Answers = nil
			return out, err
		}
		if r.Check != nil {
			if err := r.Check(ctx); err != nil {
				out.Answers = nil
				return out, err
			}
		}
		out.Answers = append(out.Answers, part.Answers...)
	}
	return out, nil
}

func (p *Provider) cloudflareDecideBatch(ctx context.Context, batch cloudflareDecisionBatch) (DecisionsResponse, error) {
	r := batch.request
	out := DecisionsResponse{Model: p.connection.Cloudflare.DecisionModel, Usage: Usage{InputTokens: int64(batch.estimate), Estimated: true}}
	raw, err := p.nativeJSON(ctx, "/run/"+p.connection.Cloudflare.DecisionModel, batch.body, r.Check)
	if err != nil {
		if ProviderOutcomeSettled(err) {
			out.Usage = Usage{}
		}
		return out, err
	}
	var native struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Type          string             `json:"type"`
			Choice        string             `json:"choice"`
			Confidence    *float64           `json:"confidence"`
			Probabilities map[string]float64 `json:"probabilities"`
			Noul          *float64           `json:"noul"`
			Score         *float64           `json:"score"`
			Legend        map[string]any     `json:"legend"`
		} `json:"answers"`
		Usage nativeUsage `json:"usage"`
	}
	model, _ := p.connection.Cloudflare.decisionModel()
	if cloudflareResult(raw, &native) != nil || native.Model != model || len(native.Answers) != len(r.Questions) || !native.Usage.hasTotals("responses") || !validProviderUsage(native.Usage.normalized("responses")) {
		return out, errProviderData
	}
	out.Usage = native.Usage.normalized("responses")
	for _, q := range r.Questions {
		a, ok := native.Answers[q.Name]
		if !ok {
			return out, errProviderData
		}
		if a.Type == "refusal" {
			if a.Choice != "" || a.Confidence != nil || len(a.Probabilities) != 0 || a.Noul != nil || a.Score != nil || len(a.Legend) != 0 {
				return out, errProviderData
			}
			out.Answers = append(out.Answers, DecisionAnswer{Type: "refusal", Name: q.Name})
			continue
		}
		if a.Type != q.Type {
			return out, errProviderData
		}
		answer := DecisionAnswer{Type: a.Type, Name: q.Name, Choice: a.Choice, Confidence: a.Confidence, Noul: a.Noul, Score: a.Score, Legend: a.Legend}
		if q.Type == "noul" {
			if a.Noul == nil || !unitProbability(*a.Noul) || a.Choice != "" || len(a.Probabilities) != 0 || a.Score != nil || a.Confidence != nil || len(a.Legend) != 0 {
				return out, errProviderData
			}
			out.Answers = append(out.Answers, answer)
			continue
		}
		if a.Confidence == nil || !unitProbability(*a.Confidence) || a.Noul != nil {
			return out, errProviderData
		}
		values := []string{}
		if q.Type == "choice" {
			if a.Score != nil || len(a.Legend) != 0 {
				return out, errProviderData
			}
			for _, c := range q.Choices {
				values = append(values, c.Value)
			}
		} else {
			if a.Score == nil || math.IsNaN(*a.Score) || math.IsInf(*a.Score, 0) || *a.Score < 0 || *a.Score > float64(len(q.Criteria)-1) || a.Choice != "" || len(a.Legend) != len(q.Criteria) {
				return out, errProviderData
			}
			for i, level := range q.Criteria {
				key := fmt.Sprint(i)
				values = append(values, key)
				expected, _ := json.Marshal(level)
				actual, _ := json.Marshal(a.Legend[key])
				if !bytes.Equal(expected, actual) {
					return out, errProviderData
				}
			}
		}
		if len(a.Probabilities) != len(values) {
			return out, errProviderData
		}
		found, sum := q.Type == "score", 0.0
		weighted := 0.0
		for i, value := range values {
			v, ok := a.Probabilities[value]
			if !ok || !unitProbability(v) {
				return out, errProviderData
			}
			found = found || value == a.Choice
			sum += v
			weighted += float64(i) * v
			answer.Probabilities = append(answer.Probabilities, DecisionProbability{Value: value, Probability: v})
		}
		if !found || math.Abs(sum-1) > .001 {
			return out, errProviderData
		}
		if q.Type == "score" && math.Abs(*a.Score-weighted) > .001 {
			return out, errProviderData
		}
		out.Answers = append(out.Answers, answer)
	}
	return out, nil
}
