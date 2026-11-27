package web

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/quicknotes"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func reviewFixture(t *testing.T, handler http.HandlerFunc) *Server {
	app := chatFixture(t)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	configureSyntheticJudgments(t, app, handler)
	store, _ := app.catalog.Access("alice", "default", "edit")
	for path, text := range map[string]string{"a.md": "# Workshop\nThe workshop opens at 09:00 on weekdays.", "b.md": "# Workshop\nThe workshop opens at 11:00 on weekdays."} {
		if _, err := store.Save(context.Background(), path, []byte(text), "", "alice"); err != nil {
			t.Fatal(err)
		}
	}
	if w := proxyCall(app, "alice", "POST", "/api/judgments/settings", `{"consent":true,"rerank":true,"citations":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	return app
}

func startReview(t *testing.T, app *Server, body string) string {
	t.Helper()
	w := proxyCall(app, "alice", "POST", "/p/default/api/judgments/jobs", body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var value struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil || value.ID == "" {
		t.Fatal(w.Body.String())
	}
	return value.ID
}

func waitReview(t *testing.T, app *Server, id string) map[string]any {
	t.Helper()
	// Each status read validates real Git evidence, competing with the worker
	// for the notebook lock. These are completion tests, not latency targets.
	deadline := time.Now().Add(30 * time.Second)
	var value map[string]any
	for time.Now().Before(deadline) {
		w := proxyCall(app, "alice", "GET", "/p/default/api/judgments/jobs/"+id, "")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		if value["state"] != "running" && value["state"] != "queued" {
			return value
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("job did not settle: state=%v examined=%v total=%v message=%v", value["state"], value["examined"], value["total"], value["message"])
	return nil
}

func TestJudgmentJobFailureResumeRetainsUnknownUsage(t *testing.T) {
	var calls atomic.Int32
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "synthetic outage", 503)
			return
		}
		syntheticReviewAnswer(w, r)
	})
	id := startReview(t, app, `{"kind":"consistency","operationId":"resume"}`)
	job := waitReview(t, app, id)
	if job["state"] != "paused" {
		t.Fatal(job)
	}
	if w := proxyCall(app, "alice", "POST", "/p/default/api/judgments/jobs/"+id+"/continue", `{}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	job = waitReview(t, app, id)
	if job["state"] != "completed" {
		t.Fatal(job)
	}
	records, err := app.chat.JudgmentLedger.Records()
	if err != nil || len(records) != 4 || records[0].Status != "unknown" || records[0].Usage != nil {
		t.Fatalf("unknown attempt lost or repeated: %+v %v", records, err)
	}
}

func TestJudgmentJobRevocationBeforePublicationProducesNoFinding(t *testing.T) {
	var app *Server
	var calls atomic.Int32
	app = reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		app.catalog.Member(catalog.Bootstrap, "default", "alice", "read")
		syntheticReviewAnswer(w, r)
	})
	id := startReview(t, app, `{"kind":"consistency","operationId":"revoke"}`)
	job := waitReview(t, app, id)
	if job["state"] != "paused" || calls.Load() != 1 {
		t.Fatal(job, calls.Load())
	}
	store, _ := app.catalog.Access(catalog.Bootstrap, "default", "read")
	findings, err := store.Findings(context.Background())
	if err != nil || len(findings) != 0 {
		t.Fatal(findings, err)
	}
}

func TestJudgmentNotificationReadStateAndAccessRemainPersonal(t *testing.T) {
	app := reviewFixture(t, syntheticReviewAnswer)
	id := startReview(t, app, `{"kind":"consistency","operationId":"notify"}`)
	if job := waitReview(t, app, id); job["state"] != "completed" {
		t.Fatal(job)
	}
	var first struct {
		Unread int                        `json:"unreadCount"`
		Items  []judgmentNotificationItem `json:"items"`
	}
	w := proxyCall(app, "alice", "GET", "/api/notifications", "")
	json.Unmarshal(w.Body.Bytes(), &first)
	if first.Unread != 1 {
		t.Fatal(w.Body.String())
	}
	body, _ := json.Marshal(map[string]any{"ids": []string{first.Items[0].ID}})
	if w = proxyCall(app, "alice", "POST", "/api/notifications/read", string(body)); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = proxyCall(app, "bob", "GET", "/api/notifications", "")
	json.Unmarshal(w.Body.Bytes(), &first)
	if first.Unread != 1 {
		t.Fatal("shared read state", w.Body.String())
	}
	app.catalog.Member(catalog.Bootstrap, "default", "bob", "")
	w = proxyCall(app, "bob", "GET", "/api/notifications", "")
	if strings.Contains(w.Body.String(), "Workshop") || !strings.Contains(w.Body.String(), `"unreadCount":0`) {
		t.Fatal("revoked notebook preview/count exposed", w.Body.String())
	}
}

func TestQuicknotesFiledDestinationNotificationIsPrivateAndWithdrawnOnUndo(t *testing.T) {
	app := chatFixture(t)
	app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner")
	settings, err := app.quicknotes.Settings("alice")
	if err != nil {
		t.Fatal(err)
	}
	settings.Mode = "manual"
	settings.NotifyDestination = true
	if _, err = app.quicknotes.UpdateSettings("alice", settings); err != nil {
		t.Fatal(err)
	}
	capture, err := app.quicknotes.Capture(context.Background(), "alice", "notify-capture", "Private captured thought.")
	if err != nil {
		t.Fatal(err)
	}
	capture, err = app.quicknotes.File(context.Background(), "alice", capture.ID, quicknotes.Destination{Project: "default", Shared: true}, capture.Version)
	if err != nil {
		t.Fatal(err)
	}
	w := proxyCall(app, "alice", "GET", "/api/notifications", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Quicknote filed") || !strings.Contains(w.Body.String(), capture.Path) {
		t.Fatalf("filed notification missing: %d %s", w.Code, w.Body.String())
	}
	w = proxyCall(app, "bob", "GET", "/api/notifications", "")
	if strings.Contains(w.Body.String(), capture.Path) {
		t.Fatal("another user saw private capture event")
	}
	if _, err = app.quicknotes.Undo(context.Background(), "alice", capture.ID); err != nil {
		t.Fatal(err)
	}
	w = proxyCall(app, "alice", "GET", "/api/notifications", "")
	if strings.Contains(w.Body.String(), "Quicknote filed") {
		t.Fatal("undo retained filing notification")
	}
}

// judgmentNotificationItem is the part of a judgment notification these tests read.
type judgmentNotificationItem struct {
	ID       string                       `json:"id"`
	Evidence []knowledge.JudgmentEvidence `json:"evidence"`
}
