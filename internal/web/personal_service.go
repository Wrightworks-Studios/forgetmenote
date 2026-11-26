package web

import (
	"context"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/personal"
)

// initPersonal starts the personal assistant worker with personalHooks. It
// runs after automations and messaging, which the worker uses directly.
func (s *Server) initPersonal() error {
	var err error
	s.personal, err = personal.New(s.catalog, s.automations, s.messaging, s.personalHooks())
	return err
}

// personalHooks binds the worker to this instance's consent and access checks
// (personal_access.go, personal_ui.go, credentials.go), its automation runs,
// Jev, and its connections and Slack app. Connections, the origin and the
// Slack app are read at each call, since web replaces them after startup.
func (s *Server) personalHooks() personal.Hooks {
	return personal.Hooks{
		Context: func(ctx context.Context) ([]personal.Memory, error) {
			out, err := s.personalContext(ctx)
			return out.Memory, err
		},
		ClientAccess:        s.clientAccess,
		Evidence:            s.personalEvidence,
		Current:             s.personalCurrent,
		CheckSources:        s.checkPersonalSources,
		DiscardUnavailable:  s.discardUnavailablePersonal,
		MailboxDraft:        s.personalMailboxDraft,
		CheckAutomationAuth: s.checkAutomationAuth,
		AutomationAnswer: func(ctx context.Context, r *automation.Run, prompt string, tools bool) (string, error) {
			return s.automationAnswer(ctx, r, prompt, tools)
		},
		CheckAutomationEvidence: s.checkAutomationEvidence,
		AutomationCall:          s.automationCall,
		JevConfigured:           s.jevConfigured,
		PersonalConnections:     func() *connectors.PersonalConnections { return s.personalConnections },
		DataConnections:         func() *connectors.Manager { return s.dataConnections },
		Origin:                  func() string { return s.origin },
		Slack: func() (string, messaging.Transport, messaging.MemberVerifier, bool) {
			if s.slack == nil {
				return "", nil, nil, false
			}
			return s.slack.config.Team, s.slack, s.slack, true
		},
	}
}
