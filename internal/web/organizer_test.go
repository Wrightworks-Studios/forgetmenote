package web

import (
	"context"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/jev"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func organizerStart(t *testing.T, app *Server, body string) string {
	t.Helper()
	w := proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs", body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var j map[string]any
	json.Unmarshal(w.Body.Bytes(), &j)
	return j["id"].(string)
}

func organizerChoiceAnswer(w http.ResponseWriter, r *http.Request) {
	var request struct {
		State     map[string]any               `json:"state"`
		Questions map[string]decision.Question `json:"questions"`
	}
	json.NewDecoder(r.Body).Decode(&request)
	answers := map[string]any{}
	for id, q := range request.Questions {
		if q.Type == "noul" {
			score := .99
			if id == "none" {
				score = .01
			}
			answers[id] = map[string]any{"type": "noul", "noul": score}
			continue
		}
		choices := q.Criteria.(map[string]any)
		choice := "related"
		if _, ok := choices["redundant"]; ok {
			choice = "redundant"
			raw, _ := json.Marshal(request.State["duplicate"])
			if strings.Contains(string(raw), "UNIQUE_DETAIL") {
				choice = "unique"
			}
		}
		if _, ok := choices["folder0"]; ok {
			choice = "folder0"
		}
		probs := map[string]float64{}
		for k := range choices {
			probs[k] = 0
		}
		probs[choice] = 1
		answers[id] = map[string]any{"type": "choice", "choice": choice, "confidence": 1, "probabilities": probs}
	}
	json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 120, "output_tokens": 20}})
}

func TestOrganizerConsentWithdrawalDuringInferenceStopsMutation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		syntheticOrganizationAnswer(w, r)
	})
	id := organizerStart(t, app, `{"kind":"organize","operationId":"consent","organization":{"mode":"apply","scope":"selected","paths":["a.md"],"policy":{"tags":true,"vocabulary":["workshop"]}}}`)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("inference did not start")
	}
	w := proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":false,"rerank":false,"citations":false}`)
	close(release)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	job := waitReview(t, app, id)
	if job["state"] == "completed" {
		t.Fatal("revoked work completed")
	}
	store, _ := app.catalog.Access("alice", "default", "read")
	doc, _ := store.Read(context.Background(), "a.md", "")
	if strings.Contains(string(doc.Content), "tags:") {
		t.Fatal("consent withdrawal allowed mutation")
	}
}

func TestOrganizerRevokedConsentCannotResumeOldGrant(t *testing.T) {
	app := reviewFixture(t, syntheticOrganizationAnswer)
	id := organizerTagJob(t, app, "alice", "old-grant")
	for _, consent := range []bool{false, true} {
		w := proxyCall(app, "alice", "POST", "/api/jev/settings", fmt.Sprintf(`{"consent":%t}`, consent))
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	if e := app.jev.CheckJob(context.Background(), id); e == nil {
		t.Fatal("renewed consent revived an invalidated applying grant")
	}
}
func TestOrganizerLockedNotesNeverReachProvider(t *testing.T) {
	var calls atomic.Int32
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); syntheticOrganizationAnswer(w, r) })
	store, _ := app.catalog.Access("alice", "default", "owner")
	state, _ := store.Status(context.Background())
	if _, e := store.SetLock(context.Background(), "a.md", true, state.Revision, "alice"); e != nil {
		t.Fatal(e)
	}
	id := organizerStart(t, app, `{"kind":"organize","operationId":"locked","organization":{"mode":"apply","scope":"selected","paths":["a.md"],"policy":{"tags":true,"vocabulary":["workshop"]}}}`)
	waitReview(t, app, id)
	if calls.Load() != 0 {
		t.Fatal("locked mutation target was sent to provider")
	}
}

func TestOrganizerRevokedMembershipCannotResumeOldGrant(t *testing.T) {
	app := reviewFixture(t, syntheticOrganizationAnswer)
	id := organizerTagJob(t, app, "alice", "old-membership")
	if e := app.catalog.Member(catalog.Bootstrap, "default", "bob", "owner"); e != nil {
		t.Fatal(e)
	}
	for _, role := range []string{"read", "owner"} {
		if e := app.catalog.Member(catalog.Bootstrap, "default", "alice", role); e != nil {
			t.Fatal(e)
		}
	}
	if e := app.jev.CheckJob(context.Background(), id); e == nil {
		t.Fatal("restored membership revived an invalidated applying grant")
	}
}

