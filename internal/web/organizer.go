package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
	"path"
	"slices"
	"sort"
	"strings"
)

type organizationPolicy struct {
	Tags       bool     `json:"tags"`
	Links      bool     `json:"links"`
	Moves      bool     `json:"moves"`
	Duplicates bool     `json:"duplicates"`
	Vocabulary []string `json:"vocabulary,omitempty"`
	Folders    []string `json:"folders,omitempty"`
}
type organizationInput struct {
	Mode       string             `json:"mode"`
	Scope      string             `json:"scope"`
	Folder     string             `json:"folder,omitempty"`
	Paths      []string           `json:"paths,omitempty"`
	Policy     organizationPolicy `json:"policy"`
	Reconsider bool               `json:"reconsider,omitempty"`
}
type organizationRun struct {
	Targets           []string          `json:"targets"`
	ProjectEpoch      uint64            `json:"projectEpoch"`
	ConsentGeneration uint64            `json:"consentGeneration"`
	AccessEpoch       uint64            `json:"accessEpoch"`
	Input             organizationInput `json:"input"`
	Generation        int               `json:"generation"`
	Allowed           []string          `json:"allowed"`
	Paths             map[string]string `json:"paths"`
}
type organizationSettings struct {
	ProjectEpoch      uint64             `json:"projectEpoch"`
	ConsentGeneration uint64             `json:"consentGeneration"`
	AccessEpoch       uint64             `json:"accessEpoch"`
	Enabled           bool               `json:"enabled"`
	Generation        int                `json:"generation"`
	Owner             string             `json:"owner"`
	AccountID         string             `json:"accountId"`
	Policy            organizationPolicy `json:"policy"`
	Baseline          map[string]string  `json:"baseline"`
	Observed          map[string]string  `json:"observed,omitempty"`
	ObservedAt        string             `json:"observedAt,omitempty"`
	Suppressed        map[string]string  `json:"suppressed,omitempty"`
}
type organizationAction struct {
	Vocabulary   []string                      `json:"vocabulary,omitempty"`
	Kind         string                        `json:"kind"`
	Path         string                        `json:"path"`
	Target       string                        `json:"target,omitempty"`
	Key          string                        `json:"key,omitempty"`
	Request      *workspace.OrganizationAction `json:"request,omitempty"`
	Answers      map[string]decision.Answer    `json:"answers,omitempty"`
	Attempted    bool                          `json:"attempted,omitempty"`
	Status       string                        `json:"status,omitempty"`
	Reason       string                        `json:"reason,omitempty"`
	Result       *workspace.OrganizationResult `json:"result,omitempty"`
	Undo         *workspace.OrganizationUndo   `json:"undo,omitempty"`
	UndoRevision string                        `json:"undoRevision,omitempty"`
}

