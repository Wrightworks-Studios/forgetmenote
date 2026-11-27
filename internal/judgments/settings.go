package judgments

import (
	"errors"
	"forgetmenote/internal/auth"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
	"net/http"
	"os"
	"strings"
)

// Register adds the judgments routes: notebook routes through project, with
// the catalog role each needs, and the person's own routes through global.
// Each /api/judgments route is also served at its former /api/jev path, with
// the same handler and role, so callers of the previous release keep working
// for one more release.
func (s *Service) Register(project func(pattern, role string, handler http.HandlerFunc), global func(pattern string, handler func(http.ResponseWriter, *http.Request))) {
	registerProject, registerGlobal := project, global
	project = func(pattern, role string, handler http.HandlerFunc) {
		for _, p := range withFormerPath(pattern) {
			registerProject(p, role, handler)
		}
	}
	global = func(pattern string, handler func(http.ResponseWriter, *http.Request)) {
		for _, p := range withFormerPath(pattern) {
			registerGlobal(p, handler)
		}
	}
	s.judgmentJobRoutes(project, global)
	s.organizationRoutes(project)
	global("GET /api/judgments/settings", s.judgmentSettings)
	global("POST /api/judgments/settings", s.judgmentSettings)
	project("GET /api/judgments/settings", "read", s.judgmentNotebookSettings)
	project("POST /api/judgments/settings", "owner", s.judgmentNotebookSettings)
}

// withFormerPath is a route pattern and, for a judgments route, the same route
// at its former /api/jev path.
func withFormerPath(pattern string) []string {
	method, path, _ := strings.Cut(pattern, " ")
	if rest, ok := strings.CutPrefix(path, "/api/judgments/"); ok {
		return []string{pattern, method + " /api/jev/" + rest}
	}
	return []string{pattern}
}

func (s *Service) judgmentSettings(w http.ResponseWriter, r *http.Request) {
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
		s.judgmentState.organizationMu.Lock()
		defer s.judgmentState.organizationMu.Unlock()
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
		s.judgmentState.mu.Lock()
		var record judgmentUserSettings
		err = s.judgmentState.update("users/"+auth.Digest(user)+".json", &record, func(bool) error {
			if !preferences.Consent {
				record.OrganizationGeneration++
			}
			record.Version, record.Owner, record.Settings = 1, user, preferences
			return nil
		})
		s.judgmentState.mu.Unlock()
		if err != nil {
			webcore.Fail(w, err)
			return
		}
		if !preferences.Consent {
			if err = s.invalidateOrganizationJobs(func(job judgmentJob) bool { return job.Owner == user }); err != nil {
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

func (s *Service) judgmentNotebookSettings(w http.ResponseWriter, r *http.Request) {
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
	s.judgmentState.mu.Lock()
	defer s.judgmentState.mu.Unlock()
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
	var record judgmentNotebookSettings
	err = s.judgmentState.read(NotebookName(project), &record)
	if errors.Is(err, os.ErrNotExist) {
		record = judgmentNotebookSettings{Version: 1, Notebook: project}
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
			snapshot, err := store.SnapshotScoped(r.Context(), "", judgmentActiveScope())
			if err != nil {
				webcore.Fail(w, err)
				return
			}
			baseline, baselineRevision = map[string]string{}, snapshot.Revision
			for _, file := range snapshot.Files {
				baseline[file.Path] = file.Hash
			}
		}
		record, err = s.judgmentState.updateNotebook(project, func(record *judgmentNotebookSettings, _ bool) error {
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
