package web

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
)

func TestJevR1ConsistencyRefreshAfterContextChange(t *testing.T) {
	app := reviewFixture(t, syntheticReviewAnswer)
	first := startReview(t, app, `{"kind":"consistency","operationId":"review-first"}`)
	if job := waitReview(t, app, first); job["state"] != "completed" {
		t.Fatal(job)
	}
	store, err := app.catalog.Access("alice", "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Read(context.Background(), "a.md", "")
	if err != nil {
		t.Fatal(err)
	}
	// Preserve the contradictory original passage while editing another section.
	if _, err = store.Save(context.Background(), "a.md", append(before.Content, []byte("\n\n# Context\nThe service desk handles all workshop inquiries.\n")...), before.Version, "alice"); err != nil {
		t.Fatal(err)
	}
	old, err := store.Findings(context.Background())
	if err != nil || len(old) != 1 || !old[0].Comment.Finding.Stale {
		t.Fatal("old finding must be stale", old, err)
	}
	second := startReview(t, app, `{"kind":"consistency","operationId":"review-after-context-change"}`)
	job := waitReview(t, app, second)
	if job["state"] != "completed" {
		if response := proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs/"+second+"/continue", `{}`); response.Code != 200 {
			t.Fatal("continue rejected", response.Code)
		}
		again := waitReview(t, app, second)
		t.Logf("explicit Continue state=%v examined=%v", again["state"], again["examined"])
		t.Fatalf("fresh review cannot replace stale finding: state=%v examined=%v total=%v message=%v", job["state"], job["examined"], job["total"], job["message"])
	}
}

func TestJevR1FindingMetadataRefreshContinueAndBookkeepingIdempotency(t *testing.T) {
	var failNext atomic.Bool
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if failNext.Swap(false) {
			http.Error(w, "synthetic outage", 503)
			return
		}
		syntheticReviewAnswer(w, r)
	})
	first := startReview(t, app, `{"kind":"consistency","operationId":"metadata-original"}`)
	if job := waitReview(t, app, first); job["state"] != "completed" {
		t.Fatal(job)
	}
	store, _ := app.catalog.Access("alice", "default", "edit")
	before, err := store.Read(context.Background(), "a.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(context.Background(), "a.md", append([]byte("---\ntags: [workshop]\n---\n"), before.Content...), before.Version, "alice"); err != nil {
		t.Fatal(err)
	}
	failNext.Store(true)
	second := startReview(t, app, `{"kind":"consistency","operationId":"metadata-refresh"}`)
	if job := waitReview(t, app, second); job["state"] != "paused" {
		t.Fatal(job)
	}
	if w := proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs/"+second+"/continue", `{}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if job := waitReview(t, app, second); job["state"] != "completed" {
		t.Fatal(job)
	}
	findings, err := store.Findings(context.Background())
	if err != nil || len(findings) != 2 {
		t.Fatal("metadata refresh did not create current dependency identity", findings, err)
	}
	current := 0
	for _, finding := range findings {
		if !finding.Comment.Finding.Stale {
			current++
		}
	}
	if current != 1 {
		t.Fatal("current finding count", current)
	}
	third := startReview(t, app, `{"kind":"consistency","operationId":"bookkeeping-repeat","sources":[{"path":"b.md"}]}`)
	if job := waitReview(t, app, third); job["state"] != "completed" {
		t.Fatal(job)
	}
	after, err := store.Findings(context.Background())
	if err != nil || len(after) != len(findings) {
		t.Fatal("unchanged bookkeeping or reversed pair republished finding", after, err)
	}
}
