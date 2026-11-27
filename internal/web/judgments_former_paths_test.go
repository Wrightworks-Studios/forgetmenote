package web

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/catalog"
	"strings"
	"testing"
)

// The judgments routes moved from /api/jev. For one release each former path
// still serves the same handler with the same role check, so a browser or
// client of the previous release keeps working.
func TestJudgmentsFormerJevPathsServeTheSameRoutes(t *testing.T) {
	app := chatFixture(t)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	configureSyntheticJudgments(t, app, syntheticReviewAnswer)
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	for path, text := range map[string]string{"notes/a.md": "# Workshop\nThe workshop opens at 09:00 on weekdays.", "notes/b.md": "# Workshop\nThe workshop opens at 11:00 on weekdays."} {
		if _, err := store.Save(context.Background(), path, []byte(text), "", "operator"); err != nil {
			t.Fatal(err)
		}
	}
	former := func(path string) string { return strings.Replace(path, "/api/judgments/", "/api/jev/", 1) }
	// both makes the same request at the judgments path and at its former
	// path, and requires the same status from each.
	both := func(user, method, path, body string, status int) (current, previous string) {
		t.Helper()
		w := proxyCall(app, user, method, path, body)
		old := proxyCall(app, user, method, former(path), body)
		if w.Code != status || old.Code != status {
			t.Fatalf("%s %s by %s: %d %s; former path %d %s; want %d", method, path, user, w.Code, w.Body.String(), old.Code, old.Body.String(), status)
		}
		return w.Body.String(), old.Body.String()
	}
	same := func(user, path string) string {
		t.Helper()
		current, previous := both(user, "GET", path, "", 200)
		if current != previous {
			t.Fatalf("GET %s: judgments path %s, former path %s", path, current, previous)
		}
		return current
	}

	// Personal settings: a change saved at the former path is the one the
	// judgments path reads.
	if w := proxyCall(app, "alice", "POST", former("/api/judgments/settings"), `{"consent":true,"rerank":true,"citations":true}`); w.Code != 200 {
		t.Fatalf("save personal settings at the former path: %d %s", w.Code, w.Body.String())
	}
	var personal struct {
		Settings struct{ Consent bool } `json:"settings"`
	}
	if json.Unmarshal([]byte(same("alice", "/api/judgments/settings")), &personal); !personal.Settings.Consent {
		t.Fatal("consent saved at the former path is not the judgments setting")
	}

	// Notebook settings and organization: readers read, only an Owner writes.
	same("bob", "/p/default/api/judgments/settings")
	both("bob", "POST", "/p/default/api/judgments/settings", `{"autoChecks":false}`, 403)
	both("alice", "POST", "/p/default/api/judgments/settings", `{"autoChecks":false}`, 200)
	same("bob", "/p/default/api/judgments/organization")
	both("bob", "POST", "/p/default/api/judgments/organization", `{"enabled":false,"generation":0,"policy":{}}`, 403)

	// Jobs: a reader cannot start one; the same operation at either path is
	// the same job.
	both("bob", "POST", "/p/default/api/judgments/jobs", `{"kind":"consistency","operationId":"former-path"}`, 403)
	created, again := both("alice", "POST", "/p/default/api/judgments/jobs", `{"kind":"consistency","operationId":"former-path"}`, 200)
	var job, repeat struct{ ID string }
	if json.Unmarshal([]byte(created), &job) != nil || json.Unmarshal([]byte(again), &repeat) != nil || job.ID == "" || job.ID != repeat.ID {
		t.Fatalf("the former path started a different job: %s and %s", created, again)
	}
	waitReview(t, app, job.ID)
	if listed := same("alice", "/p/default/api/judgments/jobs"); !strings.Contains(listed, job.ID) {
		t.Fatalf("job list misses %s: %s", job.ID, listed)
	}
	same("alice", "/p/default/api/judgments/jobs/"+job.ID)
	both("bob", "POST", "/p/default/api/judgments/jobs/"+job.ID+"/cancel", `{}`, 403)
	both("bob", "POST", "/p/default/api/judgments/jobs/"+job.ID+"/continue", `{}`, 403)
	both("bob", "POST", "/p/default/api/judgments/jobs/"+job.ID+"/organization/dismiss", `{}`, 403)
	// A completed review can be cancelled; a cancelled one cannot be
	// continued, and a review has no organization to dismiss. Each handler
	// answers both paths alike, where an unregistered path would get the mux's
	// "404 page not found". The order matters: continuing a review that is not
	// cancelled starts it again.
	for _, step := range []struct {
		control string
		status  int
	}{{"/cancel", 200}, {"/continue", 409}, {"/organization/dismiss", 404}} {
		path := "/p/default/api/judgments/jobs/" + job.ID + step.control
		w, old := proxyCall(app, "alice", "POST", path, `{}`), proxyCall(app, "alice", "POST", former(path), `{}`)
		if w.Code != step.status || old.Code != step.status || strings.Contains(w.Body.String()+old.Body.String(), "page not found") {
			t.Fatalf("POST %s: %d %s; former path %d %s; want %d from the handler", path, w.Code, w.Body.String(), old.Code, old.Body.String(), step.status)
		}
	}
}
