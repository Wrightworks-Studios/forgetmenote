package jev

import (
	"context"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/attribution"
	"forgetmenote/internal/auth"
	"forgetmenote/internal/decision"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOrganizerManualApplyAndPreviewWithoutAutomaticMode(t *testing.T) {
	for _, mode := range []string{"preview", "apply"} {
		t.Run(mode, func(t *testing.T) {
			app := reviewFixture(t, syntheticOrganizationAnswer)
			store, _ := app.catalog.Access("alice", "default", "owner")
			before, _ := store.Read(context.Background(), "a.md", "")
			body := `{"kind":"organize","operationId":"manual","organization":{"mode":"` + mode + `","scope":"selected","paths":["a.md"],"policy":{"tags":true,"vocabulary":["workshop"]}}}`
			w := proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs", body)
			if w.Code != 200 {
				t.Fatalf("start organizer: %d %s", w.Code, w.Body.String())
			}
			var response map[string]any
			json.Unmarshal(w.Body.Bytes(), &response)
			job := waitReview(t, app, response["id"].(string))
			if job["state"] != "completed" {
				t.Fatal(job)
			}
			after, _ := store.Read(context.Background(), "a.md", "")
			if mode == "preview" && string(after.Content) != string(before.Content) {
				t.Fatal("preview mutated note")
			}
			if mode == "apply" && !strings.Contains(string(after.Content), "workshop") {
				t.Fatal("applying run did not add tag")
			}
		})
	}
}

func TestOrganizerAutomaticBaselineDebounceAndIndependentSettings(t *testing.T) {
	var calls atomic.Int32
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); syntheticOrganizationAnswer(w, r) })
	body := `{"enabled":true,"generation":0,"policy":{"tags":true,"vocabulary":["workshop"]}}`
	if w := proxyCall(app, "alice", "POST", "/p/default/api/jev/organization", body); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	now := time.Now()
	app.scanJevAutomatic(context.Background(), now)
	if calls.Load() != 0 {
		t.Fatal("enablement backfilled existing notes")
	}
	store, _ := app.catalog.Access("alice", "default", "edit")
	doc, _ := store.Read(context.Background(), "a.md", "")
	if _, err := store.Save(context.Background(), doc.Path, append(doc.Content, []byte("\nA new detail.\n")...), doc.Revision, "alice"); err != nil {
		t.Fatal(err)
	}
	app.scanJevAutomatic(context.Background(), now)
	app.scanJevAutomatic(context.Background(), now.Add(time.Minute))
	if calls.Load() != 0 {
		t.Fatal("ignored debounce")
	}
	app.scanJevAutomatic(context.Background(), now.Add(3*time.Minute))
	app.jevState.mu.Lock()
	ids, err := app.jevState.jobIDs()
	app.jevState.mu.Unlock()
	if err != nil || len(ids) != 1 {
		t.Fatalf("automatic organizer did not schedule changed note: %v %v", ids, err)
	}
	if job := waitReview(t, app, ids[0]); job["state"] != "completed" {
		t.Fatal(job)
	}
	app.scanJevAutomatic(context.Background(), now.Add(5*time.Minute))
	app.scanJevAutomatic(context.Background(), now.Add(10*time.Minute))
	if calls.Load() != 1 {
		t.Fatalf("bookkeeping or unchanged note repeated inference: %d", calls.Load())
	}
	settings, err := app.loadJevNotebook("default")
	if err != nil || settings.AutoChecks {
		t.Fatal("organization enabled consistency", err)
	}
}

