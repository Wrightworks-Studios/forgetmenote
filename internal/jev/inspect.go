package jev

import (
	"context"
	"time"

	"forgetmenote/internal/decision"
)

// These methods let tests in other packages arrange and inspect Jev's state.
// Each takes the lock the runner takes.

// Records are Jev's durable records.
func (s *Service) Records() *decision.Records { return s.jevState.records }

// JobIDs lists the stored jobs.
func (s *Service) JobIDs() ([]string, error) {
	s.jevState.mu.Lock()
	defer s.jevState.mu.Unlock()
	return s.jevState.jobIDs()
}

// ReadJob returns a stored job as the job routes show it.
func (s *Service) ReadJob(id string) (map[string]any, error) {
	s.jevState.mu.Lock()
	job, err := s.jevState.readJob(id)
	s.jevState.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return publicJevJob(job), nil
}

// ScanAutomatic runs one pass of automatic review and organization, as the
// scheduler does at now.
func (s *Service) ScanAutomatic(ctx context.Context, now time.Time) { s.scanJevAutomatic(ctx, now) }

// CreateJob prepares a job without launching it and returns its ID.
func (s *Service) CreateJob(ctx context.Context, user, project string, input JobInput, automatic bool, changed, known map[string]bool) (string, error) {
	job, err := s.createJevJob(ctx, user, project, input, automatic, changed, known)
	return job.ID, err
}

// CheckJob runs the check a stored job passes before each run and Continue.
func (s *Service) CheckJob(ctx context.Context, id string) error {
	s.jevState.mu.Lock()
	job, err := s.jevState.readJob(id)
	s.jevState.mu.Unlock()
	if err != nil {
		return err
	}
	judge, _ := s.hooks.Judge()
	return s.checkJevJob(ctx, job, judge)
}
