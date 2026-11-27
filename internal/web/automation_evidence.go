package web

import (
	"context"
	"errors"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/chatrun"
	"forgetmenote/internal/workspace"
)

// Persist only citation metadata with a job, then use the same current-source
// checks as browser chat before another provider call, publication or delivery.
func (s *Server) checkAutomationEvidence(ctx context.Context, r *automation.Run, upstream bool) error {
	if upstream && r.Spec.Kind == "assistant" && r.ChatGuidanceHash != "" {
		_, hash, err := s.chatrun.AssistantInstructions(ctx, r.Spec)
		if err != nil {
			return err
		}
		if hash != r.ChatGuidanceHash {
			return workspace.ErrConflict
		}
	}
	if upstream && r.Spec.Kind == "assistant" && r.Spec.Delivery != nil {
		if err := s.messaging.VerifyMessageMember(ctx, *r.Spec.Delivery); err != nil {
			return err
		}
	}
	if r.Spec.Repair != nil && r.GenerationStarted {
		if policy, managed := s.catalog.AISettings(); managed && !policy.Writing {
			return errInstanceAI
		}
	}
	if err := s.checkAutomationAuth(ctx); err != nil {
		return err
	}
	if r.Batch != nil {
		if !time.Now().Before(r.Batch.Expires) {
			return errors.New("automation event evidence expired")
		}
		for _, event := range r.Batch.Events {
			if event.Actor != "" {
				if !s.catalog.ValidSession(event.Actor, event.Epoch) {
					return workspace.ErrForbidden
				}
				if _, err := s.catalog.Access(event.Actor, r.Spec.Project, "read"); err != nil {
					return err
				}
			}
			if event.Project != "" {
				if _, err := s.clientAccess(ctx, r.Spec.Actor, event.Project, "read"); err != nil {
					return err
				}
				repos, err := s.repos()
				if err != nil {
					return err
				}
				state, err := repos.Status(event.Project)
				if err != nil {
					return err
				}
				if !state.Connected || state.Paused || state.Downloaded != event.Revision {
					return errors.New("repository event was superseded")
				}
			}
		}
	}
	if r.Spec.Kind == "learning" {
		if err := s.checkLearningEvidence(ctx, r); err != nil {
			return err
		}
	}
	for _, receipt := range r.Receipts {
		if !s.dataAllowed(ctx, receipt.Connection, receipt.Operation) {
			return workspace.ErrForbidden
		}
	}
	if !upstream {
		// Automation streams are buffered. Recheck authorization on each event,
		// but reread pinned content at model-call and publication boundaries,
		// as for non-streaming providers, rather than running Git per fragment.
		for _, project := range r.Spec.ReadProjects {
			if _, err := s.clientAccess(ctx, r.Spec.Actor, project, "read"); err != nil {
				return err
			}
		}
		return s.checkDataCitations(ctx, r.Citations)
	}
	if scope := chat.RunScopeFrom(ctx); scope != nil {
		if r.Spec.Kind == "recipe" || r.Spec.Repair != nil && r.Spec.Recipe != "" {
			store, err := s.clientAccess(ctx, r.Spec.Actor, r.Spec.Project, "read")
			if err != nil {
				return err
			}
			if _, err = chatrun.ReadNote(ctx, store, r.Spec.Recipe, ""); err != nil {
				return err
			}
		}
		run := scope.Run
		run.Citations = r.Citations
		return s.chatrun.ValidateSources(ctx, run, upstream)
	}
	return nil
}

func (s *Server) checkLearningEvidence(ctx context.Context, r *automation.Run) error {
	if r.Learning == nil || !time.Now().Before(r.Learning.Expires) {
		return errors.New("learning evidence expired or unavailable")
	}
	for _, e := range r.Learning.Events {
		if time.Since(e.At) > time.Duration(r.Spec.Learning.RetentionHours)*time.Hour {
			return errors.New("learning evidence expired")
		}
		if err := s.learningEventAllowed(ctx, r, e); err != nil {
			return err
		}
	}
	if len(r.Learning.Sources) > 0 {
		store, err := s.clientAccess(ctx, r.Spec.Actor, r.Spec.Project, "read")
		if err != nil {
			return err
		}
		snapshot, err := chatrun.ScopedSnapshot(ctx, store, "")
		if err != nil {
			return err
		}
		eligible := map[string]bool{}
		for _, file := range snapshot.Files {
			eligible[file.Path] = true
		}
		for _, source := range r.Learning.Sources {
			if !eligible[source] {
				return chat.ErrOutsideCorpus
			}
		}
	}
	return nil
}