func organizerStart(t *testing.T, app *Service, body string) string {
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

func TestOrganizerDuplicateFullNotesAndUndo(t *testing.T) {
	for _, unique := range []bool{false, true} {
		t.Run(fmt.Sprint(unique), func(t *testing.T) {
			app := reviewFixture(t, organizerChoiceAnswer)
			store, _ := app.catalog.Access("alice", "default", "edit")
			content := "# Workshop\n" + strings.Repeat("Workshop begins at 09:00. ", 80)
			for _, p := range []string{"a.md", "b.md"} {
				d, _ := store.Read(context.Background(), p, "")
				text := content
				if p == "b.md" && unique {
					text += "\nUNIQUE_DETAIL: weekends begin at 10:00.\n"
				}
				if _, e := store.Save(context.Background(), p, []byte(text), d.Revision, "test"); e != nil {
					t.Fatal(e)
				}
			}
			id := organizerStart(t, app, `{"kind":"organize","operationId":"duplicates","organization":{"mode":"apply","scope":"all","policy":{"duplicates":true}}}`)
			j := waitReview(t, app, id)
			if j["state"] != "completed" {
				t.Fatal(j)
			}
			listing, e := store.ListTrash(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			want := 1
			if unique {
				want = 0
			}
			if len(listing.Entries) != want {
				t.Fatal("wrong duplicate result", listing, j)
			}
			if !unique {
				w := proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs/"+id+"/organization/undo", `{}`)
				if w.Code != 200 || strings.Contains(w.Body.String(), `"error"`) {
					t.Fatal(w.Code, w.Body.String())
				}
				if _, e := store.Read(context.Background(), "b.md", ""); e != nil {
					t.Fatal("undo did not restore", e)
				}
				// Every update gives the row a new xmin, as a rewrite gave the file a new mtime.
				written := func() string {
					var xmin string
					if e := app.db.Pool.QueryRow(context.Background(), `SELECT xmin::text FROM jev_records WHERE org = $1 AND name = $2`, app.db.Org, "notebooks/"+auth.Digest("default")+".json").Scan(&xmin); e != nil {
						t.Fatal(e)
					}
					return xmin
				}
				before := written()
				w = proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs/"+id+"/organization/undo", `{}`)
				if w.Code != 200 || strings.Contains(w.Body.String(), `"error"`) {
					t.Fatal("undo retry failed", w.Code, w.Body.String())
				}
				if written() != before {
					t.Fatal("undo retry rewrote unchanged suppression rules")
				}
			}
		})
	}
}

func TestOrganizerSelectionPolicyAndUndoSuppression(t *testing.T) {
	app := reviewFixture(t, organizerChoiceAnswer)
	body := `{"kind":"organize","operationId":"first","organization":{"mode":"apply","scope":"selected","paths":["a.md"],"policy":{"tags":true,"links":true,"vocabulary":["workshop"]}}}`
	id := organizerStart(t, app, body)
	j := waitReview(t, app, id)
	if j["state"] != "completed" {
		t.Fatal(j)
	}
	store, _ := app.catalog.Access("alice", "default", "read")
	other, _ := store.Read(context.Background(), "b.md", "")
	if strings.Contains(string(other.Content), "tags:") || strings.Contains(string(other.Content), "Related:") {
		t.Fatal("selection widened")
	}
	w := proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs/"+id+"/organization/undo", `{}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"error"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	app.jevState.close()
	if e := app.initState(); e != nil {
		t.Fatal(e)
	}
	if _, e := store.Save(context.Background(), "unrelated.md", []byte("An unrelated note."), "", "alice"); e != nil {
		t.Fatal(e)
	}
	id = organizerStart(t, app, strings.Replace(body, `"first"`, `"suppressed"`, 1))
	j = waitReview(t, app, id)
	if j["state"] != "completed" {
		t.Fatal(j)
	}
	doc, _ := store.Read(context.Background(), "a.md", "")
	if strings.Contains(string(doc.Content), "tags:") || strings.Contains(string(doc.Content), "Related:") {
		t.Fatal("undo suppression was ignored")
	}
}

func TestOrganizerChangedBacklinkDuringInferenceBlocksCleanup(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		organizerChoiceAnswer(w, r)
	})
	store, _ := app.catalog.Access("alice", "default", "owner")
	ctx := context.Background()
	for _, p := range []string{"a.md", "b.md"} {
		d, _ := store.Read(ctx, p, "")
		if _, e := store.Save(ctx, p, []byte("# Shared\nWorkshop begins at 09:00.\n"), d.Revision, "alice"); e != nil {
			t.Fatal(e)
		}
	}
	back, e := store.Save(ctx, "links.md", []byte("[Workshop](b.md)\n"), "", "alice")
	if e != nil {
		t.Fatal(e)
	}
	id := organizerStart(t, app, `{"kind":"organize","operationId":"backlink","organization":{"mode":"apply","scope":"all","policy":{"duplicates":true}}}`)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("inference did not start")
	}
	_, e = store.Save(ctx, "links.md", []byte("[Workshop](b.md)\nUser edited while organization ran.\n"), back.Revision, "alice")
	close(release)
	if e != nil {
		t.Fatal(e)
	}
	waitReview(t, app, id)
	if _, e := store.Read(ctx, "b.md", ""); e != nil {
		t.Fatal("stale backlink plan trashed duplicate", e)
	}
}

