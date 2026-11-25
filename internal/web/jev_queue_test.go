package web

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestJevReviewQueueBoundsConcurrentSourcePreparation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		syntheticReviewAnswer(w, r)
	})
	first := startReview(t, app, `{"kind":"consistency","operationId":"first"}`)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first job did not start")
	}
	second := startReview(t, app, `{"kind":"consistency","operationId":"second"}`)
	w := proxyCall(app, "alice", "GET", "/p/default/api/jev/jobs/"+second, "")
	var pending map[string]any
	json.Unmarshal(w.Body.Bytes(), &pending)
	close(release)
	if pending["state"] != "queued" {
		t.Fatalf("second job prepared sources concurrently instead of remaining durable queued: %+v", pending)
	}
	if job := waitReview(t, app, first); job["state"] != "completed" {
		t.Fatal(job)
	}
	if job := waitReview(t, app, second); job["state"] != "completed" {
		t.Fatal(job)
	}
}
