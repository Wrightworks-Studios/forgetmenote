package web

import (
	"context"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"
	"reflect"
	"time"
)

func (s *Server) startJevScheduler(ctx context.Context) {
	s.jevState.mu.Lock()
	if s.jevState.closed {
		s.jevState.mu.Unlock()
		return
	}
	s.jevState.wg.Add(1)
	s.jevState.mu.Unlock()
	scheduler, stop := context.WithCancel(ctx)
	go func() {
		<-s.jevState.ctx.Done()
		stop()
	}()
	// One gateway launches queued and automatic reviews, and applies job
	// retention once a lease is held and hourly after.
	go func() {
		defer s.jevState.wg.Done()
		defer stop()
		s.db.Duty(scheduler, "jev-scheduler", time.Minute, func(ctx context.Context, _ *pg.Lease) {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			var pruned time.Time
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-ticker.C:
					s.syncInstanceAI(s.jevState.ctx)
					if now.Sub(pruned) >= jevPruneEvery {
						s.maintainJevJobs(now)
						pruned = now
					}
					s.launchNextJevJob()
					s.scanJevAutomatic(ctx, now)
				}
			}
		})
	}()
}

// Only current committed active Notes participate. Finding commits do not alter
// the inventory, and enabling does not launch an initial broad review. The
// instance policy reviews only ordinary notebooks: memory, for one, rewrites
// its whole file on every remember and reviews each new item itself.
func (s *Server) scanJevAutomatic(ctx context.Context, now time.Time) {
	s.scanOrganizationAutomatic(ctx, now)
	judge, localOnly := s.jevConfigured()
	if judge == nil || localOnly || ctx.Err() != nil {
		return
	}
	for _, project := range s.catalog.List(catalog.Bootstrap) {
		if policy, managed := s.catalog.AISettings(); managed {
			if !policy.Reviews || !project.Ordinary() {
				continue
			}
			if err := s.prepareInstanceReview(ctx, project.ID); err != nil {
				continue
			}
		}
		settings, err := s.loadJevNotebook(project.ID)
		if err != nil || !settings.AutoChecks {
			continue
		}
		account, err := s.catalog.Account(settings.EnabledBy)
		if err != nil || account.ID != settings.AccountID {
			continue
		}
		if s.inboxAIAllowed(settings.EnabledBy, project) != nil {
			continue
		}
		if err = s.jevAuthorized(ctx, settings.EnabledBy, project.ID, "consistency", judge); err != nil {
			continue
		}
		store, err := s.catalog.Access(settings.EnabledBy, project.ID, "owner")
		if err != nil {
			continue
		}
		snapshot, err := store.SnapshotScoped(ctx, "", jevActiveScope())
		if err != nil {
			continue
		}
		current := map[string]string{}
		for _, file := range snapshot.Files {
			current[file.Path] = file.Hash
		}
		if reflect.DeepEqual(current, settings.Baseline) {
			continue
		}
		if !reflect.DeepEqual(current, settings.Observed) {
			settings.Observed = current
			settings.ObservedAt = now.UTC().Format(time.RFC3339Nano)
			s.saveJevAutoObservation(settings)
			continue
		}
		changedAt, err := time.Parse(time.RFC3339Nano, settings.ObservedAt)
		if err != nil || now.Sub(changedAt) < 2*time.Minute {
			continue
		}
		changed := map[string]bool{}
		for path, hash := range current {
			if settings.Baseline[path] != hash {
				changed[path] = true
			}
		}
		known, fresh, err := jevPassageChanges(ctx, store, snapshot, settings, changed)
		if err != nil {
			known, fresh = nil, true // Check the changed notes whole.
		}
		if len(changed) == 0 || !fresh {
			settings.Baseline, settings.BaselineRevision = current, snapshot.Revision
			settings.Observed = nil
			settings.ObservedAt = ""
			s.saveJevAutoObservation(settings)
			continue
		}
		input := jevJobInput{Kind: "consistency", OperationID: "automatic-" + jevInventory(snapshot)}
		job, err := s.createJevJob(ctx, settings.EnabledBy, project.ID, input, true, changed, known)
		if err != nil {
			continue
		}
		if job.State == "completed" {
			settings.Baseline, settings.BaselineRevision = current, snapshot.Revision
			settings.Observed = nil
			settings.ObservedAt = ""
			s.saveJevAutoObservation(settings)
		} else if job.State == "queued" || job.State == "running" {
			s.launchJevJob(job.ID, false)
		}
	}
}

func (s *Server) saveJevAutoObservation(settings jevNotebookSettings) error {
	s.jevState.mu.Lock()
	defer s.jevState.mu.Unlock()
	_, err := s.jevState.updateNotebook(settings.Notebook, func(current *jevNotebookSettings, _ bool) error {
		if !current.AutoChecks || current.AccountID != settings.AccountID || current.EnabledBy != settings.EnabledBy {
			return decision.ErrDisabled
		}
		current.Baseline, current.BaselineRevision = settings.Baseline, settings.BaselineRevision
		current.Observed = settings.Observed
		current.ObservedAt = settings.ObservedAt
		return nil
	})
	return err
}

// jevPassageChanges compares the changed notes with what they, and any note
// removed since, held at the baseline revision. known holds the text hashes of
// the passages an earlier review covered, and fresh reports whether any
// current passage of a changed note is new, so an edit is checked by its new
// passages alone and a moved note by none. A baseline from before revisions
// were recorded knows nothing, so its changed notes are checked whole.
func jevPassageChanges(ctx context.Context, store *workspace.Store, snapshot workspace.Snapshot, settings jevNotebookSettings, changed map[string]bool) (known map[string]bool, fresh bool, err error) {
	known = map[string]bool{}
	if settings.BaselineRevision != "" {
		current := map[string]bool{}
		for _, file := range snapshot.Files {
			current[file.Path] = true
		}
		for path := range settings.Baseline {
			if current[path] && !changed[path] {
				continue
			}
			doc, err := store.KnowledgeRead(ctx, path, settings.BaselineRevision)
			if err != nil {
				return nil, true, err
			}
			metadata, err := workspace.ParseMetadata(doc.Content)
			if err != nil {
				return nil, true, err
			}
			before, err := knowledge.Split(workspace.Snapshot{Revision: settings.BaselineRevision, Files: []workspace.SnapshotFile{{Path: path, Content: string(doc.Content), Hash: settings.Baseline[path], Metadata: metadata}}}, knowledge.Config{PassageRunes: jevPassageRunes})
			if err != nil {
				return nil, true, err
			}
			for _, passage := range before {
				known[passage.Hash] = true
			}
		}
	}
	now := workspace.Snapshot{Revision: snapshot.Revision}
	for _, file := range snapshot.Files {
		if changed[file.Path] {
			now.Files = append(now.Files, file)
		}
	}
	passages, err := knowledge.Split(now, knowledge.Config{PassageRunes: jevPassageRunes})
	if err != nil {
		return nil, true, err
	}
	for _, passage := range passages {
		if !known[passage.Hash] {
			return known, true, nil
		}
	}
	return known, false, nil
}
