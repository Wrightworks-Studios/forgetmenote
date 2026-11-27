package judgments

import (
	"forgetmenote/internal/decision"
	"maps"
	"strings"
	"time"
)

// judgmentJobLimit bounds the jobs stored, and so what one listing reads: a new
// job is admitted while fewer are stored.
const judgmentJobLimit = 2000

func judgmentJobTime(job judgmentJob) time.Time {
	at, _ := time.Parse(time.RFC3339Nano, job.UpdatedAt)
	return at
}

// scanJobs reads the stored jobs whose bodies contain fields in one query,
// without judgmentState.mu. It skips and counts records that are not valid jobs,
// and reports more when over judgmentJobLimit match, returning the first
// judgmentJobLimit by name.
func (j *judgmentState) scanJobs(fields map[string]any) (jobs []judgmentJob, invalid int, more bool, err error) {
	bodies, err := j.records.ListMatching("jobs/", fields, judgmentJobLimit)
	if err != nil {
		return nil, 0, false, err
	}
	if len(bodies) > judgmentJobLimit {
		bodies, more = bodies[:judgmentJobLimit], true
	}
	jobs = make([]judgmentJob, 0, len(bodies))
	for _, body := range bodies {
		id, ok := strings.CutSuffix(strings.TrimPrefix(body.Name, "jobs/"), ".json")
		if !ok || strings.Contains(id, "/") {
			continue
		}
		var job judgmentJob
		if err := body.Decode(&job); err != nil || validJudgmentJob(id, job) != nil {
			invalid++
			continue
		}
		jobs = append(jobs, job)
	}
	return jobs, invalid, more, nil
}

// jobs is scanJobs for a caller that must see every matching job.
func (j *judgmentState) jobs(fields map[string]any) ([]judgmentJob, error) {
	jobs, invalid, more, err := j.scanJobs(fields)
	switch {
	case err != nil:
		return nil, err
	case invalid > 0:
		return nil, decision.ErrStorage
	case more:
		return nil, decision.ErrLimit
	}
	return jobs, nil
}

// running returns the jobs a runner on this gateway holds.
func (j *judgmentState) running() map[string]bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	ids := map[string]bool{}
	for key := range j.active {
		if id, ok := strings.CutPrefix(key, "job:"); ok {
			ids[id] = true
		}
	}
	return ids
}

// jobsAndRunners lists jobs as jobs does, with the ones a runner on this
// gateway held before or after the read, so a job that starts or finishes
// meanwhile is not shown as interrupted.
func (j *judgmentState) jobsAndRunners(fields map[string]any) ([]judgmentJob, map[string]bool, error) {
	active := j.running()
	jobs, err := j.jobs(fields)
	maps.Copy(active, j.running())
	return jobs, active, err
}

// readJobAndRunner is jobsAndRunners for one job.
func (j *judgmentState) readJobAndRunner(id string) (judgmentJob, bool, error) {
	before := j.running()[id]
	job, err := j.readJob(id)
	return job, before || j.running()[id], err
}