// An applied action stays undoable after consent is withdrawn, even when its
// result was never recorded: reading the job recovers it from its Git receipt.
func TestOrganizerCommittedActionRemainsUndoableAfterRevocation(t *testing.T) {
	app := reviewFixture(t, syntheticOrganizationAnswer)
	restore := failJevRecordWrites(t, app, "NEW.name LIKE 'results/%'")
	id := organizerStart(t, app, `{"kind":"organize","operationId":"receipt-revoked","organization":{"mode":"apply","scope":"selected","paths":["a.md"],"policy":{"tags":true,"vocabulary":["workshop"]}}}`)
	// The run commits, cannot record the result and pauses. Reading a paused
	// job recovers the result, which fails too while writes fail.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		w := proxyCall(app, "alice", "GET", "/p/default/api/jev/jobs/"+id, "")
		var job map[string]any
		json.Unmarshal(w.Body.Bytes(), &job)
		if w.Code != 200 || job["state"] == "paused" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run did not stop at its unrecorded result", w.Code, w.Body.String())
		}
	}
	store, _ := app.catalog.Access("alice", "default", "owner")
	if d, _ := store.Read(context.Background(), "a.md", ""); !strings.Contains(string(d.Content), "tags:") {
		t.Fatal("the action was not committed")
	}
	restore()
	if w := proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":false}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	got := waitReview(t, app, id)
	if len(got["results"].([]any)) != 1 {
		t.Fatal("committed action disappeared behind revoked consent", got)
	}
	w := proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs/"+id+"/organization/undo", `{}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"error"`) {
		t.Fatal(w.Body.String())
	}
	d, _ := store.Read(context.Background(), "a.md", "")
	if strings.Contains(string(d.Content), "tags:") {
		t.Fatal("revoked receipt not undone")
	}
}

// A run that cannot start, here because its owner lost the role it needs while
// it was queued, stops paused with a message rather than staying running.
func TestOrganizerRunWithoutNotebookAccessPausesWithMessage(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() {
			close(entered)
			<-release
		})
		syntheticReviewAnswer(w, r)
	})
	// A review holds the one run this gateway allows, so the organizer queues.
	ahead := startReview(t, app, `{"kind":"consistency","operationId":"ahead"}`)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the review did not start")
	}
	queued := organizerStart(t, app, `{"kind":"organize","operationId":"withdrawn","organization":{"mode":"preview","scope":"selected","paths":["a.md"],"policy":{"tags":true,"vocabulary":["workshop"]}}}`)
	var job map[string]any
	if w := proxyCall(app, "alice", "GET", "/p/default/api/jev/jobs/"+queued, ""); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &job) != nil || job["state"] != "queued" {
		close(release)
		t.Fatal("the organizer did not queue", w.Code, w.Body.String())
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "bob", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitReview(t, app, ahead)
	stored := waitReview(t, app, queued)
	if stored["state"] != "paused" || stored["message"] != "Notebook permission is unavailable." {
		t.Fatalf("the run ended %q: %q", stored["state"], stored["message"])
	}
}

func TestOrganizerDifferentOwnersCanUndoDependentActions(t *testing.T) {
	app := reviewFixture(t, organizerChoiceAnswer)
	if e := app.catalog.Member(catalog.Bootstrap, "default", "bob", "owner"); e != nil {
		t.Fatal(e)
	}
	id := organizerStart(t, app, `{"kind":"organize","operationId":"two-owners","organization":{"mode":"apply","scope":"selected","paths":["a.md"],"policy":{"tags":true,"links":true,"vocabulary":["workshop"]}}}`)
	waitReview(t, app, id)
	w := proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs/"+id+"/organization/undo", `{"index":1}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"error"`) {
		t.Fatal(w.Body.String())
	}
	w = proxyCall(app, "bob", "POST", "/p/default/api/jev/jobs/"+id+"/organization/undo", `{}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"error"`) {
		t.Fatal("different owner could not continue dependent Undo", w.Body.String())
	}
}

func TestOrganizerSettingsExposeOnlyPublicPolicy(t *testing.T) {
	app := reviewFixture(t, syntheticOrganizationAnswer)
	w := proxyCall(app, "alice", "POST", "/p/default/api/jev/organization", `{"enabled":true,"generation":0,"policy":{"tags":true,"vocabulary":["workshop"]}}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, user := range []string{"alice", "bob"} {
		w = proxyCall(app, user, "GET", "/p/default/api/jev/organization", "")
		var response struct {
			Settings map[string]json.RawMessage `json:"settings"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil {
			t.Fatal(user, w.Body.String())
		}
		if len(response.Settings) != 4 || response.Settings["enabled"] == nil || response.Settings["generation"] == nil || response.Settings["policy"] == nil || response.Settings["owner"] == nil {
			t.Fatal("public settings exposed internal organization state", user, string(w.Body.Bytes()))
		}
	}
}

// organizerTagJob prepares, as user, a job that tags a.md in the default
// notebook, without running it, and returns its ID.
func organizerTagJob(t *testing.T, app *Server, user, operation string) string {
	t.Helper()
	var input jev.JobInput
	body := `{"kind":"organize","operationId":"` + operation + `","organization":{"mode":"apply","scope":"selected","paths":["a.md"],"policy":{"tags":true,"vocabulary":["workshop"]}}}`
	if err := json.Unmarshal([]byte(body), &input); err != nil {
		t.Fatal(err)
	}
	id, err := app.jev.CreateJob(context.Background(), user, "default", input, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

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
