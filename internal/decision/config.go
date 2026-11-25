// Package decision owns typed judgments and their shared paid-work ledger.
// It never loads notes, decides permissions, or applies workspace mutations.
package decision

import (
	"errors"
	"forgetmenote/internal/jsonfile"
	"path/filepath"
	"strings"
)

var (
	ErrDisabled         = errors.New("decision service is not enabled")
	ErrLocalOnly        = errors.New("local-only policy prohibits remote decisions")
	ErrLimit            = errors.New("decision execution or storage limit reached")
	ErrInvalid          = errors.New("invalid decision request or provider answer")
	ErrUnavailable      = errors.New("decision service is unavailable")
	ErrStorage          = errors.New("decision usage could not be durably recorded")
	ErrAlreadyAttempted = errors.New("decision operation already attempted; inspect its durable checkpoint")
)

type Config struct {
	Provider   string `json:"provider,omitempty"`
	Enabled    bool   `json:"enabled"`
	Endpoint   string `json:"endpoint,omitempty"`
	Model      string `json:"model"`
	SecretFile string `json:"secretFile,omitempty"`
	Limits     Limits `json:"limits"`
}

func LoadConfig(path string) (*Config, error) {
	var cfg *Config
	if err := jsonfile.Load(path, 65536, &cfg); err != nil || cfg == nil {
		return nil, errors.New("invalid decision configuration JSON, unknown field, or size limit")
	}
	if err := cfg.Validate(false); err != nil {
		return nil, err
	}
	return cfg, nil
}

type Limits struct {
	RequestBytes   int `json:"requestBytes"`
	ResponseBytes  int `json:"responseBytes"`
	Questions      int `json:"questions"`
	TimeoutSeconds int `json:"timeoutSeconds"`
	Retries        int `json:"retries"`
	Concurrent     int `json:"concurrent"`
	// Deprecated compatibility fields are accepted but never limit execution.
	DailyTokens     int64 `json:"dailyTokens,omitempty"`
	UserDailyTokens int64 `json:"userDailyTokens,omitempty"`
	RunTokens       int64 `json:"runTokens,omitempty"`
	RunRequests     int   `json:"runRequests,omitempty"`
	LedgerRecords   int   `json:"ledgerRecords"`
}

func (c *Config) Validate(localOnly bool) error {
	if c == nil {
		return nil
	}
	// Existing chat configuration predates the explicit provider field.
	if c.Provider == "" {
		c.Provider = "typesafe"
	}
	if c.Provider != "typesafe" {
		return errors.New("unsupported decision provider")
	}
	if !c.Enabled {
		return nil
	}
	if localOnly {
		return ErrLocalOnly
	}
	if err := validateTypeSafe(c); err != nil {
		return err
	}
	if !identity(c.Model, 128) {
		return errors.New("decision service requires an explicit operator model")
	}
	if !filepath.IsAbs(c.SecretFile) {
		return errors.New("decision secret file must use an absolute path")
	}
	l := &c.Limits
	fields := []*int{&l.RequestBytes, &l.ResponseBytes, &l.Questions, &l.TimeoutSeconds, &l.Concurrent, &l.LedgerRecords}
	defaults := []int{65536, 65536, 32, 20, 2, 20000}
	maximum := []int{65536, 1048576, 64, 120, 8, 20000}
	for i, p := range fields {
		if *p == 0 {
			*p = defaults[i]
		}
		if *p < 1 || *p > maximum[i] {
			return errors.New("invalid decision limits")
		}
	}
	if l.Retries < 0 || l.Retries > 2 {
		return errors.New("invalid decision retry limit")
	}
	l.DailyTokens, l.UserDailyTokens, l.RunTokens, l.RunRequests = 0, 0, 0, 0

	return nil
}

func identity(s string, limit int) bool {
	if s == "" || len(s) > limit || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if r < 33 || r > 126 {
			return false
		}
	}
	return true
}
