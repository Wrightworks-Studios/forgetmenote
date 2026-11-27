package chat

import (
	"errors"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/jsonfile"
	"math"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Limits describe model capacity and execution safeguards; zero values select defaults.
type Limits struct {
	ContextTokens        int `json:"contextTokens"`
	OutputTokens         int `json:"outputTokens"`
	RunTokens            int `json:"runTokens,omitempty"` // Deprecated: accepted for compatibility and ignored.
	RunSeconds           int `json:"runSeconds"`
	MaxRounds            int `json:"maxRounds"`
	MaxTools             int `json:"maxTools"`
	MaxConcurrent        int `json:"maxConcurrent"`
	ConnectionConcurrent int `json:"connectionConcurrent"`
	DailyTokens          int `json:"dailyTokens,omitempty"` // Deprecated: accepted for compatibility and ignored.
	EvidenceTokens       int `json:"evidenceTokens"`
	MaxPassages          int `json:"maxPassages"`
	PerNotePassages      int `json:"perNotePassages"`
	MaxConversations     int `json:"maxConversations"`
	MaxEvents            int `json:"maxEvents"`
	MaxRecordBytes       int `json:"maxRecordBytes"`
}
type Capabilities struct {
	Images               bool   `json:"images,omitempty"`
	Tools                bool   `json:"tools"`
	Stream               bool   `json:"stream"`
	StrictTools          bool   `json:"strictTools"`
	StreamUsage          bool   `json:"streamUsage"`
	JSONOutput           bool   `json:"jsonOutput,omitempty"`
	StoreFalse           bool   `json:"storeFalse"`
	CacheControl         bool   `json:"cacheControl"`
	EncryptedReasoning   bool   `json:"encryptedReasoning"`
	ThinkingBudget       int    `json:"thinkingBudget,omitempty"`
	ReasoningContent     bool   `json:"reasoningContent"`
	Thinking             string `json:"thinking,omitempty"`
	ReasoningEffort      string `json:"reasoningEffort,omitempty"`
	OutputTokenParameter string `json:"outputTokenParameter,omitempty"`
}
type Connection struct {
	Cloudflare   *CloudflareConfig `json:"cloudflare,omitempty"`
	ID           string            `json:"id"`
	Label        string            `json:"label"`
	Format       string            `json:"format"`
	BaseURL      string            `json:"baseURL"`
	Models       []string          `json:"models"`
	DefaultModel string            `json:"defaultModel"`
	SecretFile   string            `json:"secretFile,omitempty"`
	Local        bool              `json:"local"`
	Capabilities Capabilities      `json:"capabilities"`
	Limits       Limits            `json:"limits"`
}
type ResearchConfig struct {
	Connection  string `json:"connection,omitempty"`
	Enabled     bool   `json:"enabled"`
	Format      string `json:"format"`
	BaseURL     string `json:"baseURL"`
	SecretFile  string `json:"secretFile,omitempty"`
	MaxSearches int    `json:"maxSearches"`
	MaxFetches  int    `json:"maxFetches"`
}
type SourceConfig struct {
	Connection         string `json:"connection"`
	Model              string `json:"model,omitempty"`
	TranscriptionModel string `json:"transcriptionModel,omitempty"`
}
type Config struct {
	WebsiteCapture    *WebsiteCaptureConfig `json:"websiteCapture,omitempty"`
	Sources           *SourceConfig         `json:"sources,omitempty"`
	Judgments         *decision.Config      `json:"jev,omitempty"`
	Connections       []Connection          `json:"connections"`
	DefaultConnection string                `json:"defaultConnection"`
	LocalOnly         bool                  `json:"localOnly"`
	EmbeddingsLocal   bool                  `json:"embeddingsLocal"`
	Limits            Limits                `json:"limits"`
	Research          *ResearchConfig       `json:"research,omitempty"`
}

var configID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func LoadConfig(path string) (*Config, error) {
	var c Config
	if err := jsonfile.Load(path, 262144, &c); err != nil {
		return nil, errors.New("invalid chat configuration JSON, unknown field, or size limit")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}
func defaultLimits() Limits {
	return Limits{ContextTokens: 16384, OutputTokens: 2048, RunSeconds: 180, MaxRounds: 8, MaxTools: 16, MaxConcurrent: 4, ConnectionConcurrent: 2, EvidenceTokens: 6000, MaxPassages: 12, PerNotePassages: 4, MaxConversations: 0, MaxEvents: 20000, MaxRecordBytes: 64 << 20}
}
func limitFields(v *Limits) []*int {
	return []*int{&v.ContextTokens, &v.OutputTokens, &v.RunSeconds, &v.MaxRounds, &v.MaxTools, &v.MaxConcurrent, &v.ConnectionConcurrent, &v.EvidenceTokens, &v.MaxPassages, &v.PerNotePassages, &v.MaxConversations, &v.MaxEvents, &v.MaxRecordBytes}
}
func normalizeLimits(v Limits, ceiling Limits) (Limits, error) {
	v.RunTokens, v.DailyTokens = 0, 0
	fields, defaults := limitFields(&v), limitFields(&ceiling)
	for i, p := range fields {
		if *p < 0 || *p > math.MaxInt/64 {
			return Limits{}, errors.New("invalid chat limits")
		}
		if *p == 0 {
			*p = *defaults[i]
		}
	}
	if v.OutputTokens >= v.ContextTokens || int64(v.RunSeconds) > math.MaxInt64/int64(time.Second) {
		return Limits{}, errors.New("invalid chat limit relationship")
	}
	return v, nil
}
func boundedLimits(v, ceiling Limits) Limits {
	// A connection declares its own model capacity. Global defaults must not
	// silently reduce an explicitly configured context or output limit.
	a, b := limitFields(&v)[2:], limitFields(&ceiling)[2:]
	for i, p := range a {
		// Zero conversation count means no product quota; explicit positive
		// operator quotas still compose across instance and connection.
		if i == 8 && (*p == 0 || *b[i] == 0) {
			*p = max(*p, *b[i])
			continue
		}
		if *p > *b[i] {
			*p = *b[i]
		}
	}
	return v
}
func validateEndpoint(raw string, local bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || strings.ContainsAny(raw, "\r\n\t") || (u.Scheme != "https" && !(local && u.Scheme == "http")) {
		return errors.New("invalid configured provider endpoint")
	}
	return nil
}
func validateConnection(c *Connection, limits Limits) error {
	if !configID.MatchString(c.ID) || strings.TrimSpace(c.Label) == "" || len(c.Label) > 128 || strings.ContainsAny(c.Label, "\r\n\x00") {
		return errors.New("invalid chat connection identity")
	}
	if c.Format != "responses" && c.Format != "messages" && c.Format != "chat-completions" && c.Format != "cloudflare-moondream" && c.Format != "cloudflare-mistral" {
		return errors.New("unsupported chat provider format")
	}
	if c.Format == "cloudflare-moondream" && (c.Cloudflare == nil || len(c.Models) != 1 || c.Models[0] != moondreamModel || c.Capabilities != (Capabilities{Images: true})) {
		return errors.New("native Moondream requires Cloudflare, its vision model and non-streaming image capability only")
	}
	if c.Format == "cloudflare-mistral" && (c.Cloudflare == nil || len(c.Models) != 1 || c.Models[0] != mistralModel || c.Capabilities != (Capabilities{Images: true})) {
		return errors.New("native Mistral requires Cloudflare, its vision model and non-streaming image capability only")
	}
	if c.Cloudflare != nil {
		if err := c.Cloudflare.validate(*c); err != nil {
			return err
		}
	}
	if err := validateEndpoint(c.BaseURL, c.Local); err != nil && !(c.Cloudflare != nil && c.BaseURL == "" && !c.Local) {
		return err
	}
	if c.SecretFile != "" && !filepath.IsAbs(c.SecretFile) {
		return errors.New("chat secret file must use an absolute path")
	}
	if len(c.Models) == 0 || len(c.Models) > 100 {
		return errors.New("chat connection requires allowed models")
	}
	found := false
	seen := map[string]bool{}
	for _, m := range c.Models {
		if strings.TrimSpace(m) != m || m == "" || len(m) > 200 || strings.ContainsAny(m, "\r\n\x00") || seen[m] {
			return errors.New("invalid chat model allowlist")
		}
		seen[m] = true
		if m == c.DefaultModel {
			found = true
		}
	}
	if c.DefaultModel == "" {
		c.DefaultModel = c.Models[0]
		found = true
	}
	if !found {
		return errors.New("default model is outside chat allowlist")
	}
	caps := c.Capabilities
	if caps.JSONOutput && c.Format != "chat-completions" && c.Format != "responses" {
		return errors.New("JSON output requires Responses or Chat Completions support")
	}
	if caps.StrictTools && !caps.Tools || caps.StreamUsage && (!caps.Stream || c.Format != "chat-completions") || caps.StoreFalse && c.Format != "responses" || caps.EncryptedReasoning && c.Format != "responses" || caps.CacheControl && c.Format != "messages" || caps.ReasoningContent && c.Format != "chat-completions" {
		return errors.New("unsupported chat capability combination")
	}
	if caps.Thinking != "" && caps.Thinking != "enabled" && caps.Thinking != "disabled" {
		return errors.New("invalid configured thinking mode")
	}
	if caps.Thinking != "" && c.Format == "responses" || caps.Thinking == "enabled" && c.Format == "chat-completions" && !caps.ReasoningContent {
		return errors.New("thinking requires native continuation support")
	}
	if caps.ThinkingBudget != 0 && (c.Format != "messages" || caps.Thinking != "enabled") {
		return errors.New("thinking budget requires Messages thinking")
	}
	if caps.Thinking == "enabled" && c.Format == "messages" && caps.ThinkingBudget < 1024 {
		return errors.New("Messages thinking requires an explicit budget of at least 1024 tokens")
	}
	switch caps.ReasoningEffort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		return errors.New("invalid reasoning effort")
	}
	if caps.ReasoningEffort != "" && c.Format == "messages" {
		return errors.New("reasoning effort is unsupported for Messages")
	}
	expected := "max_tokens"
	if c.Format == "responses" {
		expected = "max_output_tokens"
	}
	if caps.OutputTokenParameter != "" && caps.OutputTokenParameter != expected && !(c.Format == "chat-completions" && caps.OutputTokenParameter == "max_completion_tokens") {
		return errors.New("unsupported output token parameter")
	}
	var err error
	c.Limits, err = normalizeLimits(c.Limits, limits)
	if err != nil {
		return err
	}
	c.Limits = boundedLimits(c.Limits, limits)
	if caps.ThinkingBudget >= c.Limits.OutputTokens {
		return errors.New("thinking budget must leave room for output")
	}
	return nil
}
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("missing chat configuration")
	}
	if err := c.Judgments.Validate(c.LocalOnly); err != nil {
		return err
	}
	var err error
	c.Limits, err = normalizeLimits(c.Limits, defaultLimits())
	if err != nil {
		return err
	}
	if len(c.Connections) == 0 || len(c.Connections) > 32 {
		return errors.New("chat requires between one and 32 configured connections")
	}
	ids := map[string]bool{}
	for i := range c.Connections {
		conn := &c.Connections[i]
		if err := validateConnection(conn, c.Limits); err != nil {
			return err
		}
		if ids[conn.ID] {
			return errors.New("duplicate chat connection ID")
		}
		ids[conn.ID] = true
		if c.LocalOnly && !conn.Local {
			return errors.New("local-only chat prohibits cloud connections")
		}
	}
	if c.DefaultConnection == "" {
		c.DefaultConnection = c.Connections[0].ID
	}
	if !ids[c.DefaultConnection] {
		return errors.New("default chat connection is unknown")
	}
	if source := c.Sources; source != nil {
		var selected *Connection
		for i := range c.Connections {
			if c.Connections[i].ID == source.Connection {
				selected = &c.Connections[i]
			}
		}
		if selected == nil || selected.Format == "messages" {
			return errors.New("source processing requires an explicit OpenAI-compatible connection")
		}
		if source.Model == "" && source.TranscriptionModel == "" {
			return errors.New("source processing requires an explicit vision or transcription model")
		}
		if source.Model != "" {
			allowed := false
			for _, model := range selected.Models {
				allowed = allowed || model == source.Model
			}
			if selected.Format != "responses" || !selected.Capabilities.Images || !allowed {
				return errors.New("source vision requires an allowed Responses image model")
			}
		}
		if len(source.TranscriptionModel) > 128 || strings.ContainsAny(source.TranscriptionModel, "\r\n\x00") || strings.TrimSpace(source.TranscriptionModel) != source.TranscriptionModel {
			return errors.New("invalid source transcription model")
		}
	}
	if c.WebsiteCapture != nil {
		if c.LocalOnly {
			return errors.New("local-only chat prohibits Cloudflare website capture")
		}
		if err := c.WebsiteCapture.Validate(); err != nil {
			return err
		}
	}
	if r := c.Research; r != nil && r.Enabled {
		if c.LocalOnly {
			return errors.New("local-only chat prohibits external research")
		}
		switch r.Format {
		case "brave":
			if r.Connection != "" {
				return errors.New("Brave research does not use a model connection")
			}
			if validateEndpoint(r.BaseURL, false) != nil {
				return errors.New("invalid research endpoint")
			}
			if r.SecretFile == "" || !filepath.IsAbs(r.SecretFile) {
				return errors.New("research requires a configured secret file")
			}
		case "openai":
			if r.BaseURL != "" || r.SecretFile != "" {
				return errors.New("OpenAI research uses only a configured connection")
			}
			conn, err := c.Resolve(r.Connection, "")
			if err != nil || r.Connection == "" || conn.Format != "responses" {
				return errors.New("OpenAI research requires a configured Responses connection")
			}
		default:
			return errors.New("unsupported research format")
		}
		if r.MaxSearches == 0 {
			r.MaxSearches = 3
		}
		if r.MaxFetches == 0 {
			r.MaxFetches = 5
		}
		if r.MaxSearches < 1 || r.MaxSearches > 20 || r.MaxFetches < 1 || r.MaxFetches > 20 {
			return errors.New("invalid research limits")
		}
	}
	return nil
}
func (c *Config) Resolve(connectionID, model string) (Connection, error) {
	if c == nil {
		return Connection{}, errors.New("chat is not configured")
	}
	if connectionID == "" {
		connectionID = c.DefaultConnection
	}
	for _, conn := range c.Connections {
		if conn.ID != connectionID {
			continue
		}
		if c.LocalOnly && !conn.Local {
			return Connection{}, errors.New("local-only chat prohibits cloud connections")
		}
		if model == "" {
			model = conn.DefaultModel
		}
		for _, m := range conn.Models {
			if m == model {
				return conn, nil
			}
		}
		return Connection{}, errors.New("model is no longer in the configured allowlist")
	}
	return Connection{}, errors.New("chat connection is no longer configured")
}

type PublicConnection struct {
	ID           string       `json:"id"`
	Label        string       `json:"label"`
	Models       []string     `json:"models"`
	DefaultModel string       `json:"defaultModel"`
	Local        bool         `json:"local"`
	Capabilities Capabilities `json:"capabilities"`
	Limits       Limits       `json:"limits"`
	Verified     bool         `json:"verified"`
}

func (c *Config) PublicConnections() any {
	list := []PublicConnection{}
	if c == nil {
		return list
	}
	for _, conn := range c.Connections {
		list = append(list, PublicConnection{ID: conn.ID, Label: conn.Label, Models: append([]string(nil), conn.Models...), DefaultModel: conn.DefaultModel, Local: conn.Local, Capabilities: conn.Capabilities, Limits: conn.Limits})
	}
	return list
}