func TestOrganizerCommitReceiptRecoveryAfterRestart(t *testing.T) {
	var calls atomic.Int32
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); syntheticOrganizationAnswer(w, r) })
	input := JobInput{Kind: "organize", OperationID: "receipt-recovery", Organization: &organizationInput{Mode: "apply", Scope: "selected", Paths: []string{"a.md"}, Policy: organizationPolicy{Tags: true, Vocabulary: []string{"workshop"}}}}
	job, e := app.createOrganizationJob(context.Background(), "alice", "default", input, false, nil)
	if e != nil {
		t.Fatal(e)
	}
	store, _ := app.catalog.Access("alice", "default", "owner")
	a := job.Work[0].Organization
	if _, _, e = app.prepareOrganizationAction(context.Background(), store, job, a); e != nil {
		t.Fatal(e)
	}
	a.Request.Tags = []string{"workshop"}
	a.Status = "eligible"
	if e = app.saveJevJob(&job); e != nil {
		t.Fatal(e)
	}
	account, _ := app.catalog.Account("alice")
	ctx := attribution.With(context.Background(), account.Identity())
	applied, e := store.Organize(ctx, *a.Request, true)
	if e != nil {
		t.Fatal(e)
	}
	// Emulate the crash window after Git committed but before the result/checkpoint.
	app.jevState.close()
	if e = app.initState(); e != nil {
		t.Fatal(e)
	}
	app.runOrganizationJob(context.Background(), job)
	got := waitReview(t, app, job.ID)
	if got["state"] != "completed" {
		t.Fatal(got)
	}
	status, _ := store.Status(ctx)
	if status.Revision != applied.Revision || calls.Load() != 0 {
		t.Fatal("receipt recovery repeated inference or commit")
	}
	w := proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs/"+job.ID+"/organization/undo", `{}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"error"`) {
		t.Fatal(w.Body.String())
	}
}

func TestOrganizerFolderMovePolicyAndScope(t *testing.T) {
	app := reviewFixture(t, organizerChoiceAnswer)
	ctx := context.Background()
	store, _ := app.catalog.Access("alice", "default", "owner")
	if _, e := store.Save(ctx, "dest/index.md", []byte("# Workshops\nWorkshop planning.\n"), "", "alice"); e != nil {
		t.Fatal(e)
	}
	id := organizerStart(t, app, `{"kind":"organize","operationId":"move","organization":{"mode":"apply","scope":"selected","paths":["a.md"],"policy":{"moves":true,"folders":["dest"]}}}`)
	got := waitReview(t, app, id)
	if got["state"] != "completed" {
		t.Fatal(got)
	}
	d, e := store.Read(ctx, "dest/a.md", "")
	if e != nil || strings.Contains(string(d.Content), "tags:") {
		t.Fatal("move policy failed", e, got)
	}
	if _, e := store.Read(ctx, "b.md", ""); e != nil {
		t.Fatal("moved outside selection")
	}
}

func TestOrganizerDuplicateGroupAndIncompleteEvidence(t *testing.T) {
	for _, large := range []bool{false, true} {
		t.Run(fmt.Sprint(large), func(t *testing.T) {
			app := reviewFixture(t, organizerChoiceAnswer)
			ctx := context.Background()
			store, _ := app.catalog.Access("alice", "default", "owner")
			content := "# Workshop\nWorkshop begins at 09:00.\n"
			if large {
				content += strings.Repeat("Workshop detail. ", 2000)
			}
			for _, p := range []string{"a.md", "b.md", "c.md"} {
				d, _ := store.Read(ctx, p, "")
				if _, e := store.Save(ctx, p, []byte(content), d.Revision, "alice"); e != nil {
					t.Fatal(e)
				}
			}
			id := organizerStart(t, app, `{"kind":"organize","operationId":"group","organization":{"mode":"apply","scope":"all","policy":{"duplicates":true}}}`)
			got := waitReview(t, app, id)
			if got["state"] != "completed" {
				t.Fatal(got)
			}
			trash, e := store.ListTrash(ctx)
			want := 2
			if large {
				want = 0
			}
			if e != nil || len(trash.Entries) != want {
				t.Fatal(trash, e, got)
			}
			if _, e := store.Read(ctx, "a.md", ""); e != nil {
				t.Fatal("last retained duplicate was lost")
			}
		})
	}
}

func TestOrganizerLargeVocabularyAdvancesInBoundedJudgments(t *testing.T) {
	app := reviewFixture(t, syntheticOrganizationAnswer)
	tags := []string{}
	for i := 0; i < 25; i++ {
		tags = append(tags, fmt.Sprintf("tag-%02d", i))
	}
	body, _ := json.Marshal(JobInput{Kind: "organize", OperationID: "vocabulary", Organization: &organizationInput{Mode: "apply", Scope: "selected", Paths: []string{"a.md"}, Policy: organizationPolicy{Tags: true, Vocabulary: tags}}})
	id := organizerStart(t, app, string(body))
	j := waitReview(t, app, id)
	if j["state"] != "completed" {
		t.Fatal("large vocabulary stalled", j)
	}
}

