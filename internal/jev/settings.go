package jev

import (
	"errors"
	"forgetmenote/internal/auth"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
	"net/http"
	"os"
)

// Register adds Jev's routes: notebook routes through project, with the
// catalog role each needs, and the person's own routes through global.
func (s *Service) Register(project func(pattern, role string, handler http.HandlerFunc), global func(pattern string, handler func(http.ResponseWriter, *http.Request))) {
	s.jevJobRoutes(project, global)
	s.organizationRoutes(project)
	global("GET /api/jev/settings", s.jevSettings)
	global("POST /api/jev/settings", s.jevSettings)
	project("GET /api/jev/settings", "read", s.jevNotebookSettings)
	project("POST /api/jev/settings", "owner", s.jevNotebookSettings)
}

func (s *Service) jevSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" && s.hooks.RejectNotebookAI(w) {
		return
	}
	user, err := s.hooks.Identity(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	preferences, err := s.hooks.Preferences(user)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	judge, localOnly := s.hooks.Judge()
	if r.Method == "POST" {
		if !webcore.Decode(w, r, &preferences) {
			return
		}
		s.jevState.organizationMu.Lock()
		defer s.jevState.organizationMu.Unlock()
		if s.hooks.RejectNotebookAI(w) {
			return
		}
		if current, e := s.hooks.Identity(r); e != nil || current != user {
			webcore.Fail(w, workspace.ErrForbidden)
			return
		}
		if localOnly && preferences.Consent {
			webcore.Fail(w, decision.ErrLocalOnly)
			return
		}
		s.jevState.mu.Lock()
		var record jevUserSettings
		err = s.jevState.update("users/"+auth.Digest(user)+".json", &record, func(bool) error {
			if !preferences.Consent {
				record.OrganizationGeneration++
			}
			record.Version, record.Owner, record.Settings = 1, user, preferences
			return nil
		})
		s.jevState.mu.Unlock()
		if err != nil {
			webcore.Fail(w, err)
			return
		}
		if !preferences.Consent {
			if err = s.invalidateOrganizationJobs(func(job jevJob) bool { return job.Owner == user }); err != nil {
				webcore.Fail(w, err)
				return
			}
		}
	}
	policy, managed := s.catalog.AISettings()
	reviews := preferences.Consent
	if managed {
		reviews = policy.Reviews
	}
	webcore.JSON(w, map[string]any{"available": judge != nil && !localOnly, "localOnly": localOnly, "managed": managed, "reviews": reviews, "settings": preferences})
}

func (s *Service) jevNotebookSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" && s.hooks.RejectNotebookAI(w) {
		return
	}
	user, err := s.hooks.Identity(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	project := webcore.NotebookID(r.Context())
	_, ownerErr := s.catalog.Access(user, project, "owner")
	judge, localOnly := s.hooks.Judge()
	if policy, managed := s.catalog.AISettings(); managed {
		webcore.JSON(w, map[string]any{"available": judge != nil && !localOnly, "canManage": false, "managed": true, "settings": map[string]any{"autoChecks": policy.Reviews}})
		return
	}
	var input struct {
		AutoChecks bool `json:"autoChecks"`
	}
	if r.Method == "POST" {
		if ownerErr != nil {
			webcore.Fail(w, ownerErr)
			return
		}
		if !webcore.Decode(w, r, &input) {
			return
		}
	}
	s.jevState.mu.Lock()
	defer s.jevState.mu.Unlock()
	if r.Method == "POST" {
		if s.hooks.RejectNotebookAI(w) {
			return
		}
		if current, e := s.hooks.Identity(r); e != nil || current != user {
			webcore.Fail(w, workspace.ErrForbidden)
			return
		}
		if _, e := s.catalog.Access(user, project, "owner"); e != nil {
			webcore.Fail(w, e)
			return
		}
	}
	var record jevNotebookSettings
	err = s.jevState.read(NotebookName(project), &record)
	if errors.Is(err, os.ErrNotExist) {
		record = jevNotebookSettings{Version: 1, Notebook: project}
	} else if err != nil || record.Version != 1 || record.Notebook != project {
		webcore.Fail(w, decision.ErrStorage)
		return
	}
	if r.Method == "POST" {
		if ownerErr != nil {
			webcore.Fail(w, ownerErr)
			return
		}
		if input.AutoChecks && (judge == nil || localOnly) {
			webcore.Fail(w, decision.ErrDisabled)
			return
		}
		if input.AutoChecks {
			if err := s.hooks.ProtectedInboxAIAllowed(user, project); err != nil {
				webcore.Fail(w, err)
				return
			}
		}
		account, err := s.catalog.Account(user)
		if err != nil {
			webcore.Fail(w, err)
			return
		}
		// The baseline is read before the record is held, so the notebook's
		// snapshot holds no database connection.
		var baseline map[string]string
		baselineRevision := ""
		if input.AutoChecks && !record.AutoChecks {
			store, err := s.catalog.Access(user, project, "owner")
			if err != nil {
				webcore.Fail(w, err)
				return
			}
			snapshot, err := store.SnapshotScoped(r.Context(), "", jevActiveScope())
			if err != nil {
				webcore.Fail(w, err)
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
			webcore.Fail(w, err)
			return
		}
	}
	webcore.JSON(w, map[string]any{"available": judge != nil && !localOnly, "canManage": ownerErr == nil, "settings": map[string]any{"autoChecks": record.AutoChecks, "enabledBy": record.EnabledBy}})
}
