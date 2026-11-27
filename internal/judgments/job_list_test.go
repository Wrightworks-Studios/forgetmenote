package judgments

import (
	"context"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/auth"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/pg"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// syntheticJudgmentJob is a stored job of the default notebook in state, with no
// work, as old reviews accumulate on a busy instance.
func syntheticJudgmentJob(state string) judgmentJob {
	return judgmentJob{Version: 2, RequestHash: "request", Project: "default", Owner: "alice", AccountID: "account", Kind: "consistency", State: state, Cancelled: state == "cancelled", CreatedAt: chat.Now(), Message: "Synthetic history.", Inventory: "inventory", Model: "jev-1.13.0", Notes: []judgmentJobNote{}, Work: []judgmentJobWork{}}
}

// storeJudgmentJobs stores n copies of job under new IDs in one statement and
// returns their IDs; the i-th was last updated at at(i).
func storeJudgmentJobs(t *testing.T, app *Service, n int, job judgmentJob, at func(i int) time.Time) []string {
	t.Helper()
	ids, names, bodies := make([]string, n), make([]string, n), make([]string, n)
	for i := range n {
		job.ID = auth.Digest(fmt.Sprint(t.Name(), job.State, time.Now().UnixNano(), i))
		job.UpdatedAt = at(i).UTC().Format(time.RFC3339Nano)
		raw, err := json.Marshal(job)
		if err != nil {
			t.Fatal(err)
		}
		ids[i], names[i], bodies[i] = job.ID, jobName(job.ID), string(raw)
	}
	if _, err := app.db.Pool.Exec(context.Background(), `INSERT INTO jev_records (org, name, body) SELECT $1, name, body::jsonb FROM unnest($2::text[], $3::text[]) AS stored(name, body)`, app.db.Org, names, bodies); err != nil {
		t.Fatal(err)
	}
	return ids
}

func storedJudgmentNames(t *testing.T, app *Service) map[string]bool {
	t.Helper()
	names, err := app.judgmentState.records.List("", 100000)
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string]bool{}
	for _, name := range names {
		stored[name] = true
	}
	return stored
}

// More finished jobs than one listing reads, as an instance from before
// retention holds, do not hide a queued job from the queue.
func TestJudgmentQueuedJobLaunchesPastTheJobLimit(t *testing.T) {
	app := reviewFixture(t, syntheticReviewAnswer)
	queued, err := app.createJudgmentJob(context.Background(), "alice", "default", JobInput{Kind: "consistency", OperationID: "behind-history"}, false, nil, nil)
	if err != nil || queued.State != "queued" {
		t.Fatal(queued.State, err)
	}
	now := time.Now()
	storeJudgmentJobs(t, app, judgmentJobLimit+1, syntheticJudgmentJob("completed"), func(i int) time.Time { return now.Add(-time.Duration(i+1) * time.Second) })
	app.launchNextJudgmentJob()
	if job := waitReview(t, app, queued.ID); job["state"] != "completed" {
		t.Fatal("the queued job did not run past the job limit", job["state"], job["message"])
	}
}

type judgmentRecordQueries struct{ n atomic.Int32 }

func (q *judgmentRecordQueries) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "jev_records") {
		q.n.Add(1)
	}
	return ctx
}
func (q *judgmentRecordQueries) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// take returns the statements on the judgment records since it was last called.
func (q *judgmentRecordQueries) take() int { return int(q.n.Swap(0)) }

// traceJudgmentRecords counts the statements the judgment records issue from now on.
func traceJudgmentRecords(t *testing.T, app *Service) *judgmentRecordQueries {
	t.Helper()
	queries := &judgmentRecordQueries{}
	config := app.db.Pool.Config()
	config.ConnConfig.Tracer = queries
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	records, err := decision.OpenRecords(&pg.DB{Pool: pool, Org: app.db.Org, Holder: app.db.Holder})
	if err != nil {
		t.Fatal(err)
	}
	app.judgmentState.records = records
	return queries
}

// Listing a notebook's jobs reads the judgment records once however many jobs there
// are, and notifications read the jobs once and each job's results once.
func TestJudgmentJobListingReadsRecordsOncePerListing(t *testing.T) {
	app := reviewFixture(t, syntheticReviewAnswer)
	reviews := []string{}
	for _, operation := range []string{"one", "two"} {
		id := startReview(t, app, `{"kind":"consistency","operationId":"`+operation+`"}`)
		if job := waitReview(t, app, id); job["state"] != "completed" || job["examined"] == float64(0) {
			t.Fatal(job["state"], job["examined"])
		}
		reviews = append(reviews, id)
	}
	storeJudgmentJobs(t, app, 20, syntheticJudgmentJob("completed"), func(i int) time.Time { return time.Now().Add(-time.Hour) })
	queries := traceJudgmentRecords(t, app)
	w := proxyCall(app, "alice", "GET", "/p/default/api/judgments/jobs", "")
	var list struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Jobs) != 22 {
		t.Fatalf("listing: %d %d", w.Code, len(list.Jobs))
	}
	if n := queries.take(); n != 1 {
		t.Fatalf("listing 22 jobs issued %d statements on the judgment records, not 1", n)
	}
	w = proxyCall(app, "alice", "GET", "/api/notifications", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), reviews[0]) && !strings.Contains(w.Body.String(), reviews[1]) {
		t.Fatalf("notifications: %d %s", w.Code, w.Body.String())
	}
	// The read state, the jobs, then the results of the two jobs that have any.
	if n := queries.take(); n != 2+len(reviews) {
		t.Fatalf("notifications issued %d statements on the judgment records, not %d", n, 2+len(reviews))
	}
}