func TestOrganizerSettingsCannotOverwriteConcurrentConsistencyDisable(t *testing.T) {
	app := reviewFixture(t, syntheticOrganizationAnswer)
	if w := proxyCall(app, "alice", "POST", "/p/default/api/jev/settings", `{"autoChecks":true}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	stale, e := app.loadJevNotebook("default")
	if e != nil {
		t.Fatal(e)
	}
	if w := proxyCall(app, "alice", "POST", "/p/default/api/jev/settings", `{"autoChecks":false}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	stale.Organization = &organizationSettings{Suppressed: map[string]string{"synthetic": "dismissed"}}
	if _, e = app.updateOrganization("default", func(o *organizationSettings) error { *o = *stale.Organization; return nil }); e != nil {
		t.Fatal(e)
	}
	current, e := app.loadJevNotebook("default")
	if e != nil || current.AutoChecks {
		t.Fatal("organization settings revived disabled consistency", e)
	}
}

func TestOrganizerAutomaticMixedOutcomesDoNotRepeatPaidWork(t *testing.T) {
	var calls atomic.Int32
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), `"path":"b.md"`) {
			json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": map[string]any{"tag0": map[string]any{"type": "noul", "noul": .2}, "none": map[string]any{"type": "noul", "noul": .8}}, "usage": map[string]int{"input_tokens": 30, "output_tokens": 10}})
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		syntheticOrganizationAnswer(w, r)
	})
	if w := proxyCall(app, "alice", "POST", "/p/default/api/jev/organization", `{"enabled":true,"generation":0,"policy":{"tags":true,"vocabulary":["workshop"]}}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	store, _ := app.catalog.Access("alice", "default", "owner")
	for _, p := range []string{"a.md", "b.md"} {
		d, _ := store.Read(context.Background(), p, "")
		if _, e := store.Save(context.Background(), p, append(d.Content, []byte("\nChanged.\n")...), d.Revision, "alice"); e != nil {
			t.Fatal(e)
		}
	}
	now := time.Now()
	app.scanOrganizationAutomatic(context.Background(), now)
	app.scanOrganizationAutomatic(context.Background(), now.Add(3*time.Minute))
	app.jevState.mu.Lock()
	ids, _ := app.jevState.jobIDs()
	app.jevState.mu.Unlock()
	if len(ids) != 1 {
		t.Fatal(ids)
	}
	waitReview(t, app, ids[0])
	app.scanOrganizationAutomatic(context.Background(), now.Add(6*time.Minute))
	app.scanOrganizationAutomatic(context.Background(), now.Add(9*time.Minute))
	app.jevState.mu.Lock()
	ids, _ = app.jevState.jobIDs()
	app.jevState.mu.Unlock()
	for _, id := range ids {
		waitReview(t, app, id)
	}
	if calls.Load() != 2 {
		t.Fatal("organizer commits retriggered uncertain evidence", calls.Load())
	}
}

func TestOrganizerCompletionWaitsForOrganizationSettingsTransaction(t *testing.T) {
	app := reviewFixture(t, syntheticOrganizationAnswer)
	if w := proxyCall(app, "alice", "POST", "/p/default/api/jev/organization", `{"enabled":true,"generation":0,"policy":{"tags":true,"vocabulary":["workshop"]}}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	input := JobInput{Kind: "organize", OperationID: "completion-race", Organization: &organizationInput{Mode: "apply", Scope: "all", Policy: organizationPolicy{Tags: true, Vocabulary: []string{"workshop"}}}}
	job, e := app.createOrganizationJob(context.Background(), "alice", "default", input, true, map[string]bool{"a.md": true})
	if e != nil {
		t.Fatal(e)
	}
	// Resume the last durable cursor while a suppression update holds its normal
	// read/modify/write boundary. Completion must not overtake that transaction.
	job.Cursor = len(job.Work)
	app.jevState.organizationMu.Lock()
	done := make(chan struct{})
	go func() { app.runOrganizationJob(context.Background(), job); close(done) }()
	select {
	case <-done:
		app.jevState.organizationMu.Unlock()
		t.Fatal("completion bypassed the concurrent organization settings transaction")
	case <-time.After(time.Second):
	}
	_, e = app.updateOrganization("default", func(o *organizationSettings) error {
		o.Suppressed = map[string]string{"synthetic": "dismissed"}
		return nil
	})
	app.jevState.organizationMu.Unlock()
	<-done
	if e != nil {
		t.Fatal(e)
	}
	settings, e := app.loadJevNotebook("default")
	if e != nil || settings.Organization.Suppressed["synthetic"] != "dismissed" {
		t.Fatal("completion lost suppression", e)
	}
}
