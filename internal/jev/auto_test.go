package jev

import (
	"context"
	"forgetmenote/internal/catalog"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestJevAutomaticReviewDebouncesCommittedChangesAndIgnoresBookkeeping(t *testing.T) {
	var calls atomic.Int32
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		syntheticReviewAnswer(w, r)
	})
	if w := proxyCall(app, "alice", "POST", "/p/default/api/jev/settings", `{"autoChecks":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	now := time.Now()
	app.scanJevAutomatic(context.Background(), now)
	if calls.Load() != 0 {
		t.Fatal("automatic setting launched an unrequested broad baseline review")
	}
	store, _ := app.catalog.Access("alice", "default", "edit")
	doc, err := store.Read(context.Background(), "a.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(context.Background(), doc.Path, []byte("# Workshop\nThe workshop opens at 10:00 on weekdays."), doc.Revision, "alice"); err != nil {
		t.Fatal(err)
	}
	app.scanJevAutomatic(context.Background(), now)
	app.scanJevAutomatic(context.Background(), now.Add(time.Minute))
	if calls.Load() != 0 {
		t.Fatal("review ignored debounce")
	}
	app.scanJevAutomatic(context.Background(), now.Add(3*time.Minute))
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if calls.Load() != 2 {
		t.Fatalf("changed note did not trigger its self/pair review: %d calls", calls.Load())
	}
	// Let the review finish and reconcile the same content despite finding commits.
	app.jevState.mu.Lock()
	ids, err := app.jevState.jobIDs()
	app.jevState.mu.Unlock()
	if err != nil || len(ids) != 1 {
		t.Fatal(ids, err)
	}
	if job := waitReview(t, app, ids[0]); job["state"] != "completed" {
		t.Fatal(job)
	}
	app.scanJevAutomatic(context.Background(), now.Add(4*time.Minute))
	app.scanJevAutomatic(context.Background(), now.Add(10*time.Minute))
	if calls.Load() != 2 {
		t.Fatal("review bookkeeping retriggered scans")
	}
	app.catalog.Member(catalog.Bootstrap, "default", "alice", "read")
	doc, _ = store.Read(context.Background(), "a.md", "")
	store.Save(context.Background(), doc.Path, []byte("# Workshop\nThe workshop opens at 12:00 on weekdays."), doc.Revision, "operator")
	app.scanJevAutomatic(context.Background(), now.Add(11*time.Minute))
	app.scanJevAutomatic(context.Background(), now.Add(15*time.Minute))
	if calls.Load() != 2 {
		t.Fatal("revoked Owner launched background work")
	}
}

func TestJevAutomaticReviewChecksOnlyChangedPassages(t *testing.T) {
	var calls atomic.Int32
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		syntheticReviewAnswer(w, r)
	})
	store, _ := app.catalog.Access("alice", "default", "edit")
	days := []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	hours := func(changed string) []byte {
		text := "# Workshop hours\n"
		for _, day := range days {
			opens := "09:00"
			if day == changed {
				opens = "10:30"
			}
			text += "\n## " + day + "\nThe workshop opens at " + opens + " on " + day + "s and closes at 17:00.\n"
		}
		return []byte(text)
	}
	doc, err := store.Save(context.Background(), "hours.md", hours(""), "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if w := proxyCall(app, "alice", "POST", "/p/default/api/jev/settings", `{"autoChecks":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	jobIDs := func() []string {
		t.Helper()
		app.jevState.mu.Lock()
		defer app.jevState.mu.Unlock()
		ids, err := app.jevState.jobIDs()
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	// review runs the automatic scan past its debounce and waits for the one
	// review it starts.
	review := func(at time.Time) map[string]any {
		t.Helper()
		before := jobIDs()
		app.scanJevAutomatic(context.Background(), at)
		app.scanJevAutomatic(context.Background(), at.Add(3*time.Minute))
		started := []string{}
		for _, id := range jobIDs() {
			if !slices.Contains(before, id) {
				started = append(started, id)
			}
		}
		if len(started) != 1 {
			t.Fatalf("the scan started %d reviews", len(started))
		}
		job := waitReview(t, app, started[0])
		if job["state"] != "completed" {
			t.Fatal(job)
		}
		app.scanJevAutomatic(context.Background(), at.Add(4*time.Minute))
		return job
	}

	// One changed section is checked alone and against its related passages;
	// the five sections that did not change are not checked again.
	if doc, err = store.Save(context.Background(), doc.Path, hours("Wednesday"), doc.Revision, "alice"); err != nil {
		t.Fatal(err)
	}
	job := review(time.Now())
	if total, _ := job["total"].(float64); total < 1 || total > 5 || int32(total) != calls.Load() {
		t.Fatalf("a one-section edit made %v work items and %d calls", job["total"], calls.Load())
	}

	// A note moved without changing its text has nothing new to check.
	before := calls.Load()
	if doc, err = store.Read(context.Background(), doc.Path, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Move(context.Background(), doc.Path, "schedule/hours.md", doc.Revision, "alice"); err != nil {
		t.Fatal(err)
	}
	jobs := func() int { return len(jobIDs()) }
	reviews := jobs()
	app.scanJevAutomatic(context.Background(), time.Now().Add(10*time.Minute))
	app.scanJevAutomatic(context.Background(), time.Now().Add(14*time.Minute))
	if jobs() != reviews || calls.Load() != before {
		t.Fatalf("moving an unchanged note started %d reviews and %d calls", jobs()-reviews, calls.Load()-before)
	}
	// The move became the baseline: a later edit is checked by its new passage.
	if doc, err = store.Read(context.Background(), "schedule/hours.md", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(context.Background(), doc.Path, hours("Friday"), doc.Revision, "alice"); err != nil {
		t.Fatal(err)
	}
	job = review(time.Now().Add(20 * time.Minute))
	if total, _ := job["total"].(float64); total < 1 || total > 10 || int32(total) != calls.Load()-before {
		t.Fatalf("an edit after the move made %v work items and %d calls", job["total"], calls.Load()-before)
	}
}
