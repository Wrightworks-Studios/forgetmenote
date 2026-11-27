package judgments

import (
	"encoding/json"
	"errors"
	"forgetmenote/internal/attribution"
	"forgetmenote/internal/auth"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

func (s *Service) organizationRoutes(project func(pattern, role string, handler http.HandlerFunc)) {
	project("GET /api/judgments/organization", "read", s.organizationSettingsHTTP)
	project("POST /api/judgments/organization", "owner", s.organizationSettingsHTTP)
	project("POST /api/judgments/jobs/{job}/organization/{action}", "owner", s.organizationControl)
}
func (s *Service) organizationSettingsHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" && s.hooks.RejectNotebookAI(w) {
		return
	}
	user, err := s.hooks.Identity(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	project := webcore.NotebookID(r.Context())
	store, err := s.organizationNotebook(user, project, "read")
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	var input struct {
		Enabled    bool               `json:"enabled"`
		Policy     organizationPolicy `json:"policy"`
		Generation int                `json:"generation"`
	}
	if r.Method == "POST" {
		if _, err = s.catalog.Access(user, project, "owner"); err != nil {
			webcore.Fail(w, err)
			return
		}
		if !webcore.Decode(w, r, &input) {
			return
		}
	}
	s.judgmentState.organizationMu.Lock()
	defer s.judgmentState.organizationMu.Unlock()
	if current, e := s.hooks.Identity(r); e != nil || current != user {
		webcore.Fail(w, workspace.ErrForbidden)
		return
	}
	if r.Method == "POST" && s.hooks.RejectNotebookAI(w) {
		return
	}
	record, err := s.loadJudgmentNotebook(project)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	snapshot, err := store.SnapshotScoped(r.Context(), "", judgmentActiveScope())
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	_, ownerErr := s.catalog.Access(user, project, "owner")
	if record.Organization == nil {
		record.Organization = &organizationSettings{Baseline: map[string]string{}, Suppressed: map[string]string{}}
	}
	judge, localOnly := s.hooks.Judge()
	if r.Method == "POST" {
		if ownerErr != nil {
			webcore.Fail(w, ownerErr)
			return
		}
		if input.Generation != record.Organization.Generation {
			webcore.Fail(w, workspace.ErrConflict)
			return
		}
		if input.Enabled {
			if err = s.hooks.Authorize(r.Context(), user, project, "organization", judge); err != nil {
				webcore.Fail(w, err)
				return
			}
			input.Policy, err = normalizeOrganizationPolicy(input.Policy, snapshot)
			if err != nil {
				webcore.Fail(w, err)
				return
			}
		}
		account, e := s.catalog.Account(user)
		if e != nil {
			webcore.Fail(w, e)
			return
		}
		consent, e := s.judgmentState.organizationConsentGeneration(user)
		if e != nil {
			webcore.Fail(w, e)
			return
		}
		accessEpoch, projectEpoch := s.catalog.Epoch(user), s.catalog.ProjectAccessEpoch(user, project)
		record, err = s.updateOrganization(project, func(o *organizationSettings) error {
			// The generation read above may have moved on through another gateway.
			if input.Generation != o.Generation {
				return workspace.ErrConflict
			}
			if input.Enabled && !o.Enabled {
				o.Baseline = map[string]string{}
				for _, f := range snapshot.Files {
					o.Baseline[f.Path] = f.Hash
				}
				o.Observed = nil
				o.ObservedAt = ""
			}
			o.Enabled = input.Enabled
			o.Policy = input.Policy
			o.Generation++
			o.Owner = user
			o.AccountID = account.ID
			o.ConsentGeneration = consent
			o.AccessEpoch = accessEpoch
			o.ProjectEpoch = projectEpoch
			return nil
		})
		if err != nil {
			webcore.Fail(w, err)
			return
		}
		if err = s.invalidateOrganizationJobs(func(job judgmentJob) bool { return job.Automatic && job.Project == project }); err != nil {
			webcore.Fail(w, err)
			return
		}
	}
	notes := []map[string]string{}
	tags := []string{}
	for _, f := range snapshot.Files {
		notes = append(notes, map[string]string{"path": f.Path, "title": f.Metadata.Title})
		for _, tag := range f.Metadata.Tags {
			if !slices.Contains(tags, tag) {
				tags = append(tags, tag)
			}
		}
	}
	prefs, e := s.hooks.Preferences(user)
	if e != nil {
		webcore.Fail(w, e)
		return
	}
	if policy, managed := s.catalog.AISettings(); managed {
		prefs.Consent = policy.Writing
	}
	o := record.Organization
	public := map[string]any{"enabled": o.Enabled, "generation": o.Generation, "policy": o.Policy, "owner": o.Owner}
	webcore.JSON(w, map[string]any{"available": judge != nil && !localOnly, "localOnly": localOnly, "consent": prefs.Consent, "canManage": ownerErr == nil, "settings": public, "folders": organizationFolders(snapshot), "notes": notes, "tags": tags})
}

