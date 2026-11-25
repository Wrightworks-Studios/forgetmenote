package web

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/decision"
	"os"
	"reflect"
	"time"
)

func (s *Server) scanOrganizationAutomatic(ctx context.Context, now time.Time) {
	judge, localOnly := s.jevConfigured()
	if judge == nil || localOnly || ctx.Err() != nil {
		return
	}
	for _, p := range s.catalog.List(catalog.Bootstrap) {
		if p.ProtectedInbox != "" {
			continue
		}
		s.scanOrganizationNotebook(ctx, p.ID, now)
	}
}
func (s *Server) scanOrganizationNotebook(ctx context.Context, project string, now time.Time) {
	s.jevState.organizationMu.Lock()
	defer s.jevState.organizationMu.Unlock()
	settings, err := s.loadJevNotebook(project)
	if err != nil || settings.Organization == nil || !settings.Organization.Enabled {
		return
	}
	o := settings.Organization
	consent, err := s.jevState.organizationConsentGeneration(o.Owner)
	if err != nil || consent != o.ConsentGeneration || s.catalog.Epoch(o.Owner) != o.AccessEpoch {
		return
	}
	if s.catalog.ProjectAccessEpoch(o.Owner, project) != o.ProjectEpoch {
		return
	}
	judge, _ := s.jevConfigured()
	if err = s.jevAuthorized(ctx, o.Owner, project, "organization", judge); err != nil {
		return
	}
	account, err := s.catalog.Account(o.Owner)
	if err != nil || account.ID != o.AccountID {
		return
	}
	store, err := s.organizationNotebook(o.Owner, project, "owner")
	if err != nil {
		return
	}
	snapshot, err := store.SnapshotScoped(ctx, "", jevActiveScope())
	if err != nil {
		return
	}
	current := map[string]string{}
	for _, file := range snapshot.Files {
		current[file.Path] = file.Hash
	}
	if reflect.DeepEqual(current, o.Baseline) {
		return
	}
	// Save only what this scan changed, and only while the settings it read
	// stay enabled at their generation: a change through another gateway wins.
	save := func(settled bool) {
		s.updateOrganization(project, func(stored *organizationSettings) error {
			if !stored.Enabled || stored.Generation != o.Generation {
				return decision.ErrDisabled
			}
			if settled {
				stored.Baseline = o.Baseline
			}
			stored.Observed, stored.ObservedAt = o.Observed, o.ObservedAt
			return nil
		})
	}
	if !reflect.DeepEqual(current, o.Observed) {
		o.Observed = current
		o.ObservedAt = now.UTC().Format(time.RFC3339Nano)
		save(false)
		return
	}
	observed, err := time.Parse(time.RFC3339Nano, o.ObservedAt)
	if err != nil || now.Sub(observed) < 2*time.Minute {
		return
	}
	changed := map[string]bool{}
	for p, h := range current {
		if o.Baseline[p] != h {
			changed[p] = true
		}
	}
	if len(changed) == 0 {
		o.Baseline = current
		o.Observed = nil
		o.ObservedAt = ""
		save(true)
		return
	}
	// The snapshot and generation make scheduling idempotent even if saving the
	// observation fails. A committed organizer action advances only its own paths.
	key, _ := json.Marshal([]any{o.Generation, current})
	input := jevJobInput{Kind: "organize", OperationID: "automatic-organize-" + digest(string(key)), Organization: &organizationInput{Mode: "apply", Scope: "all", Policy: o.Policy}}
	job, err := s.createOrganizationJob(ctx, o.Owner, project, input, true, changed)
	if err != nil {
		return
	}
	if job.State == "completed" {
		o.Baseline = current
		o.Observed = nil
		o.ObservedAt = ""
		save(true)
	} else if job.State == "queued" {
		s.launchJevJob(job.ID, false)
	}
}

// These settings only suppress organizer actions. Consistency and Quicknotes
// retain their separate opt-ins, baselines, destination policies and workers.

// Consume exactly the pinned target evidence, including non-mutating outcomes.
// Refreshing from HEAD here would hide user edits made while the run was active.
// Callers hold organizationMu through this checkpoint and terminal job state.
func (s *Server) recordOrganizationCompletion(job jevJob) error {
	if !job.Automatic {
		return nil
	}
	s.jevState.mu.Lock()
	defer s.jevState.mu.Unlock()
	_, err := s.jevState.updateNotebook(job.Project, func(settings *jevNotebookSettings, found bool) error {
		if !found {
			return os.ErrNotExist
		}
		o := settings.Organization
		if o == nil || !o.Enabled || o.Generation != job.Organization.Generation {
			return nil
		}
		if o.Baseline == nil {
			o.Baseline = map[string]string{}
		}
		for _, original := range job.Organization.Targets {
			current := organizationCurrentPath(job, original)
			if current != original {
				delete(o.Baseline, original)
			}
			for _, note := range job.Notes {
				if note.Path == current {
					o.Baseline[current] = note.Hash
					break
				}
			}
		}
		return nil
	})
	return err
}
