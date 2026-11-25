package web

import (
	"context"
	"fmt"
	"forgetmenote/internal/catalog"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var jevWriteFailures atomic.Int32

// failJevRecordWrites makes Postgres refuse every insert and update of Jev's
// records in app's org whose new row meets condition, until the returned
// function runs or the test ends.
func failJevRecordWrites(t *testing.T, app *Server, condition string) func() {
	t.Helper()
	name := fmt.Sprintf("fail_jev_%s_%d", strings.ReplaceAll(app.db.Org, "-", "_"), jevWriteFailures.Add(1))
	ctx := context.Background()
	if _, err := app.db.Pool.Exec(ctx, "CREATE FUNCTION "+name+"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected write failure'; END $$"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Pool.Exec(ctx, "CREATE TRIGGER "+name+" BEFORE INSERT OR UPDATE ON jev_records FOR EACH ROW WHEN (NEW.org = '"+app.db.Org+"' AND ("+condition+")) EXECUTE FUNCTION "+name+"()"); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	restore := func() {
		once.Do(func() {
			if _, err := app.db.Pool.Exec(ctx, "DROP TRIGGER IF EXISTS "+name+" ON jev_records; DROP FUNCTION IF EXISTS "+name+"()"); err != nil {
				t.Errorf("restore writes: %v", err)
			}
		})
	}
	t.Cleanup(restore)
	return restore
}

// runJevJobToRest launches the job and waits until its runner has returned.
func runJevJobToRest(t *testing.T, app *Server, id string, explicit bool) jevJob {
	t.Helper()
	app.launchJevJob(id, explicit)
	for deadline := time.Now().Add(30 * time.Second); app.jevState.running()[id]; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the run did not return")
		}
	}
	job, err := app.jevState.readJob(id)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func organizerTagInput(operation, mode string) jevJobInput {
	return jevJobInput{Kind: "organize", OperationID: operation, Organization: &organizationInput{Mode: mode, Scope: "selected", Paths: []string{"a.md"}, Policy: organizationPolicy{Tags: true, Vocabulary: []string{"workshop"}}}}
}

// A run that cannot start, here because the notebook was withdrawn after the
// job was queued, stops paused with a message rather than staying running.
func TestOrganizerRunWithoutNotebookAccessPausesWithMessage(t *testing.T) {
	app := reviewFixture(t, syntheticOrganizationAnswer)
	job, err := app.createOrganizationJob(context.Background(), "alice", "default", organizerTagInput("withdrawn", "preview"), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", "bob", "owner"); err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	stored := runJevJobToRest(t, app, job.ID, false)
	if stored.State != "paused" || stored.Message != "Notebook permission is unavailable." {
		t.Fatalf("the run ended %q: %q", stored.State, stored.Message)
	}
}

// When an applied action's result cannot be saved the run pauses with a
// message; Continue records the result without a second judgment or commit.
func TestOrganizerResultWriteFailurePausesAndContinueRecordsIt(t *testing.T) {
	var calls atomic.Int32
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); syntheticOrganizationAnswer(w, r) })
	job, err := app.createOrganizationJob(context.Background(), "alice", "default", organizerTagInput("result-lost", "apply"), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	restore := failJevRecordWrites(t, app, "NEW.name LIKE 'results/%'")
	stored := runJevJobToRest(t, app, job.ID, false)
	if stored.State != "paused" || !strings.Contains(stored.Message, "result could not be saved") || stored.Cursor != 0 {
		t.Fatalf("the run ended %q at %d: %q", stored.State, stored.Cursor, stored.Message)
	}
	store, _ := app.catalog.Access("alice", "default", "read")
	applied, err := store.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	restore()
	if w := proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs/"+job.ID+"/continue", `{}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if done := waitReview(t, app, job.ID); done["state"] != "completed" || !strings.Contains(fmt.Sprint(done["results"]), "applied") {
		t.Fatal(done["state"], done["message"], done["results"])
	}
	after, _ := store.Status(context.Background())
	doc, _ := store.Read(context.Background(), "a.md", "")
	if calls.Load() != 1 || after.Revision != applied.Revision || !strings.Contains(string(doc.Content), "tags:") {
		t.Fatalf("Continue repeated the judgment or the commit: %d calls, %s then %s\n%s", calls.Load(), applied.Revision, after.Revision, doc.Content)
	}
}

// When the finished run cannot be recorded the job pauses with a message, and
// Continue finishes it.
func TestOrganizerCompletionSaveFailurePausesAndContinueFinishes(t *testing.T) {
	app := reviewFixture(t, syntheticOrganizationAnswer)
	job, err := app.createOrganizationJob(context.Background(), "alice", "default", organizerTagInput("unrecorded", "preview"), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	restore := failJevRecordWrites(t, app, "NEW.name LIKE 'jobs/%' AND NEW.body->>'state' = 'completed'")
	stored := runJevJobToRest(t, app, job.ID, false)
	if stored.State != "paused" || !strings.Contains(stored.Message, "outcome could not be saved") || stored.Cursor != stored.Total {
		t.Fatalf("the run ended %q at %d of %d: %q", stored.State, stored.Cursor, stored.Total, stored.Message)
	}
	restore()
	if stored = runJevJobToRest(t, app, job.ID, true); stored.State != "completed" {
		t.Fatal(stored.State, stored.Message)
	}
}

// A pause that cannot be saved either is logged with the job, not dropped.
func TestOrganizerUnsavedPauseIsLogged(t *testing.T) {
	app := reviewFixture(t, syntheticOrganizationAnswer)
	job, err := app.createOrganizationJob(context.Background(), "alice", "default", organizerTagInput("unsaved", "preview"), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	var logs lockedLogBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	failJevRecordWrites(t, app, "NEW.name LIKE 'results/%' OR NEW.body->>'state' = 'paused'")
	runJevJobToRest(t, app, job.ID, false)
	if !strings.Contains(logs.String(), `"msg":"jev job state not saved","job":"`+job.ID+`","state":"paused"`) {
		t.Fatalf("the unsaved pause was not logged:\n%s", logs.String())
	}
}

// The review runner pauses the same way when a result cannot be saved.
func TestJevReviewResultWriteFailurePausesWithMessage(t *testing.T) {
	app := reviewFixture(t, syntheticReviewAnswer)
	restore := failJevRecordWrites(t, app, "NEW.name LIKE 'results/%'")
	id := startReview(t, app, `{"kind":"consistency","operationId":"result-lost"}`)
	for deadline := time.Now().Add(30 * time.Second); app.jevState.running()[id]; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the run did not return")
		}
	}
	stored, err := app.jevState.readJob(id)
	if err != nil || stored.State != "paused" || !strings.Contains(stored.Message, "result could not be saved") {
		t.Fatalf("the review ended %q: %q %v", stored.State, stored.Message, err)
	}
	restore()
	if w := proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs/"+id+"/continue", `{}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if done := waitReview(t, app, id); done["state"] != "completed" {
		t.Fatal(done["state"], done["message"])
	}
}
