package web

import (
	"context"

	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
)

// bindSearchRerank attaches the judge to an explicit notebook search only after the
// checks chat retrieval uses: configured service, not local-only, adopted
// instance policy, personal consent and current read access. It reports whether
// an explicit request could rank, so search-as-you-type can offer the action
// without sending anything. History search keeps its local ranking.
func (s *Server) bindSearchRerank(ctx context.Context, store *workspace.Store, project string, query *knowledge.HybridQuery) (bool, error) {
	requested := query.Rerank
	query.Judgments = nil
	judge, localOnly := s.judgmentsConfigured()
	if judge == nil || localOnly || query.Scope == "history" {
		return false, nil
	}
	user := subject(ctx)
	prefs, err := s.decisionPreferences(user)
	if err != nil {
		return false, err
	}
	if !prefs.Consent || !prefs.Rerank {
		return false, nil
	}
	// Unavailable policy is not a search failure; Check repeats this before
	// and after every outbound request.
	if s.judgmentsAuthorized(ctx, user, project, "rerank", judge) != nil {
		return false, nil
	}
	if !requested {
		return true, nil
	}
	account, err := s.catalog.Account(user)
	if err != nil {
		return false, err
	}
	query.Judgments = &knowledge.JudgmentRanking{
		Service: judge,
		Scope:   decision.Scope{Actor: account.ID, Notebook: project, Feature: "rerank", Run: "notebook-search-" + RandomToken(), Operation: "search"},
		Check: func(ctx context.Context) error {
			if err := s.judgmentsAuthorized(ctx, user, project, "rerank", judge); err != nil {
				return err
			}
			current, err := s.catalog.Access(user, project, "read")
			if err != nil {
				return err
			}
			if current != store {
				return workspace.ErrConflict
			}
			return nil
		},
	}
	return true, nil
}
