package web

import (
	"context"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/judgments"
	"forgetmenote/internal/quicknotes"
	"forgetmenote/internal/workspace"
)

// initJudgments opens the judgment reviews and organizer, bound to this instance's
// identity, policy and authorization. The chat runtime, the decision policy
// and Quick Notes are configured after it, so each is read at each call.
func (s *Server) initJudgments() error {
	var err error
	s.judgments, err = judgments.New(s.catalog, s.db, s.outgoing, s.indexes, judgments.Hooks{
		Identity:                s.interactiveIdentity,
		RejectNotebookAI:        s.rejectNotebookAI,
		Judge:                   s.judgmentsConfigured,
		Authorize:               s.judgmentsAuthorized,
		InboxAIAllowed:          s.inboxAIAllowed,
		ProtectedInboxAIAllowed: s.protectedInboxAIAllowed,
		Preferences:             s.decisionPreferences,
		SyncInstanceAI:          s.syncInstanceAI,
		Quicknotes:              func() *quicknotes.Service { return s.quicknotes },
	})
	if err != nil {
		return err
	}
	s.citations = s.judgments.Citations()
	return nil
}

func (s *Server) judgmentRoutes(r routeRegistrar) {
	s.judgments.Register(r.project, r.global)
}

func (s *Server) judgmentsConfigured() (*decision.Service, bool) {
	snapshot := s.chat.Snapshot()
	localOnly := snapshot.Config != nil && snapshot.Config.LocalOnly
	return snapshot.Judgments, localOnly
}

// inboxAIAllowed admits model processing of a protected Inbox only for its
// owner, with that owner's personal cloud consent. Admin or Owner authority
// over another person's Inbox never extends to AI processing of its captures.
func (s *Server) inboxAIAllowed(user string, p catalog.Project) error {
	if p.ProtectedInbox == "" {
		return nil
	}
	account, err := s.catalog.Account(user)
	if err != nil || account.ID != p.ProtectedInbox || s.quicknotes == nil {
		return workspace.ErrForbidden
	}
	if settings, err := s.quicknotes.Settings(user); err != nil || !settings.CloudConsent {
		return workspace.ErrForbidden
	}
	return nil
}

func (s *Server) protectedInboxAIAllowed(user, project string) error {
	for _, p := range s.catalog.List(catalog.Bootstrap) {
		if p.ID == project {
			return s.inboxAIAllowed(user, p)
		}
	}
	return nil
}

// judgmentsAuthorized is the shared judgment admission for every feature, explicit or
// automatic, so a protected Inbox reaches the judge only for its consenting owner.
func (s *Server) judgmentsAuthorized(ctx context.Context, user, project, feature string, expected *decision.Service) error {
	if err := s.protectedInboxAIAllowed(user, project); err != nil {
		return err
	}
	return s.decisions.Authorize(ctx, user, project, feature, expected)
}
