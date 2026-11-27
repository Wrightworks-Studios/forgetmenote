package web

import (
	"errors"
	"fmt"

	"forgetmenote/internal/instanceconfig"
)

// ConfigureInstance is the startup composition boundary. Runtime invokes it
// before recovery and admission; a failed application is closed, never served.
// File adapters remain for existing operators and feature-level consumers.
func (s *Server) ConfigureInstance(resolved instanceconfig.Resolved) error {
	unlock, err := s.startupConfiguration()
	if err != nil {
		return err
	}
	unlock()
	c := resolved.Config
	if c.Version != 1 {
		return errors.New("unsupported instance configuration version")
	}
	// First, so that every later step can tell which modules this instance runs.
	if err := s.ConfigureModules(c.Modules.Off); err != nil {
		return fmt.Errorf("instance modules: %w", err)
	}
	// Before any integration is installed, so none is ever briefly unpaused.
	if err := s.ConfigureOutgoing(c.Server.Outgoing); err != nil {
		return fmt.Errorf("instance outgoing: %w", err)
	}
	for _, file := range resolved.Files {
		if err := s.privateRuntimeFile(file); err != nil {
			return err
		}
	}
	secret := func(file string) (string, error) {
		if err := s.privateRuntimeFile(file); err != nil {
			return "", err
		}
		return runtimeSecret(file)
	}
	for _, step := range []struct {
		name  string
		apply func() error
	}{
		{"data", func() error {
			if c.Integrations.Data != nil {
				return s.ConfigureDataConnectionsConfig(*c.Integrations.Data)
			}
			return nil
		}},
		{"personal", func() error {
			if c.Integrations.Personal != nil {
				return s.ConfigurePersonalConnectionsConfig(*c.Integrations.Personal)
			}
			return nil
		}},
		{"python", func() error {
			if c.Integrations.Python != "" {
				return s.ConfigureAutomationPython(c.Integrations.Python)
			}
			return nil
		}},
		{"automations", func() error {
			if c.Integrations.Automations != nil {
				return s.ConfigureAutomationsConfig(*c.Integrations.Automations)
			}
			return nil
		}},
		{"slack", func() error {
			if c.Integrations.Slack != nil {
				return s.ConfigureSlackConfig(*c.Integrations.Slack)
			}
			return nil
		}},
	} {
		if err := step.apply(); err != nil {
			return fmt.Errorf("instance %s: %w", step.name, err)
		}
	}
	if chat := c.AI.Chat; chat != nil {
		files := []string{}
		for _, conn := range chat.Connections {
			if conn.Cloudflare != nil {
				files = append(files, conn.Cloudflare.AccountIDFile)
			}
			if conn.SecretFile != "" {
				files = append(files, conn.SecretFile)
			}
		}
		if chat.Research != nil && chat.Research.Enabled && chat.Research.SecretFile != "" {
			files = append(files, chat.Research.SecretFile)
		}
		if chat.WebsiteCapture != nil {
			files = append(files, chat.WebsiteCapture.AccountIDFile, chat.WebsiteCapture.SecretFile)
		}
		if chat.Judgments != nil && chat.Judgments.Enabled {
			files = append(files, chat.Judgments.SecretFile)
		}
		for _, file := range files {
			if _, err := secret(file); err != nil {
				return err
			}
		}
	}
	if c.AI.Decisions != nil && c.AI.Decisions.Enabled {
		if _, err := secret(c.AI.Decisions.SecretFile); err != nil {
			return err
		}
	}
	if c.AI.Chat != nil || c.AI.Decisions != nil {
		if err := s.ConfigureAIConfig(c.AI.Chat, c.AI.Decisions); err != nil {
			return err
		}
	}
	if embeddings := c.AI.Embeddings; embeddings != nil {
		if embeddings.URL == "" {
			return errors.New("embedding configuration requires a URL")
		}
		credential := ""
		if embeddings.SecretFile != "" {
			credential, err = secret(embeddings.SecretFile)
			if err != nil {
				return err
			}
		}
		if err := s.ConfigureEmbeddings(embeddings.URL, credential); err != nil {
			return err
		}
	}
	if github := c.Identity.GitHub; github != nil {
		if github.ClientID == "" || github.SecretFile == "" {
			return errors.New("GitHub identity requires clientId and secretFile")
		}
		credential, err := secret(github.SecretFile)
		if err != nil {
			return err
		}
		if err := s.ConfigureGitHub(github.ClientID, credential); err != nil {
			return err
		}
	}
	if proxy := c.Identity.Proxy; proxy != nil {
		credential, err := secret(proxy.SecretFile)
		if err != nil {
			return err
		}
		if err := s.ConfigureProxy(credential, proxy.TrustedCIDRs); err != nil {
			return err
		}
	}
	if err := s.catalog.SeedAdmins(c.Identity.Admins); err != nil {
		return err
	}
	if oidc := c.Identity.OIDC; oidc != nil {
		if oidc.Issuer == "" || oidc.ClientID == "" || oidc.SecretFile == "" {
			return errors.New("OIDC requires issuer, clientId and secretFile")
		}
		credential, err := secret(oidc.SecretFile)
		if err != nil {
			return err
		}
		if err := s.ConfigureOIDC(oidc.Issuer, oidc.ClientID, credential, oidc.DefaultGroups...); err != nil {
			return err
		}
		s.oidc.requestEmail = oidc.RequestEmail
	}
	if c.Integrations.Repository != nil {
		if err := s.ConfigureRepositorySourcesConfig(*c.Integrations.Repository); err != nil {
			return err
		}
	}
	if c.Integrations.Backups != nil {
		if err := s.ConfigureBackupsConfig(*c.Integrations.Backups); err != nil {
			return err
		}
	}
	return nil
}

// InstanceSummary is deliberately credential-free. It describes installed
// boundaries, not consent: instance AI policy and per-notebook grants still
// decide whether a configured integration may process content.
func (s *Server) InstanceSummary() map[string]bool {
	cfg := s.chat.Snapshot().Config
	judge, _ := s.judgmentsConfigured()
	automations := s.automations != nil && len(s.automations.Definitions.List()) > 0
	return map[string]bool{
		"chat": cfg != nil, "decisions": judge != nil, "embeddings": s.indexes.Endpoint() != nil,
		"dataConnections": s.dataConnections != nil, "personalConnections": s.personalConnections != nil,
		"slack": s.slack != nil, "repositorySources": s.repositories != nil, "managedBackups": s.backups != nil,
		"automations": automations, "automationPython": s.python != nil,
		"trustedProxy": s.proxy != nil, "oidc": s.oidc != nil, "githubIdentity": s.github != nil, "headless": s.headless,
		"outgoingPaused": s.outgoing.Paused(),
	}
}