// Called with organizationMu held: invalidate the old grant and stop its worker.
// Organization jobs are created under organizationMu too, so none appears on
// this gateway after the listing; each one listed live is rechecked as it is
// cancelled.
func (s *Service) invalidateOrganizationJobs(matches func(judgmentJob) bool) error {
	jobs, err := s.judgmentState.jobs(map[string]any{"organization": map[string]any{}})
	if err != nil {
		return err
	}
	live := func(job judgmentJob) bool {
		return job.Organization != nil && matches(job) && !job.Cancelled && job.State != "completed" && job.State != "failed"
	}
	for _, job := range jobs {
		if !live(job) {
			continue
		}
		s.judgmentState.mu.Lock()
		_, err = s.judgmentState.updateJob(job.ID, func(job *judgmentJob) error {
			if live(*job) {
				job.Cancelled, job.State, job.Message = true, "cancelled", "Organization authorization was withdrawn. Applied actions remain available for Undo."
			}
			return nil
		})
		if cancel := s.judgmentState.active["job:"+job.ID]; err == nil && cancel != nil {
			cancel()
		}
		s.judgmentState.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) recordOrganizationBaseline(project string, result *workspace.OrganizationResult) error {
	if result == nil {
		return nil
	}
	s.judgmentState.mu.Lock()
	defer s.judgmentState.mu.Unlock()
	_, err := s.judgmentState.updateNotebook(project, func(record *judgmentNotebookSettings, _ bool) error {
		if record.Organization == nil || !record.Organization.Enabled {
			return nil
		}
		o := record.Organization
		if o.Baseline == nil {
			o.Baseline = map[string]string{}
		}
		for _, f := range result.Files {
			if !strings.HasSuffix(f.Path, ".md") || workspace.IsTrash(f.Path) || strings.HasPrefix(f.Path, "comments/") {
				continue
			}
			if f.After == nil {
				delete(o.Baseline, f.Path)
			} else {
				o.Baseline[f.Path] = workspace.ContentHash([]byte(*f.After))
			}
		}
		// Observed user changes remain subject to the same debounce on the next scan.
		return nil
	})
	return err
}
func (s *Service) suppressOrganization(project, reason string, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	s.judgmentState.mu.Lock()
	defer s.judgmentState.mu.Unlock()
	_, err := s.judgmentState.updateNotebook(project, func(record *judgmentNotebookSettings, _ bool) error {
		if record.Organization == nil {
			record.Organization = &organizationSettings{}
		}
		if record.Organization.Suppressed == nil {
			record.Organization.Suppressed = map[string]string{}
		}
		for _, key := range keys {
			if record.Organization.Suppressed[key] == reason {
				continue
			}
			if len(record.Organization.Suppressed) >= 10000 && record.Organization.Suppressed[key] == "" {
				return decision.ErrLimit
			}
			record.Organization.Suppressed[key] = reason
		}
		return nil
	})
	return err
}
func actionFromResult(result judgmentJobResult) (organizationAction, error) {
	raw, err := json.Marshal(result.Details)
	if err != nil {
		return organizationAction{}, err
	}
	var action organizationAction
	err = json.Unmarshal(raw, &action)
	return action, err
}

// saveOrganizationResult writes the result at index. A caller that changes a
// result it read passes that result as prior, and the write succeeds only over
// it, so a change made through another gateway meanwhile is refused rather
// than overwritten.
func (s *Service) saveOrganizationResult(job judgmentJob, index int, prior *judgmentJobResult, result judgmentJobResult) error {
	raw, _ := json.Marshal(result)
	next := judgmentResultRecord{Version: 1, Job: job.ID, Index: index, Hash: auth.Digest(string(raw)), Result: result}
	s.judgmentState.mu.Lock()
	defer s.judgmentState.mu.Unlock()
	if prior == nil {
		return s.judgmentState.write(resultName(job.ID, index), next)
	}
	read, _ := json.Marshal(*prior)
	var record judgmentResultRecord
	return s.judgmentState.update(resultName(job.ID, index), &record, func(found bool) error {
		if !found || record.Hash != auth.Digest(string(read)) {
			return workspace.ErrConflict
		}
		record = next
		return nil
	})
}
func (s *Service) organizationControl(w http.ResponseWriter, r *http.Request) {
	user, err := s.hooks.Identity(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	var input struct {
		Index *int `json:"index"`
	}
	if !webcore.Decode(w, r, &input) {
		return
	}
	s.judgmentState.organizationMu.Lock()
	defer s.judgmentState.organizationMu.Unlock()
	if current, e := s.hooks.Identity(r); e != nil || current != user {
		webcore.Fail(w, workspace.ErrForbidden)
		return
	}
	s.judgmentState.mu.Lock()
	job, err := s.judgmentState.readJob(r.PathValue("job"))
	active := s.judgmentState.active["job:"+job.ID] != nil
	s.judgmentState.mu.Unlock()
	if err != nil || job.Project != webcore.NotebookID(r.Context()) || job.Organization == nil {
		webcore.Fail(w, chat.ErrNotFound)
		return
	}
	if active || job.State == "queued" {
		webcore.Fail(w, errors.New("cancel or finish organization before Undo or dismissal"))
		return
	}
	store, err := s.organizationNotebook(user, job.Project, "owner")
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	account, err := s.catalog.Account(user)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	ctx := attribution.WithSource(r.Context(), account.Identity(), "jev", "organizer-undo")
	command := r.PathValue("action")
	if command != "undo" && command != "dismiss" {
		webcore.Fail(w, decision.ErrInvalid)
		return
	}
	if input.Index != nil && (*input.Index < 0 || *input.Index >= job.Cursor) {
		webcore.Fail(w, decision.ErrInvalid)
		return
	}
	outcomes := []map[string]any{}
	// Only actions touching the same files can explain intervening history. Keep
	// the actual committed file inventory, including replayed action results.
	later := map[string][]workspace.OrganizationUndo{}
	rememberUndo := func(a organizationAction) error {
		if a.Result == nil || a.Undo == nil {
			return decision.ErrStorage
		}
		for _, p := range organizationPaths(a.Result) {
			later[p] = append(later[p], *a.Undo)
		}
		return nil
	}
	for index := job.Cursor - 1; index >= 0; index-- {
		record, e := s.readJudgmentResult(job, index)
		if e != nil {
			webcore.Fail(w, e)
			return
		}
		prior := record
		a, e := actionFromResult(record)
		if e != nil {
			webcore.Fail(w, e)
			return
		}
		if a.UndoRevision != "" && a.Undo != nil {
			if e := rememberUndo(a); e != nil {
				webcore.Fail(w, e)
				return
			}
			continue
		}
		if input.Index != nil && *input.Index != index {
			continue
		}
		if command == "dismiss" {
			if a.Status == "applied" {
				outcomes = append(outcomes, map[string]any{"index": index, "error": "Use Undo for an applied action."})
				continue
			}
			if a.Key != "" {
				if e = s.suppressOrganization(job.Project, "dismissed", a.Key); e != nil {
					webcore.Fail(w, e)
					return
				}
			}
			a.Status = "dismissed"
			record.Status = a.Status
			record.Details = organizationDetails(a)
			if e = s.saveOrganizationResult(job, index, &prior, record); e != nil {
				webcore.Fail(w, e)
				return
			}
			continue
		}
		if a.Status != "applied" || a.Request == nil || a.Result == nil {
			continue
		}
		if e = s.suppressOrganization(job.Project, "undone", a.Key); e != nil {
			webcore.Fail(w, e)
			return
		}
		if a.Undo == nil {
			a.Undo = &workspace.OrganizationUndo{ID: job.ID + ":" + strconv.Itoa(index), Action: *a.Request, Actor: job.AccountID, Executor: account.ID, Revision: a.Result.Revision}
			record.Details = organizationDetails(a)
			if e = s.saveOrganizationResult(job, index, &prior, record); e != nil {
				webcore.Fail(w, e)
				return
			}
			prior = record
		}
		undo := *a.Undo
		undo.Later = nil
		seen := map[string]bool{}
		for _, p := range organizationPaths(a.Result) {
			for _, dependent := range later[p] {
				if !seen[dependent.ID] {
					seen[dependent.ID] = true
					undo.Later = append(undo.Later, dependent)
				}
			}
		}
		var restored workspace.OrganizationResult
		e = s.catalog.WithProjectAuthority(user, job.Project, account.ID, "owner", s.catalog.Epoch(user), s.catalog.ProjectAccessEpoch(user, job.Project), func(authorized *workspace.Store) error {
			var err error
			restored, err = authorized.UndoOrganization(ctx, undo)
			return err
		})
		if e != nil {
			outcomes = append(outcomes, map[string]any{"index": index, "error": e.Error()})
			continue
		}
		a.UndoRevision = restored.Revision
		a.Status = "undone"
		if summary := organizationTrashSummary(restored.TrashLinks, "Links reverted in notes in the Trash", "Notes in the Trash left as they are"); summary != "" {
			a.Reason = "Undone." + summary
			record.Summary = a.Reason
		}
		record.Status = "undone"
		record.Details = organizationDetails(a)
		if e = s.recordOrganizationBaseline(job.Project, &restored); e != nil {
			webcore.Fail(w, e)
			return
		}
		if e = s.saveOrganizationResult(job, index, &prior, record); e != nil {
			webcore.Fail(w, e)
			return
		}
		if e := rememberUndo(a); e != nil {
			webcore.Fail(w, e)
			return
		}
		outcomes = append(outcomes, map[string]any{"index": index, "revision": restored.Revision})
	}
	// Reverse dependent actions can restore earlier evidence than an individual
	// action saw (for example, a link applied after a tag). Suppress that restored
	// evidence too, so Undo run does not immediately recreate the later action.
	if command == "undo" {
		snapshot, e := store.SnapshotScoped(ctx, "", judgmentActiveScope())
		if e != nil {
			webcore.Fail(w, e)
			return
		}
		restoredJob := job
		run := *job.Organization
		run.Paths = map[string]string{}
		restoredJob.Organization = &run
		restoredJob.Notes = []judgmentJobNote{}
		locks, e := store.Locks(ctx, "")
		if e != nil {
			webcore.Fail(w, e)
			return
		}
		evidence := map[string]knowledge.JudgmentEvidence{}
		for _, f := range snapshot.Files {
			restoredJob.Notes = append(restoredJob.Notes, judgmentJobNote{Path: f.Path, Revision: snapshot.Revision, Hash: f.Hash})
			if len(f.Content) > 24<<10 {
				continue
			}
			if _, err := workspace.ParseMetadata([]byte(f.Content)); err != nil {
				continue
			}
			locked := false
			for _, rule := range locks.Rules {
				if f.Path == rule.Path || strings.HasPrefix(f.Path, rule.Path+"/") {
					locked = true
					break
				}
			}
			if !locked {
				evidence[f.Path] = knowledge.JudgmentEvidence{Path: f.Path, Revision: snapshot.Revision, Hash: f.Hash, Text: f.Content, URL: evidenceURL(f.Path, snapshot.Revision)}
			}
		}
		// Suppression does not authorize writes or send text to a provider. A
		// snapshot hash remains safe even if a later edit changes the live note.
		read := func(p string) (knowledge.JudgmentEvidence, error) {
			if value, ok := evidence[p]; ok {
				return value, nil
			}
			return knowledge.JudgmentEvidence{}, workspace.ErrForbidden
		}
		keys := []string{}
		for i := 0; i < job.Cursor; i++ {
			record, e := s.readJudgmentResult(job, i)
			if e != nil {
				webcore.Fail(w, e)
				return
			}
			a, e := actionFromResult(record)
			if e != nil {
				webcore.Fail(w, e)
				return
			}
			if a.Status != "undone" {
				continue
			}
			if _, _, e = organizationRequest(restoredJob, &a, read); e == nil {
				keys = append(keys, a.Key)
			}
		}
		if e = s.suppressOrganization(job.Project, "undone", keys...); e != nil {
			webcore.Fail(w, e)
			return
		}
	}
	webcore.JSON(w, map[string]any{"outcomes": outcomes})
}

// updateOrganization changes a notebook's current organization settings
// while holding their record, so a baseline, suppression or setting changed
// through another gateway is kept.
func (s *Service) updateOrganization(project string, change func(*organizationSettings) error) (judgmentNotebookSettings, error) {
	s.judgmentState.mu.Lock()
	defer s.judgmentState.mu.Unlock()
	return s.judgmentState.updateNotebook(project, func(record *judgmentNotebookSettings, _ bool) error {
		if record.Organization == nil {
			record.Organization = &organizationSettings{Baseline: map[string]string{}, Suppressed: map[string]string{}}
		}
		return change(record.Organization)
	})
}
