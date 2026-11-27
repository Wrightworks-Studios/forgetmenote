package judgments

import (
	"errors"
	"forgetmenote/internal/decision"
	"log/slog"
	"slices"
	"strings"
	"time"
)

// Judgments keep every job that can still run and a bounded history of finished
// ones: retention removes a finished job, with its results, once it is
// judgmentJobRetention old or judgmentKeptJobs newer finished jobs exist.
const (
	judgmentKeptJobs     = 500
	judgmentJobRetention = 30 * 24 * time.Hour
	judgmentPruneEvery   = time.Hour
)

// pruneJudgmentJobs applies retention and returns how many jobs it removed. A
// finished job never runs again, so deciding from a read without its record
// held removes nothing another gateway still needs. Seeing only some finished
// jobs, when more are stored than one listing reads, still removes only jobs
// the complete rule would: each has judgmentKeptJobs newer ones or is too old.
func (s *Service) pruneJudgmentJobs(now time.Time) (int, error) {
	cutoff := now.Add(-judgmentJobRetention)
	removed := 0
	for {
		finished, more := []judgmentJob{}, false
		for _, state := range []string{"completed", "failed", "cancelled"} {
			jobs, _, overflow, err := s.judgmentState.scanJobs(map[string]any{"state": state})
			if err != nil {
				return removed, err
			}
			finished, more = append(finished, jobs...), more || overflow
		}
		slices.SortFunc(finished, func(a, b judgmentJob) int {
			if c := judgmentJobTime(b).Compare(judgmentJobTime(a)); c != 0 {
				return c
			}
			return strings.Compare(a.ID, b.ID)
		})
		pass := 0
		for i, job := range finished {
			if i < judgmentKeptJobs && !judgmentJobTime(job).Before(cutoff) {
				continue
			}
			if err := s.judgmentState.records.Remove(jobName(job.ID), jobResultsPrefix(job.ID)); err != nil {
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

// maintainJudgmentJobs runs retention for the scheduler and logs its outcome.
func (s *Service) maintainJudgmentJobs(now time.Time) {
	removed, err := s.pruneJudgmentJobs(now)
	if err != nil {
		slog.Warn("judgment job retention stopped", "removed", removed, "error", err)
	} else if removed > 0 {
		slog.Info("judgment job retention", "removed", removed)
	}
}

// roomForJudgmentJob applies retention when judgmentJobLimit jobs are stored, so only
// jobs that can still run refuse a new one; the insert checks the count again.
func (s *Service) roomForJudgmentJob() error {
	ids, err := s.judgmentState.jobIDs()
	if err == nil && len(ids) < judgmentJobLimit {
		return nil
	}
	if err != nil && !errors.Is(err, decision.ErrLimit) {
		return err
	}
	if _, err = s.pruneJudgmentJobs(time.Now()); err != nil {
		slog.Warn("judgment job retention stopped", "error", err)
	}
	return nil
}
