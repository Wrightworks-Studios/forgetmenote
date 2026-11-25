package web

import (
	"context"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/jev"
	"forgetmenote/internal/quicknotes"
	"forgetmenote/internal/workspace"
)

// initJev opens Jev's reviews and organizer, bound to this instance's
// identity, policy and authorization. The chat runtime, the decision policy
// and Quick Notes are configured after it, so each is read at each call.
func (s *Server) initJev() error {
	var err error
	s.jev, err = jev.New(s.catalog, s.db, s.outgoing, s.indexes, jev.Hooks{
		Identity:                s.interactiveIdentity,
		RejectNotebookAI:        s.rejectNotebookAI,
		Judge:                   s.jevConfigured,
		Authorize:               s.jevAuthorized,
		InboxAIAllowed:          s.inboxAIAllowed,
		ProtectedInboxAIAllowed: s.protectedInboxAIAllowed,
		Preferences:             s.decisionPreferences,
		SyncInstanceAI:          s.syncInstanceAI,
		Quicknotes:              func() *quicknotes.Service { return s.quicknotes },
	})
	if err != nil {
		return err
	}
	s.citations = s.jev.Citations()
	return nil
}

func (s *Server) jevRoutes() {
	s.jev.Register(s.handleProject, s.mux.HandleFunc)
	s.handleProject("GET /api/chat/{chat}/runs/{run}/citations", "read", s.jevCitations)
	s.handleProject("POST /api/chat/{chat}/runs/{run}/citations", "read", s.jevCitations)
}

func (s *Server) jevConfigured() (*decision.Service, bool) {
	snapshot := s.chat.Snapshot()
	localOnly := snapshot.Config != nil && snapshot.Config.LocalOnly
	return snapshot.Jev, localOnly
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

// jevAuthorized is the shared Jev admission for every feature, explicit or
// automatic, so a protected Inbox reaches Jev only for its consenting owner.
func (s *Server) jevAuthorized(ctx context.Context, user, project, feature string, expected *decision.Service) error {
	if err := s.protectedInboxAIAllowed(user, project); err != nil {
		return err
	}
	return s.decisions.Authorize(ctx, user, project, feature, expected)
}