func (s *Server) organizationNotebook(user, project, role string) (*workspace.Store, error) {
	for _, p := range s.catalog.List(user) {
		if p.ID == project && p.ProtectedInbox != "" {
			return nil, workspace.ErrForbidden
		}
	}
	return s.catalog.Access(user, project, role)
}
func organizationFolders(snapshot workspace.Snapshot) []string {
	folders := map[string]bool{"": true}
	for _, f := range snapshot.Files {
		for p := path.Dir(f.Path); p != "." && p != ""; p = path.Dir(p) {
			folders[p] = true
		}
	}
	result := []string{}
	for p := range folders {
		result = append(result, p)
	}
	sort.Strings(result)
	return result
}
func normalizeOrganizationPolicy(p organizationPolicy, snapshot workspace.Snapshot) (organizationPolicy, error) {
	if !p.Tags && !p.Links && !p.Moves && !p.Duplicates {
		return p, errors.New("choose at least one organization action")
	}
	if len(p.Vocabulary) > 100 || len(p.Folders) > 32 {
		return p, decision.ErrLimit
	}
	if p.Tags && len(p.Vocabulary) == 0 {
		for _, f := range snapshot.Files {
			p.Vocabulary = append(p.Vocabulary, f.Metadata.Tags...)
		}
	}
	unique := map[string]bool{}
	tags := []string{}
	for _, tag := range p.Vocabulary {
		tag = strings.TrimSpace(tag)
		if tag == "" || len(tag) > 80 || strings.ContainsAny(tag, "\x00\r\n") {
			return p, decision.ErrInvalid
		}
		key := strings.ToLower(tag)
		if !unique[key] {
			unique[key] = true
			tags = append(tags, tag)
		}
	}
	if len(tags) > 100 {
		return p, errors.New("tag vocabulary exceeds 100; choose an explicit vocabulary")
	}
	sort.Strings(tags)
	p.Vocabulary = tags
	folders := organizationFolders(snapshot)
	unique = map[string]bool{}
	destinations := []string{}
	for _, folder := range p.Folders {
		if !slices.Contains(folders, folder) {
			return p, errors.New("choose existing eligible folders")
		}
		if !unique[folder] {
			destinations = append(destinations, folder)
			unique[folder] = true
		}
	}
	sort.Strings(destinations)
	p.Folders = destinations
	if p.Moves && len(p.Folders) == 0 {
		return p, errors.New("approve at least one existing destination folder")
	}
	return p, nil
}
func (s *Server) createOrganizationJob(ctx context.Context, user, project string, input jevJobInput, automatic bool, changed map[string]bool) (jevJob, error) {
	empty := jevJob{}
	if input.Organization == nil || input.OperationID == "" || len(input.OperationID) > 128 || len(input.Sources) > 0 || len(input.Tags) > 0 || len(input.Destinations) > 0 {
		return empty, decision.ErrInvalid
	}
	request := *input.Organization
	if request.Mode != "preview" && request.Mode != "apply" {
		return empty, decision.ErrInvalid
	}
	role := "comment"
	if request.Mode == "apply" || automatic {
		role = "owner"
	}
	store, err := s.organizationNotebook(user, project, role)
	if err != nil {
		return empty, err
	}
	judge, _ := s.jevConfigured()
	if err = s.jevAuthorized(ctx, user, project, "organization", judge); err != nil {
		return empty, err
	}
	account, err := s.catalog.Account(user)
	if err != nil {
		return empty, err
	}
	raw, _ := json.Marshal(input)
	requestHash := digest(string(raw))
	id := digest(account.ID + "\x00" + project + "\x00" + input.OperationID)
	s.jevState.mu.Lock()
	prior, err := s.jevState.readJob(id)
	s.jevState.mu.Unlock()
	if err == nil {
		if prior.RequestHash != requestHash {
			return empty, workspace.ErrConflict
		}
		return prior, nil
	}
	if !errors.Is(err, chat.ErrNotFound) {
		return empty, err
	}
	select {
	case s.jevState.prepare <- struct{}{}:
	case <-ctx.Done():
		return empty, ctx.Err()
	}
	defer func() { <-s.jevState.prepare }()
	snapshot, err := store.SnapshotScoped(ctx, "", jevActiveScope())
	if err != nil {
		return empty, err
	}
	request.Policy, err = normalizeOrganizationPolicy(request.Policy, snapshot)
	if err != nil {
		return empty, err
	}
	if request.Scope != "all" && request.Scope != "folder" && request.Scope != "selected" {
		return empty, decision.ErrInvalid
	}
	if request.Scope == "selected" && len(request.Paths) == 0 || request.Scope != "selected" && len(request.Paths) > 0 || request.Scope != "folder" && request.Folder != "" {
		return empty, decision.ErrInvalid
	}
	if request.Scope == "folder" && !slices.Contains(organizationFolders(snapshot), request.Folder) {
		return empty, decision.ErrInvalid
	}
	available := map[string]bool{}
	for _, f := range snapshot.Files {
		available[f.Path] = true
	}
	for _, p := range request.Paths {
		if !available[p] {
			return empty, workspace.ErrForbidden
		}
	}
	job := jevJob{Version: 2, ID: id, RequestHash: requestHash, Project: project, Owner: user, AccountID: account.ID, Kind: "organize", State: "queued", Automatic: automatic, Inventory: jevInventory(snapshot), Model: judge.Model(), Notes: []jevJobNote{}, Work: []jevJobWork{}, CreatedAt: chat.Now(), UpdatedAt: chat.Now(), Organization: &organizationRun{Input: request, Paths: map[string]string{}, Allowed: []string{}}}
	job.Organization.ConsentGeneration, err = s.jevState.organizationConsentGeneration(user)
	if err != nil {
		return empty, err
	}
	job.Organization.AccessEpoch = s.catalog.Epoch(user)
	job.Organization.ProjectEpoch = s.catalog.ProjectAccessEpoch(user, project)
	selected := map[string]bool{}
	for _, f := range snapshot.Files {
		job.Notes = append(job.Notes, jevJobNote{Path: f.Path, Revision: snapshot.Revision, Hash: f.Hash, CurrentHash: f.Hash})
		inScope := request.Scope == "all" || request.Scope == "selected" && slices.Contains(request.Paths, f.Path) || request.Scope == "folder" && (request.Folder == "" || strings.HasPrefix(f.Path, request.Folder+"/"))
		if inScope {
			job.Organization.Allowed = append(job.Organization.Allowed, f.Path)
		}
		if inScope && (changed == nil || changed[f.Path]) {
			selected[f.Path] = true
			job.Organization.Targets = append(job.Organization.Targets, f.Path)
		}
	}
	if automatic {
		settings, err := s.loadJevNotebook(project)
		if err != nil || settings.Organization == nil {
			return empty, decision.ErrDisabled
		}
		job.Organization.Generation = settings.Organization.Generation
	}
	passages, err := knowledge.Split(snapshot, knowledge.Config{PassageRunes: 1024})
	if err != nil {
		return empty, err
	}
	candidates, err := knowledge.NewReviewCandidates(ctx, passages)
	if err != nil {
		return empty, err
	}
	pairs := map[string][]string{}
	if request.Policy.Links || request.Policy.Duplicates {
		for _, p := range passages {
			if !selected[p.Path] || len(pairs[p.Path]) >= 4 {
				continue
			}
			related, err := candidates.Related(ctx, p, 4, false)
			if err != nil {
				return empty, err
			}
			for _, other := range related {
				if other.Path != p.Path && !slices.Contains(pairs[p.Path], other.Path) && len(pairs[p.Path]) < 4 {
					pairs[p.Path] = append(pairs[p.Path], other.Path)
				}
			}
		}
	}
	add := func(kind, p, target string) {
		job.Work = append(job.Work, jevJobWork{Kind: "organize-" + kind, Organization: &organizationAction{Kind: kind, Path: p, Target: target}})
	}
	// Remove verified duplicates before additive edits, and relocate only after all
	// content decisions. Each subsequent judgment gets newly pinned full evidence.
	for _, f := range snapshot.Files {
		if selected[f.Path] && request.Policy.Duplicates {
			sort.Strings(pairs[f.Path])
			for _, p := range pairs[f.Path] {
				if p < f.Path {
					add("duplicate", f.Path, p)
				}
			}
		}
	}
	for _, f := range snapshot.Files {
		if !selected[f.Path] {
			continue
		}
		if request.Policy.Tags && len(request.Policy.Vocabulary) > 0 {
			batchSize := max(1, judge.QuestionLimit()-1)
			for start := 0; start < len(request.Policy.Vocabulary); start += batchSize {
				add("tags", f.Path, "")
				job.Work[len(job.Work)-1].Organization.Vocabulary = append([]string{}, request.Policy.Vocabulary[start:min(start+batchSize, len(request.Policy.Vocabulary))]...)
			}
		}
		if request.Policy.Links {
			for _, p := range pairs[f.Path] {
				add("links", f.Path, p)
			}
		}
	}
	if request.Policy.Moves {
		for _, f := range snapshot.Files {
			if selected[f.Path] {
				add("move", f.Path, "")
			}
		}
	}
	job.Total = len(job.Work)
	job.Remaining = job.Total
	job.Message = "Up to four retrieved related-note candidates per note; complete notes are verified before duplicate cleanup. This is not an exhaustive duplicate search."
	raw, _ = json.Marshal(job)
	if len(raw) > 3<<20 {
		return empty, errors.New("organization work exceeds durable capacity; choose a smaller folder or selection")
	}
	if err = s.checkOrganizationJob(ctx, job, judge); err != nil {
		return empty, err
	}
	if err = s.roomForJevJob(); err != nil {
		return empty, err
	}
	s.jevState.mu.Lock()
	defer s.jevState.mu.Unlock()
	job, err = s.jevState.insertJob(job, func() error {
		ids, err := s.jevState.jobIDs()
		if err != nil {
			return err
		}
		if len(ids) >= jevJobLimit {
			return decision.ErrLimit
		}
		if request.Mode != "apply" {
			return nil
		}
		// Serialize applying runs per notebook, including paused/unknown operations.
		applying, err := s.jevState.jobs(map[string]any{"project": project, "organization": map[string]any{"input": map[string]any{"mode": "apply"}}})
		if err != nil {
			return err
		}
		for _, prior := range applying {
			if !prior.Cancelled && prior.State != "completed" && prior.State != "failed" {
				return errors.New("finish or cancel the existing applying organization run first")
			}
		}
		return nil
	})
	if err != nil {
		return empty, err
	}
	return job, nil
}
func (s *Server) checkOrganizationJob(ctx context.Context, job jevJob, judge *decision.Service) error {
	if job.Organization == nil {
		return decision.ErrInvalid
	}
	if err := s.jevAuthorized(ctx, job.Owner, job.Project, "organization", judge); err != nil {
		return err
	}
	consent, err := s.jevState.organizationConsentGeneration(job.Owner)
	if err != nil {
		return err
	}
	if consent != job.Organization.ConsentGeneration || s.catalog.Epoch(job.Owner) != job.Organization.AccessEpoch {
		return workspace.ErrForbidden
	}
	if s.catalog.ProjectAccessEpoch(job.Owner, job.Project) != job.Organization.ProjectEpoch {
		return workspace.ErrForbidden
	}
	if judge.Model() != job.Model {
		return decision.ErrDisabled
	}
	role := "comment"
	if job.Organization.Input.Mode == "apply" || job.Automatic {
		role = "owner"
	}
	if _, err := s.organizationNotebook(job.Owner, job.Project, role); err != nil {
		return err
	}
	account, err := s.catalog.Account(job.Owner)
	if err != nil || account.ID != job.AccountID {
		return workspace.ErrForbidden
	}
	if job.Automatic {
		settings, err := s.loadJevNotebook(job.Project)
		if err != nil {
			return err
		}
		o := settings.Organization
		if o == nil || !o.Enabled || o.Generation != job.Organization.Generation || o.Owner != job.Owner || o.AccountID != job.AccountID {
			return decision.ErrDisabled
		}
	}
	s.jevState.mu.Lock()
	current, err := s.jevState.readJob(job.ID)
	s.jevState.mu.Unlock()
	if err == nil && current.Cancelled {
		return context.Canceled
	}
	if err != nil && !errors.Is(err, chat.ErrNotFound) {
		return err
	}
	return nil
}
func organizationExplanation(kind, status string) string {
	return fmt.Sprintf("%s: %s. Only the recorded policy and current guarded evidence authorize execution.", kind, status)
}

