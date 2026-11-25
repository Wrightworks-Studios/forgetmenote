package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"forgetmenote/internal/durable"
)

type Message struct {
	Images       []string        `json:"images,omitempty"`
	Role         string          `json:"role"`
	Content      string          `json:"content"`
	ToolCalls    []ToolCall      `json:"toolCalls,omitempty"`
	ToolCallID   string          `json:"toolCallID,omitempty"`
	Continuation json.RawMessage `json:"continuation,omitempty"`
}
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}
type Request struct {
	Check           func(context.Context) error `json:"-"`
	Schema          json.RawMessage             `json:"-"`
	ReasoningEffort string
	Model           string
	Messages        []Message
	System          string
	Tools           []Tool
	MaxOutputTokens int
	FinalAnswer     bool
	JSONOutput      bool
}
type Usage struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CachedTokens     int64 `json:"cachedTokens"`
	ReasoningTokens  int64 `json:"reasoningTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
	Estimated        bool  `json:"estimated"`
}
type Event struct {
	Type     string
	Text     string
	ToolCall *ToolCall
	Usage    *Usage
}
type Response struct {
	Message Message
	Usage   Usage
	Finish  string
	// Native totals are authoritative only when their documented final event was received.
	usageFinal             bool
	observedOutputBytes    int
	observedReasoningBytes int
}

// JSON escaping, tool envelopes and opaque reasoning need wire headroom beyond
// plain-text token estimates. Capacity follows the configured model, not a fixed
// byte cutoff. Native token/context checks remain authoritative.
func encodedLimit(tokens int) int    { return tokens*24 + (1 << 20) }
func (p *Provider) inputBytes() int  { return encodedLimit(p.connection.Limits.ContextTokens) }
func (p *Provider) outputBytes() int { return encodedLimit(p.connection.Limits.OutputTokens) }

// These markers describe the boundary that failed, independently of display text.
// Contract failures include native input validation and incomplete/invalid output.
var ErrProviderRequest = errors.New("configured provider is unavailable")
var ErrProviderContract = errors.New("provider contract failure")

type providerFailure struct {
	kind    error
	message string
}

func (e *providerFailure) Error() string { return e.message }
func (e *providerFailure) Unwrap() error { return e.kind }
func providerContractError(message string) error {
	return &providerFailure{ErrProviderContract, message}
}

var errProviderData = providerContractError("provider returned invalid or incomplete output")
var errProviderLimit = providerContractError("provider response exceeded configured limits")

// Provider keeps credentials and transport state private and has no ambient proxy or fallback.
type Provider struct {
	connection Connection
	wire       providerWire
	client     *http.Client
	credential string
	slots      chan struct{}
}

func NewProvider(c Connection) (*Provider, error) {
	if err := validateConnection(&c, func() Limits { v, _ := normalizeLimits(c.Limits, defaultLimits()); return v }()); err != nil {
		return nil, err
	}
	if c.Cloudflare != nil && !c.Local {
		account, err := cloudflareAccount(c.Cloudflare.AccountIDFile)
		if err != nil {
			return nil, err
		}
		c.BaseURL = "https://api.cloudflare.com/client/v4/accounts/" + account + "/ai/v1"
		if c.Format == "cloudflare-moondream" || c.Format == "cloudflare-mistral" {
			c.BaseURL = strings.TrimSuffix(c.BaseURL, "/v1")
		}
	}
	token := ""
	if c.SecretFile != "" {
		var err error
		token, err = durable.ReadSecret(c.SecretFile, 16384)
		switch {
		case errors.Is(err, durable.ErrOpen):
			return nil, errors.New("cannot read configured provider credential")
		case errors.Is(err, durable.ErrNotRegular):
			return nil, errors.New("invalid configured provider credential file")
		case err != nil:
			return nil, errors.New("invalid configured provider credential")
		}
	}
	headerTimeout := 30 * time.Second
	if c.Cloudflare != nil {
		headerTimeout = time.Duration(c.Limits.RunSeconds) * time.Second
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: headerTimeout, IdleConnTimeout: 60 * time.Second, MaxIdleConns: 8, MaxConnsPerHost: c.Limits.ConnectionConcurrent, MaxIdleConnsPerHost: c.Limits.ConnectionConcurrent}
	wire, err := selectProviderWire(c.Format)
	if err != nil {
		return nil, err
	}
	return &Provider{connection: c, wire: wire, credential: token, slots: make(chan struct{}, c.Limits.ConnectionConcurrent), client: &http.Client{Transport: transport, Timeout: time.Duration(c.Limits.RunSeconds) * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (p *Provider) validToolCall(c ToolCall) bool {
	var obj map[string]json.RawMessage
	return c.ID != "" && len(c.ID) <= 256 && configID.MatchString(c.Name) && len(c.Arguments) <= p.inputBytes() && json.Unmarshal(c.Arguments, &obj) == nil && obj != nil
}
func strictToolSchema(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if x["type"] == "object" || x["properties"] != nil {
			if x["additionalProperties"] != false {
				return false
			}
			properties, _ := x["properties"].(map[string]any)
			required, _ := x["required"].([]any)
			names := map[string]bool{}
			for _, r := range required {
				n, ok := r.(string)
				if !ok {
					return false
				}
				names[n] = true
			}
			for n := range properties {
				if !names[n] {
					return false
				}
			}
		}
		for _, child := range x {
			if !strictToolSchema(child) {
				return false
			}
		}
	case []any:
		for _, child := range x {
			if !strictToolSchema(child) {
				return false
			}
		}
	}
	return true
}
func (p *Provider) validateRequest(req Request, estimate int) error {
	if req.JSONOutput && !p.connection.Capabilities.JSONOutput {
		return providerContractError("JSON output is not supported by this configured provider")
	}
	allowed := false
	for _, m := range p.connection.Models {
		if m == req.Model {
			allowed = true
		}
	}
	if !allowed {
		return providerContractError("provider model is outside configured allowlist")
	}
	if req.MaxOutputTokens < 1 || req.MaxOutputTokens > p.connection.Limits.OutputTokens {
		return providerContractError("requested provider output exceeds configured limit")
	}
	if p.connection.Capabilities.ThinkingBudget >= req.MaxOutputTokens {
		return errors.New("output budget cannot contain configured thinking budget")
	}
	if len(req.Messages) == 0 || len(req.Tools) > 0 && !p.connection.Capabilities.Tools {
		return providerContractError("unsupported or oversized provider request")
	}
	names := map[string]bool{}
	for _, tool := range req.Tools {
		var schema map[string]any
		if !configID.MatchString(tool.Name) || names[tool.Name] || len(tool.Schema) > p.inputBytes() || json.Unmarshal(tool.Schema, &schema) != nil || schema["type"] != "object" {
			return providerContractError("invalid provider tool definition")
		}
		if p.connection.Capabilities.StrictTools && !strictToolSchema(schema) {
			return errors.New("tool schema is incompatible with configured strict mode")
		}
		names[tool.Name] = true
	}
	pending := map[string]bool{}
	for _, m := range req.Messages {
		if err := validateImages(m, p.connection); err != nil {
			return err
		}
		if len(m.Continuation) > p.inputBytes() {
			return providerContractError("provider continuation exceeds limit")
		}
		switch m.Role {
		case "user":
			if len(pending) > 0 {
				return providerContractError("missing provider tool results")
			}
		case "assistant":
			if len(pending) > 0 {
				return providerContractError("missing provider tool results")
			}
		case "tool":
			if !pending[m.ToolCallID] {
				return providerContractError("provider tool result has no matching call")
			}
			delete(pending, m.ToolCallID)
		default:
			return providerContractError("unsupported provider message role")
		}
		if m.Role != "assistant" && (len(m.ToolCalls) > 0 || len(m.Continuation) > 0) {
			return providerContractError("invalid provider message continuation")
		}
		for _, call := range m.ToolCalls {
			if !p.validToolCall(call) || pending[call.ID] {
				return providerContractError("invalid provider tool call history")
			}
			pending[call.ID] = true
		}
	}
	if len(pending) > 0 {
		return providerContractError("missing provider tool results")
	}
	if estimate+req.MaxOutputTokens > p.connection.Limits.ContextTokens {
		return providerContractError("provider context exceeds configured token estimate")
	}
	return nil
}

type preparedRequest struct {
	request  Request
	path     string
	encoded  []byte
	estimate int
}

// prepareRequest owns normalization and native encoding for both fitting and sending.
// It deliberately does not enforce admission limits: the fitter needs oversized estimates.
func (p *Provider) prepareRequest(req Request) (preparedRequest, error) {
	// Compatible endpoints need only tool omission; optional tool-choice controls are not assumed.
	if req.FinalAnswer && p.connection.Format == "chat-completions" {
		req.Tools = nil
	}
	if req.Model == "" {
		req.Model = p.connection.DefaultModel
	}
	if req.MaxOutputTokens == 0 {
		req.MaxOutputTokens = p.connection.Limits.OutputTokens
	}
	if p.connection.Capabilities.StrictTools {
		tools, err := StrictToolDefinitions(req.Tools)
		if err != nil {
			return preparedRequest{}, err
		}
		req.Tools = tools
	}
	body, path, err := p.requestBody(req)
	if err != nil {
		return preparedRequest{}, err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return preparedRequest{}, providerContractError("cannot encode configured provider request")
	}
	size := len(req.System)
	for _, tool := range req.Tools {
		size += len(tool.Schema) + len(tool.Description) + len(tool.Name)
	}
	for _, m := range req.Messages {
		size += len(m.Content) + len(m.Continuation) + 24
		for _, call := range m.ToolCalls {
			size += len(call.Arguments) + len(call.Name) + len(call.ID)
		}
	}
	wireSize := len(encoded)
	for _, m := range req.Messages {
		for _, data := range m.Images {
			// Base64 is transport, not text tokens. Reserve a conservative per-image
			// allowance; native final usage remains authoritative.
			wireSize -= len(data)
			size += 8192 * 3
			wireSize += 8192 * 3
		}
	}
	return preparedRequest{request: req, path: path, encoded: encoded, estimate: max((size+2)/3, (wireSize+2)/3)}, nil
}

// EstimateRequest uses the configured native wire format without sending data.
// The raw validation bound remains a floor, including opaque native reasoning.
func (p *Provider) EstimateRequest(req Request) (int, error) {
	prepared, err := p.prepareRequest(req)
	return prepared.estimate, err
}

func (p *Provider) Generate(ctx context.Context, req Request, emit func(Event) error) (Response, error) {
	prepared, err := p.prepareRequest(req)
	if err != nil {
		return Response{}, err
	}
	req = prepared.request
	if err := p.validateRequest(req, prepared.estimate); err != nil {
		return Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.connection.Limits.RunSeconds)*time.Second)
	defer cancel()
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
	encoded := prepared.encoded
	textBytes := len(encoded)
	if p.connection.Format == "cloudflare-moondream" || p.connection.Format == "cloudflare-mistral" {
		// The native image was validated separately. Base64 transport bytes
		// are not textual context and must not impose a screenshot size cap.
		for _, message := range req.Messages {
			for _, image := range message.Images {
				textBytes -= len(image)
			}
		}
	}
	if textBytes > p.inputBytes() {
		return Response{}, providerContractError("provider request exceeds size limit")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.connection.BaseURL, "/")+prepared.path, bytes.NewReader(encoded))
	if err != nil {
		return Response{}, providerContractError("cannot construct provider request")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.connection.Capabilities.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	p.wire.authorize(httpReq.Header, p.credential)
	if p.connection.Cloudflare != nil {
		// Route native extraction through the same prepaid gateway as search.
		httpReq.Header.Set("cf-aig-gateway-id", p.connection.Cloudflare.GatewayID)
	}

	result := Response{Message: Message{Role: "assistant"}, Usage: Usage{InputTokens: int64(prepared.estimate), Estimated: true}}
	resp, err := p.providerHTTP(httpReq, req.Check)
	if err != nil {
		if ProviderOutcomeSettled(err) {
			result.Usage = Usage{}
		} else if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, err
	}
	defer resp.Body.Close()
	send := func(e Event) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if emit != nil {
			return emit(e)
		}
		return nil
	}
	if p.connection.Capabilities.Stream {
		if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			return result, providerContractError("provider did not return the configured stream format")
		}
		result, err = p.readStream(resp.Body, send)
	} else {
		var raw []byte
		raw, err = io.ReadAll(io.LimitReader(resp.Body, int64(p.outputBytes())+1))
		if err == nil && len(raw) > p.outputBytes() {
			err = errProviderLimit
		}
		if err == nil {
			result, err = p.parseResponse(raw)
		}
		if err != nil && len(raw) > result.observedOutputBytes {
			result.observedOutputBytes = len(raw)
		}
		if err == nil && result.Message.Content != "" {
			err = send(Event{Type: "text_delta", Text: result.Message.Content})
		}
	}
	if ctx.Err() != nil {
		return p.estimateUsage(result, prepared.estimate*3), ctx.Err()
	}
	if err != nil {
		return p.estimateUsage(result, prepared.estimate*3), err
	}
	if err = p.validateResponse(result, req); err != nil {
		return p.estimateUsage(result, prepared.estimate*3), err
	}
	for _, call := range result.Message.ToolCalls {
		call := call
		if err = send(Event{Type: "tool_call", ToolCall: &call}); err != nil {
			return p.estimateUsage(result, prepared.estimate*3), err
		}
	}
	result = p.estimateUsage(result, prepared.estimate*3)
	if err = send(Event{Type: "usage", Usage: &result.Usage}); err != nil {
		return result, err
	}
	return result, nil
}
func (p *Provider) estimateUsage(r Response, inputBytes int) Response {
	if !validProviderUsage(r.Usage) {
		r.Usage = Usage{}
		r.usageFinal = false
	}
	if r.usageFinal {
		return r
	}
	// These are uncertain observed-work estimates, never final billing totals.
	// Preserve native counters as lower bounds and let the runner retain its reservation.
	r.Usage.Estimated = true
	inputEstimate := int64((inputBytes + 2) / 3)
	if inputEstimate > r.Usage.InputTokens {
		r.Usage.InputTokens = inputEstimate
	}
	visibleBytes := len(r.Message.Content)
	for _, call := range r.Message.ToolCalls {
		visibleBytes += len(call.Arguments)
	}
	outputBytes := visibleBytes
	if len(r.Message.Continuation) > outputBytes {
		outputBytes = len(r.Message.Continuation)
	}
	if r.observedOutputBytes > outputBytes {
		outputBytes = r.observedOutputBytes
	}
	outputEstimate := int64((outputBytes + 2) / 3)
	if outputEstimate > r.Usage.OutputTokens {
		r.Usage.OutputTokens = outputEstimate
	}
	reasoningEstimate := int64((r.observedReasoningBytes + 2) / 3)
	if reasoningEstimate > r.Usage.ReasoningTokens {
		r.Usage.ReasoningTokens = reasoningEstimate
	}
	return r
}
func (p *Provider) validateResponse(r Response, req Request) error {
	if req.FinalAnswer && len(r.Message.ToolCalls) > 0 {
		return errProviderData
	}
	if len(r.Message.Content) > p.outputBytes() || len(r.Message.Continuation) > p.outputBytes() || len(r.Message.ToolCalls) > p.connection.Limits.MaxTools {
		return errProviderLimit
	}
	names := map[string]bool{}
	for _, t := range req.Tools {
		names[t.Name] = true
	}
	ids := map[string]bool{}
	for _, c := range r.Message.ToolCalls {
		if !p.validToolCall(c) || !names[c.Name] || ids[c.ID] {
			return errProviderData
		}
		ids[c.ID] = true
	}
	if !validProviderUsage(r.Usage) {
		return errProviderData
	}
	return nil
}
func (p *Provider) requestBody(req Request) (map[string]any, string, error) {
	caps := p.connection.Capabilities
	if req.ReasoningEffort != "" {
		if !slices.Contains([]string{"none", "low", "medium", "high", "xhigh", "max"}, req.ReasoningEffort) || p.connection.Format != "responses" && p.connection.Format != "chat-completions" {
			return nil, "", providerContractError("unsupported workload reasoning effort")
		}
		caps.ReasoningEffort = req.ReasoningEffort
	}
	body := map[string]any{"model": req.Model}
	if caps.Stream {
		body["stream"] = true
	}
	outputParam := caps.OutputTokenParameter
	if outputParam == "" {
		outputParam = "max_tokens"
		if p.connection.Format == "responses" {
			outputParam = "max_output_tokens"
		}
	}
	body[outputParam] = req.MaxOutputTokens
	for _, message := range req.Messages {
		if err := validateContinuation(message.Continuation, p.connection.Format); err != nil {
			return nil, "", err
		}
	}
	return p.wire.encode(req, caps, body)
}

func validProviderUsage(u Usage) bool {
	for _, value := range []int64{u.InputTokens, u.OutputTokens, u.CachedTokens, u.ReasoningTokens, u.CacheWriteTokens} {
		if value < 0 || value > 1<<40 {
			return false
		}
	}
	return true
}
