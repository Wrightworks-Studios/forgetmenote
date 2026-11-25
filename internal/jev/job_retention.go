package jev

import (
	"errors"
	"forgetmenote/internal/decision"
	"log/slog"
	"slices"
	"strings"
	"time"
)

// Jev keeps every job that can still run and a bounded history of finished
// ones: retention removes a finished job, with its results, once it is
// jevJobRetention old or jevKeptJobs newer finished jobs exist.
const (
	jevKeptJobs     = 500
	jevJobRetention = 30 * 24 * time.Hour
	jevPruneEvery   = time.Hour
)

// pruneJevJobs applies retention and returns how many jobs it removed. A
// finished job never runs again, so deciding from a read without its record
// held removes nothing another gateway still needs. Seeing only some finished
// jobs, when more are stored than one listing reads, still removes only jobs
// the complete rule would: each has jevKeptJobs newer ones or is too old.
func (s *Service) pruneJevJobs(now time.Time) (int, error) {
	cutoff := now.Add(-jevJobRetention)
	removed := 0
	for {
		finished, more := []jevJob{}, false
		for _, state := range []string{"completed", "failed", "cancelled"} {
			jobs, _, overflow, err := s.jevState.scanJobs(map[string]any{"state": state})
			if err != nil {
				return removed, err
			}
			finished, more = append(finished, jobs...), more || overflow
		}
		slices.SortFunc(finished, func(a, b jevJob) int {
			if c := jevJobTime(b).Compare(jevJobTime(a)); c != 0 {
				return c
			}
			return strings.Compare(a.ID, b.ID)
		})
		pass := 0
		for i, job := range finished {
			if i < jevKeptJobs && !jevJobTime(job).Before(cutoff) {
				continue
			}
			if err := s.jevState.records.Remove(jobName(job.ID), jobResultsPrefix(job.ID)); err != nil {
				return removed, err
			}
			removed++
			pass++
		}
		if !more || pass == 0 {
			return removed, nil
		}
	}
}

// maintainJevJobs runs retention for the scheduler and logs its outcome.
func (s *Service) maintainJevJobs(now time.Time) {
	removed, err := s.pruneJevJobs(now)
	if err != nil {
		slog.Warn("jev job retention stopped", "removed", removed, "error", err)
	} else if removed > 0 {
		slog.Info("jev job retention", "removed", removed)
	}
}

// roomForJevJob applies retention when jevJobLimit jobs are stored, so only
// jobs that can still run refuse a new one; the insert checks the count again.
func (s *Service) roomForJevJob() error {
	ids, err := s.jevState.jobIDs()
	if err == nil && len(ids) < jevJobLimit {
		return nil
	}
	if err != nil && !errors.Is(err, decision.ErrLimit) {
		return err
	}
	if _, err = s.pruneJevJobs(time.Now()); err != nil {
		slog.Warn("jev job retention stopped", "error", err)
	}
	return nil
}