// The labels of an applied move's organizationTrashSummary.
const (
	organizationTrashUpdated = "Links updated in notes in the Trash"
	organizationTrashLeft    = "Notes in the Trash left as they were"
)

// organizationTrashSummary names, for an action's reason, the notes in the
// Trash whose links a move or its undo changed (labelled changed) and those
// it left as they were (labelled left), with why. It names at most ten of
// each; the result's trashLinks has them all.
func organizationTrashSummary(links []workspace.OrganizationTrashLink, changed, left string) string {
	var done, kept []string
	for _, link := range links {
		name := link.Path
		if link.OriginalPath != "" {
			name += " (from " + link.OriginalPath + ")"
		}
		if link.Updated {
			done = append(done, name)
		} else {
			kept = append(kept, name+": "+link.Reason)
		}
	}
	list := func(label string, names []string, separator string) string {
		if len(names) == 0 {
			return ""
		}
		more := ""
		if len(names) > 10 {
			more = fmt.Sprintf("%sand %d more", separator, len(names)-10)
			names = names[:10]
		}
		return " " + label + ": " + strings.Join(names, separator) + more + "."
	}
	return list(changed, done, ", ") + list(left, kept, "; ")
}

// organizationPaths are the paths an action's result changed: its files and
// the notes in the Trash whose links it updated.
func organizationPaths(result *workspace.OrganizationResult) []string {
	var paths []string
	for _, file := range result.Files {
		paths = append(paths, file.Path)
	}
	for _, link := range result.TrashLinks {
		if link.Updated {
			paths = append(paths, link.Path)
		}
	}
	return paths
}
