package judgments

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/auth"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
)

// Reconstruct only an already committed action. Reading activity can repair its
// sidecar after a crash even when the original grant was revoked; it never calls
// a provider or writes notebook content. Inference/mutation still require a grant.
func (s *Service) reconcileOrganizationReceipt(ctx context.Context, store *workspace.Store, id string) (judgmentJob, error) {
	s.judgmentState.organizationMu.Lock()
	defer s.judgmentState.organizationMu.Unlock()
	s.judgmentState.mu.Lock()
	job, err := s.judgmentState.readJob(id)
	if err != nil || s.judgmentState.active["job:"+id] != nil || job.Organization == nil || job.Organization.Input.Mode != "apply" || job.Cursor >= len(job.Work) {
		s.judgmentState.mu.Unlock()
		return job, err
	}
	a := job.Work[job.Cursor].Organization
	if a == nil || a.Request == nil {
		s.judgmentState.mu.Unlock()
		return job, nil
	}
	read, _ := json.Marshal(job)
	// Reserve this checkpoint against the scheduler while reconstructing metadata.
	s.judgmentState.active["job:"+id] = func() {}
	s.judgmentState.mu.Unlock()
	defer func() {
		s.judgmentState.mu.Lock()
		delete(s.judgmentState.active, "job:"+id)
		s.judgmentState.mu.Unlock()
	}()
	result, err := store.OrganizationReceipt(ctx, *a.Request, job.AccountID)
	if err != nil || !result.Replayed {
		return job, err
	}
	a.Status, a.Reason, a.Result = "applied", "Recovered the committed action from its Git receipt."+organizationTrashSummary(result.TrashLinks, organizationTrashUpdated, organizationTrashLeft), &result
	if err = s.advanceOrganization(ctx, &job, a); err != nil {
		return job, err
	}
	record := judgmentJobResult{ID: auth.Digest(job.ID + fmt.Sprint(job.Cursor)), Kind: job.Work[job.Cursor].Kind, Title: a.Kind + ": " + a.Path, Summary: a.Reason, Status: a.Status, Evidence: organizationEvidenceForAction(a), Details: organizationDetails(*a)}
	if err = s.saveOrganizationResult(job, job.Cursor, nil, record); err != nil {
		return job, err
	}
	job.Work[job.Cursor].Organization = &organizationAction{Kind: a.Kind, Path: a.Path, Target: a.Target, Key: a.Key, Status: a.Status}
	job.Work[job.Cursor].Verdict = a.Status
	job.Cursor++
	job.Examined = job.Cursor
	job.Remaining = job.Total - job.Cursor
	job.UpdatedAt = chat.Now()
	if !job.Cancelled && job.Cursor == job.Total {
		if err := s.recordOrganizationCompletion(job); err != nil {
			return job, err
		}
		job.State = "completed"
		job.Message = "Recovered committed organization activity."
	}
	s.judgmentState.mu.Lock()
	_, err = s.judgmentState.updateJob(job.ID, func(current *judgmentJob) error {
		// A change through another gateway, such as a cancellation, wins; the
		// next reconciliation reads the receipt again.
		if stored, _ := json.Marshal(current); !bytes.Equal(stored, read) {
			return workspace.ErrConflict
		}
		*current = job
		return nil
	})
	s.judgmentState.mu.Unlock()
	return job, err
}
