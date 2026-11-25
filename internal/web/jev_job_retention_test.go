package web

import (
	"encoding/json"
	"testing"
	"time"
)

// Retention removes finished jobs older than 30 days and all but the newest
// 500 finished ones, with their results, and keeps every job that can still
// run however old it is.
func TestJevRetentionKeepsLiveJobsAndRecentFinishedHistory(t *testing.T) {
	app := chatFixture(t)
	now := time.Now()
	ago := func(d time.Duration) func(int) time.Time { return func(int) time.Time { return now.Add(-d) } }
	recent := storeJevJobs(t, app, 600, syntheticJevJob("completed"), func(i int) time.Time { return now.Add(-time.Duration(i+1) * time.Minute) })
	var old, live []string
	for _, state := range []string{"completed", "failed", "cancelled"} {
		old = append(old, storeJevJobs(t, app, 1, syntheticJevJob(state), ago(31*24*time.Hour))...)
	}
	for _, state := range []string{"queued", "running", "paused"} {
		live = append(live, storeJevJobs(t, app, 1, syntheticJevJob(state), ago(90*24*time.Hour))...)
	}
	for _, id := range []string{old[0], recent[0], live[0]} {
		if err := app.jevState.records.Write(resultName(id, 0), map[string]string{"job": id}); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := app.pruneJevJobs(now)
	if err != nil || removed != 100+len(old) {
		t.Fatalf("retention removed %d jobs, not the 100 surplus and %d old ones: %v", removed, len(old), err)
	}
	stored := storedJevNames(t, app)
	for i, id := range recent {
		if stored[jobName(id)] != (i < 500) {
			t.Fatalf("finished job %d of 600 by age kept=%v", i, stored[jobName(id)])
		}
	}
	for _, id := range old {
		if stored[jobName(id)] {
			t.Fatal("a finished job older than 30 days was kept")
		}
	}
	for _, id := range live {
		if !stored[jobName(id)] {
			t.Fatal("retention removed a job that can still run")
		}
	}
	if stored[resultName(old[0], 0)] || !stored[resultName(recent[0], 0)] || !stored[resultName(live[0], 0)] {
		t.Fatal("results did not follow their jobs", stored[resultName(old[0], 0)], stored[resultName(recent[0], 0)], stored[resultName(live[0], 0)])
	}
	if removed, err = app.pruneJevJobs(now); err != nil || removed != 0 {
		t.Fatalf("a second pass removed %d: %v", removed, err)
	}
}

// With more finished jobs stored than the job limit, retention makes room
// for a new job, and the notebook's newest jobs list again.
func TestJevJobAdmittedPastTheJobLimitByRetention(t *testing.T) {
	app := reviewFixture(t, syntheticReviewAnswer)
	now := time.Now()
	storeJevJobs(t, app, jevJobLimit+1, syntheticJevJob("completed"), func(i int) time.Time { return now.Add(-time.Duration(i+1) * time.Second) })
	next := startReview(t, app, `{"kind":"consistency","operationId":"after-history"}`)
	if job := waitReview(t, app, next); job["state"] != "completed" {
		t.Fatal(job["state"], job["message"])
	}
	w := proxyCall(app, "alice", "GET", "/p/default/api/jev/jobs", "")
	var list struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Jobs) != jevKeptJobs+1 {
		t.Fatalf("listing after retention: %d, %d jobs", w.Code, len(list.Jobs))
	}
	if list.Jobs[0]["id"] != next {
		t.Fatal("the newest job was not listed first", list.Jobs[0]["id"])
	}
}
