package web

import (
	"context"
	"errors"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/workspace"
	"net/http"
	"os"
)

func (s *Server) jevRoutes() {
	s.jevJobRoutes()
	s.organizationRoutes()
	s.handleProject("GET /api/chat/{chat}/runs/{run}/citations", "read", s.jevCitations)
	s.handleProject("POST /api/chat/{chat}/runs/{run}/citations", "read", s.jevCitations)
	s.mux.HandleFunc("GET /api/jev/settings", s.jevSettings)
	s.mux.HandleFunc("POST /api/jev/settings", s.jevSettings)
	s.handleProject("GET /api/jev/settings", "read", s.jevNotebookSettings)
	s.handleProject("POST /api/jev/settings", "owner", s.jevNotebookSettings)
}

func (s *Server) jevConfigured() (*decision.Service, bool) {
	snapshot := s.chat.Snapshot()
	localOnly := snapshot.Config != nil && snapshot.Config.LocalOnly
	return snapshot.Jev, localOnly
}

func (s *Server) jevSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" && s.rejectNotebookAI(w) {
		return
	}
	user, err := s.interactiveIdentity(r)
	if err != nil {
		chatFail(w, err)
		return
	}
	preferences, err := s.decisionPreferences(user)
	if err != nil {
		chatFail(w, err)
		return
	}
	judge, localOnly := s.jevConfigured()
	if r.Method == "POST" {
		if !decode(w, r, &preferences) {
			return
		}
		s.jevState.organizationMu.Lock()
		defer s.jevState.organizationMu.Unlock()
		if s.rejectNotebookAI(w) {
			return
		}
		if current, e := s.interactiveIdentity(r); e != nil || current != user {
			chatFail(w, workspace.ErrForbidden)
			return
		}
		if localOnly && preferences.Consent {
			chatFail(w, decision.ErrLocalOnly)
			return
		}
		s.jevState.mu.Lock()
		var record jevUserSettings
		err = s.jevState.update("users/"+digest(user)+".json", &record, func(bool) error {
			if !preferences.Consent {
				record.OrganizationGeneration++
			}
			record.Version, record.Owner, record.Settings = 1, user, preferences
			return nil
		})
		s.jevState.mu.Unlock()
		if err != nil {
			chatFail(w, err)
			return
		}
		if !preferences.Consent {
			if err = s.invalidateOrganizationJobs(func(job jevJob) bool { return job.Owner == user }); err != nil {
				chatFail(w, err)
				return
			}
		}
	}
	policy, managed := s.catalog.AISettings()
	reviews := preferences.Consent
	if managed {
		reviews = policy.Reviews
	}
	jsonResponse(w, map[string]any{"available": judge != nil && !localOnly, "localOnly": localOnly, "managed": managed, "reviews": reviews, "settings": preferences})
}

func (s *Server) jevNotebookSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" && s.rejectNotebookAI(w) {
		return
	}
	user, err := s.interactiveIdentity(r)
	if err != nil {
		chatFail(w, err)
		return
	}
	project := projectID(r.Context())
	_, ownerErr := s.catalog.Access(user, project, "owner")
	judge, localOnly := s.jevConfigured()
	if policy, managed := s.catalog.AISettings(); managed {
		jsonResponse(w, map[string]any{"available": judge != nil && !localOnly, "canManage": false, "managed": true, "settings": map[string]any{"autoChecks": policy.Reviews}})
		return
	}
	var input struct {
		AutoChecks bool `json:"autoChecks"`
	}
	if r.Method == "POST" {
		if ownerErr != nil {
			chatFail(w, ownerErr)
			return
		}
		if !decode(w, r, &input) {
			return
		}
	}
	s.jevState.mu.Lock()
	defer s.jevState.mu.Unlock()
	if r.Method == "POST" {
		if s.rejectNotebookAI(w) {
			return
		}
		if current, e := s.interactiveIdentity(r); e != nil || current != user {
			chatFail(w, workspace.ErrForbidden)
			return
		}
		if _, e := s.catalog.Access(user, project, "owner"); e != nil {
			chatFail(w, e)
			return
		}
	}
	var record jevNotebookSettings
	err = s.jevState.read(jevNotebookName(project), &record)
	if errors.Is(err, os.ErrNotExist) {
		record = jevNotebookSettings{Version: 1, Notebook: project}
	} else if err != nil || record.Version != 1 || record.Notebook != project {
		chatFail(w, decision.ErrStorage)
		return
	}
	if r.Method == "POST" {
		if ownerErr != nil {
			chatFail(w, ownerErr)
			return
		}
		if input.AutoChecks && (judge == nil || localOnly) {
			chatFail(w, decision.ErrDisabled)
			return
		}
		if input.AutoChecks {
			if err := s.protectedInboxAIAllowed(user, project); err != nil {
				chatFail(w, err)
				return
			}
		}
		account, err := s.catalog.Account(user)
		if err != nil {
			chatFail(w, err)
			return
		}
		// The baseline is read before the record is held, so the notebook's
		// snapshot holds no database connection.
		var baseline map[string]string
		baselineRevision := ""
		if input.AutoChecks && !record.AutoChecks {
			store, err := s.catalog.Access(user, project, "owner")
			if err != nil {
				chatFail(w, err)
				return
			}
			snapshot, err := store.SnapshotScoped(r.Context(), "", jevActiveScope())
			if err != nil {
				chatFail(w, err)
				return
			}
			baseline, baselineRevision = map[string]string{}, snapshot.Revision
			for _, file := range snapshot.Files {
				baseline[file.Path] = file.Hash
			}
		}
		record, err = s.jevState.updateNotebook(project, func(record *jevNotebookSettings, _ bool) error {
			if input.AutoChecks && !record.AutoChecks {
				// Another gateway turned checks off after the read above.
				if baseline == nil {
					return workspace.ErrConflict
				}
				record.Baseline, record.BaselineRevision = baseline, baselineRevision
				record.Observed = nil
				record.ObservedAt = ""
			}
			record.AutoChecks = input.AutoChecks
			record.EnabledBy = user
			record.AccountID = account.ID
			return nil
		})
		if err != nil {
			chatFail(w, err)
			return
		}
	}
	jsonResponse(w, map[string]any{"available": judge != nil && !localOnly, "canManage": ownerErr == nil, "settings": map[string]any{"autoChecks": record.AutoChecks, "enabledBy": record.EnabledBy}})
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
