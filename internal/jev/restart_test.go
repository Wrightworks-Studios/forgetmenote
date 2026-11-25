package jev

import (
	"context"
	"encoding/json"
	"errors"
	"forgetmenote/internal/auth"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/pg/pgtest"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestJevReviewRestartResumesDurableCheckpointAndUnknownCost(t *testing.T) {
	var calls atomic.Int32
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 2 {
			http.Error(w, "synthetic outage", 503)
			return
		}
		syntheticReviewAnswer(w, r)
	})
	id := startReview(t, app, `{"kind":"consistency","operationId":"restart"}`)
	paused := waitReview(t, app, id)
	if paused["state"] != "paused" || paused["examined"] != float64(1) || len(paused["results"].([]any)) != 1 {
		t.Fatal(paused)
	}
	first := paused["results"].([]any)[0].(map[string]any)["id"]
	reopened := reopen(t, app)
	w := proxyCall(reopened, "alice", "GET", "/p/default/api/jev/jobs/"+id, "")
	var restored map[string]any
	json.Unmarshal(w.Body.Bytes(), &restored)
	if w.Code != 200 || restored["results"].([]any)[0].(map[string]any)["id"] != first {
		t.Fatal("partial result lost across restart", w.Body.String())
	}
	if w := proxyCall(reopened, "alice", "POST", "/p/default/api/jev/jobs/"+id+"/continue", `{}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if job := waitReview(t, reopened, id); job["state"] != "completed" {
		t.Fatal(job)
	}
	records, err := usageLedger(t, reopened).Records()
	if err != nil || len(records) != 4 || records[1].Status != "unknown" {
		t.Fatalf("restart lost attempted usage: %+v %v", records, err)
	}
}

func TestJevReviewCancellationRetainsInFlightUsageAndNoFindings(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		syntheticReviewAnswer(w, r)
	})
	id := startReview(t, app, `{"kind":"consistency","operationId":"cancel"}`)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no paid attempt")
	}
	if w := proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs/"+id+"/cancel", `{}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	close(release)
	if job := waitReview(t, app, id); job["state"] != "cancelled" {
		t.Fatal(job)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		app.jevState.mu.Lock()
		active := app.jevState.active["job:"+id] != nil
		app.jevState.mu.Unlock()
		if !active {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	records, err := usageLedger(t, app).Records()
	if err != nil || len(records) != 1 || records[0].Status == "reserved" {
		t.Fatal(records, err)
	}
	store, _ := app.catalog.Access("alice", "default", "read")
	findings, err := store.Findings(context.Background())
	if err != nil || len(findings) != 0 {
		t.Fatal(findings, err)
	}
}

// A review cancelled through one gateway while another saves its checkpoint
// stays cancelled, and the checkpoint is either kept or refused, never lost.
func TestJevReviewCancelledOnAnotherGatewaySurvivesACheckpoint(t *testing.T) {
	app := chatFixture(t)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "comment"); err != nil {
		t.Fatal(err)
	}
	other := gatewayOf(t, app)
	now := chat.Now()
	job := jevJob{Version: 2, ID: auth.Digest("checkpoint"), RequestHash: auth.Digest("request"), Project: "default", Owner: "alice", AccountID: "account", Kind: "consistency", State: "running", Inventory: "inventory", Model: "synthetic", Notes: []jevJobNote{}, Work: []jevJobWork{{Kind: "consistency", Passages: []string{}}, {Kind: "consistency", Passages: []string{}}}, Total: 2, Remaining: 2, CreatedAt: now, UpdatedAt: now}
	if err := app.jevState.write(jobName(job.ID), job); err != nil {
		t.Fatal(err)
	}
	checkpoint := job
	checkpoint.Work = []jevJobWork{{Kind: "consistency", Passages: []string{}, Verdict: "compatible"}, {Kind: "consistency", Passages: []string{}}}
	checkpoint.Cursor = 1
	waiting, release := pgtest.HoldWrites(t, app.db, "jev_records")
	saved := make(chan error, 1)
	go func() { saved <- app.saveJevJob(&checkpoint) }()
	waiting(1)
	cancelled := make(chan int, 1)
	go func() {
		cancelled <- proxyCall(other, "alice", "POST", "/p/default/api/jev/jobs/"+job.ID+"/cancel", `{}`).Code
	}()
	waiting(2)
	release()
	if code := <-cancelled; code != 200 {
		t.Fatal("the cancellation answered", code)
	}
	saveErr := <-saved
	stored, err := app.jevState.readJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Cancelled || stored.State != "cancelled" {
		t.Fatal("a checkpoint overwrote a cancellation made through another gateway", stored.State, saveErr)
	}
	if stored.Cursor != 1 && !errors.Is(saveErr, context.Canceled) {
		t.Fatal("a cancellation through another gateway overwrote a checkpoint it never read", stored.Cursor, saveErr)
	}
}
