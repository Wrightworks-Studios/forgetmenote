package chat

import (
	"errors"
	"fmt"
	"forgetmenote/internal/decision"
)

// Hooks connect cross-owner policy updates to a single configuration lifetime.
type ConfigurationHooks struct {
	Guard     func() func()
	Installed func(previous, current *decision.Service, localOnly bool) error
}

func (r *Runtime) Configure(data, chatPath, decisionPath string, hooks ConfigurationHooks) error {
	var cfg *Config
	var judge *decision.Config
	var err error
	if chatPath != "" {
		cfg, err = LoadConfig(chatPath)
		if err != nil {
			return fmt.Errorf("CHAT_CONFIG_FILE: %w", err)
		}
	}
	if decisionPath != "" {
		judge, err = decision.LoadConfig(decisionPath)
		if err != nil {
			return fmt.Errorf("DECISION_CONFIG_FILE: %w", err)
		}
	}
	return r.ConfigureValues(data, cfg, judge, hooks)
}

// ConfigureValues is the typed installation boundary for both supported operator
// files and the sectioned instance configuration. It never runs provider work.
func (r *Runtime) ConfigureValues(data string, cfg *Config, judgeConfig *decision.Config, hooks ConfigurationHooks) error {
	r.configMu.Lock()
	defer r.configMu.Unlock()
	var err error
	var localOnly bool
	if cfg != nil {
		if err := cfg.Validate(); err != nil {
			return err
		}
		localOnly = cfg.LocalOnly
		if cfg.Jev != nil {
			if judgeConfig != nil {
				return errors.New("configure decisions in DECISION_CONFIG_FILE or CHAT_CONFIG_FILE, not both")
			}
			judgeConfig = cfg.Jev
		}
	}
	if err := judgeConfig.Validate(localOnly); err != nil {
		return err
	}
	providers := map[string]*Provider{}
	if cfg != nil {
		for _, connection := range cfg.Connections {
			p, err := NewProvider(connection)
			if err != nil {
				return err
			}
			providers[connection.ID] = p
		}
	}
	var website *WebsiteCapture
	if cfg != nil && cfg.WebsiteCapture != nil {
		website, err = NewWebsiteCapture(*cfg.WebsiteCapture)
		if err != nil {
			return err
		}
	}
	var research *Research
	if cfg != nil && cfg.Research != nil && cfg.Research.Enabled {
		if cfg.Research.Format == "openai" {
			research, err = NewConnectionResearch(*cfg.Research, providers[cfg.Research.Connection])
		} else {
			research, err = NewResearch(*cfg.Research)
		}
		if err != nil {
			return err
		}
	}
	var judge *decision.Service
	ledger := r.JevLedger
	openedLedger := false
	if judgeConfig != nil && judgeConfig.Enabled || cfg != nil && cfg.Sources != nil {
		if ledger == nil {
			ledger, err = decision.OpenLedger(r.db)
			if err != nil {
				return err
			}
			openedLedger = true
		}
		if judgeConfig != nil && judgeConfig.Enabled {
			judge, err = decision.New(*judgeConfig, ledger, localOnly)
			if err != nil {
				if openedLedger {
					ledger.Close()
				}
				return err
			}
		}
	}
	if hooks.Guard != nil {
		release := hooks.Guard()
		defer release()
	}
	r.Mu.Lock()
	if r.Closed || len(r.Active) > 0 {
		r.Mu.Unlock()
		judge.Close()
		if openedLedger {
			ledger.Close()
		}
		return errors.New("cannot reconfigure active chat")
	}
	if cfg != nil {
		if err := r.Store.SetLimits(cfg.Limits, cfg.Connections); err != nil {
			r.Mu.Unlock()
			judge.Close()
			if openedLedger {
				ledger.Close()
			}
			return err
		}
	}
	oldJudge := r.Jev
	r.Jev = judge
	r.JevLedger = ledger
	r.Config = cfg
	r.Providers = providers
	r.Research = research
	r.WebsiteCapture = website
	r.Mu.Unlock()
	defer oldJudge.Close()
	if hooks.Installed != nil {
		return hooks.Installed(oldJudge, judge, localOnly)
	}
	return nil
}
