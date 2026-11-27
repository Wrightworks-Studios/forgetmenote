package jev

import (
	"forgetmenote/internal/decision"
	"maps"
	"strings"
	"time"
)

// jevJobLimit bounds the jobs stored, and so what one listing reads: a new
// job is admitted while fewer are stored.
const jevJobLimit = 2000

func jevJobTime(job jevJob) time.Time {
	at, _ := time.Parse(time.RFC3339Nano, job.UpdatedAt)
	return at
}

// scanJobs reads the stored jobs whose bodies contain fields in one query,
// without jevState.mu. It skips and counts records that are not valid jobs,
// and reports more when over jevJobLimit match, returning the first
// jevJobLimit by name.
func (j *jevState) scanJobs(fields map[string]any) (jobs []jevJob, invalid int, more bool, err error) {
	bodies, err := j.records.ListMatching("jobs/", fields, jevJobLimit)
	if err != nil {
		return nil, 0, false, err
	}
	if len(bodies) > jevJobLimit {
		bodies, more = bodies[:jevJobLimit], true
	}
	jobs = make([]jevJob, 0, len(bodies))
	for _, body := range bodies {
		id, ok := strings.CutSuffix(strings.TrimPrefix(body.Name, "jobs/"), ".json")
		if !ok || strings.Contains(id, "/") {
			continue
		}
		var job jevJob
		if err := body.Decode(&job); err != nil || validJevJob(id, job) != nil {
			invalid++
			continue
		}
		jobs = append(jobs, job)
	}
	return jobs, invalid, more, nil
}

// jobs is scanJobs for a caller that must see every matching job.
func (j *jevState) jobs(fields map[string]any) ([]jevJob, error) {
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
func (j *jevState) running() map[string]bool {
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
func (j *jevState) jobsAndRunners(fields map[string]any) ([]jevJob, map[string]bool, error) {
	active := j.running()
	jobs, err := j.jobs(fields)
	maps.Copy(active, j.running())
	return jobs, active, err
}

// readJobAndRunner is jobsAndRunners for one job.
func (j *jevState) readJobAndRunner(id string) (jevJob, bool, error) {
	before := j.running()[id]
	job, err := j.readJob(id)
	return job, before || j.running()[id], err
}
