package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/workspace"
)

func TestCheckSuiteRetainsEveryFailure(t *testing.T) {
	app, _ := fixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"rows":[[false]]}`)
	}))
	defer server.Close()
	var err error
	app.dataConnections, err = connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "metrics", Kind: "http", Endpoint: server.URL, Operations: []connectors.Operation{{Name: "read", Path: "/", ReadOnly: true}}}}})
	if err != nil {
		t.Fatal(err)
	}
	spec := automation.Spec{ID: "contracts", Kind: "checks", Actor: catalog.Bootstrap, Project: "default", Connections: []string{"metrics"}}
	for _, name := range []string{"freshness", "uniqueness"} {
		spec.Checks = append(spec.Checks, automation.Check{Name: name, Call: automation.Call{Connection: "metrics", Operation: "read"}, Assertions: []automation.Assertion{{Pointer: "/rows/0/0", Equals: json.RawMessage(`true`)}}})
	}
	run := automation.Run{ID: automation.Hash("all-failures"), Spec: spec, DryRun: true}
	err = app.automationrun.RunAutomation(t.Context(), &run, func() error { return nil })
	if err == nil || len(run.Receipts) != 2 || !strings.Contains(run.Result, "freshness") || !strings.Contains(run.Result, "uniqueness") {
		t.Fatalf("every failure must be retained: receipts=%d report=%q error=%v", len(run.Receipts), run.Result, err)
	}
}

type repairTestFixture struct {
	app              *Server
	store            *workspace.Store
	spec             automation.Spec
	before           workspace.Document
	answer           string
	onModel          func()
	onRequest        func(*http.Request)
	calls, watermark int
	available, guard bool
}

func newRepairTest(t *testing.T) *repairTestFixture {
	t.Helper()
	f := &repairTestFixture{available: true, guard: true, watermark: 1, answer: `{"summary":"No proven repair.","sql":{}}`}
	f.app, _ = runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		if f.onRequest != nil {
			f.onRequest(r)
		}
		if f.onModel != nil {
			f.onModel()
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": f.answer}, "finish_reason": "stop"}}})
	})
	if err := f.app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !f.available {
			http.Error(w, "unavailable", 503)
			return
		}
		var args map[string]any
		json.NewDecoder(r.Body).Decode(&args)
		sql := fmt.Sprint(args["sql"])
		var value any = 1
		if strings.Contains(sql, "active = true") {
			value = 2
		}
		if strings.Contains(sql, "watermark") {
			value = f.watermark
		}
		if strings.Contains(sql, "guard") {
			value = f.guard
		}
		json.NewEncoder(w).Encode(map[string]any{"rows": [][]any{{value}}})
	}))
	t.Cleanup(server.Close)
	var err error
	f.app.dataConnections, err = connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "metrics", Kind: "http", Endpoint: server.URL, ExternalProcessing: true, Operations: []connectors.Operation{{Name: "query", Method: "POST", Body: true, Path: "/", ReadOnly: true, InputSchema: json.RawMessage(`{"type":"object","properties":{"sql":{"type":"string"}},"required":["sql"],"additionalProperties":false}`)}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "metrics", Users: map[string][]string{"alice": {"query"}}}); err != nil {
		t.Fatal(err)
	}
	f.store, _ = f.app.catalog.Access("alice", "default", "edit")
	f.before, err = f.store.Save(t.Context(), "recipe.md", []byte("# Active records\n\n```sql count\nSELECT count(*) AS n FROM public.events WHERE active = false\n```\n"), "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	call := func(sql string) automation.Call {
		return automation.Call{Connection: "metrics", Operation: "query", Arguments: map[string]any{"sql": sql}}
	}
	f.spec = automation.Spec{ID: "repair", Kind: "checks", Actor: "alice", Project: "default", Connections: []string{"metrics"}, Recipe: "recipe.md", ExternalProcessing: true, Repair: &automation.RepairPolicy{CooldownSeconds: 60, Evidence: []automation.Call{call("SELECT watermark FROM public.events")}}, Checks: []automation.Check{{Name: "active-count", Block: "count", Call: call(""), Reference: ptrCall(call("SELECT count(*) AS n FROM public.events WHERE active = true"))}}}
	return f
}
func ptrCall(c automation.Call) *automation.Call { return &c }

func TestRepairReportsInvestigationBeforeModelCall(t *testing.T) {
	f := newRepairTest(t)
	r := automation.Run{ID: automation.Hash("investigation-status"), Spec: f.spec, State: "running"}
	observed := false
	err := f.app.automationrun.RunAutomation(t.Context(), &r, func() error {
		if r.GenerationStarted && f.calls == 0 {
			observed = true
			status := runStatus(r)
			if status.Checks == nil || status.Checks.State != "investigating" || status.Checks.Failed != 1 {
				t.Errorf("pending diagnosis misrepresented: %+v", status.Checks)
			}
			return fmt.Errorf("stop after investigation checkpoint")
		}
		return nil
	})
	if err == nil || !observed || f.calls != 0 {
		t.Fatal("investigation checkpoint not observed", err, observed, f.calls)
	}
}

func TestRepairRequestsNativeJSONWhenSupported(t *testing.T) {
	f := newRepairTest(t)
	connection := f.app.chat.Config.Connections[0]
	connection.Capabilities.JSONOutput = true
	setChatConnection(t, f.app, connection)
	f.onRequest = func(r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if string(body["response_format"]) != `{"type":"json_object"}` {
			t.Errorf("repair omitted native JSON output: %s", body["response_format"])
		}
	}
	if _, err := f.run(t, "native-json", false); err == nil || f.calls != 1 {
		t.Fatal("expected one diagnosis with unresolved failure", err, f.calls)
	}
}

func TestRepairRetainsRejectedAnswerOnlyInAdminResult(t *testing.T) {
	f := newRepairTest(t)
	f.answer = "Private diagnostic evidence: this is not JSON."
	r, err := f.run(t, "invalid-answer", false)
	if err == nil || r.CheckReport.State != "repair_failed" || !strings.Contains(r.Result, f.answer) || !strings.Contains(r.Result, "invalid character") {
		t.Fatalf("missing original failure diagnostics: %v %+v %q", err, r.CheckReport, r.Result)
	}
	public, _ := json.Marshal(runStatus(r))
	if strings.Contains(string(public), "Private diagnostic evidence") {
		t.Fatal("raw answer leaked into public status")
	}
	got, err := f.store.Read(t.Context(), f.before.Path, "")
	if err != nil || string(got.Content) != string(f.before.Content) {
		t.Fatal("invalid answer changed recipe", err)
	}
}

func TestRepairSendsOnlyFailedQueriesAndDependencies(t *testing.T) {
	f := newRepairTest(t)
	unused := "SELECT unrelated_passing_metric FROM public.untouched_source"
	content := string(f.before.Content) + "\nThe active count measures distinct active records.\n\n```sql unused\n" + unused + "\n```\n"
	if _, err := f.store.Save(t.Context(), f.before.Path, []byte(content), f.before.Revision, "alice"); err != nil {
		t.Fatal(err)
	}
	call := func(sql string) automation.Call {
		return automation.Call{Connection: "metrics", Operation: "query", Arguments: map[string]any{"sql": sql}}
	}
	f.spec.Checks[0].DependsOn = []string{"freshness"}
	f.spec.Checks = append(f.spec.Checks,
		automation.Check{Name: "freshness", Call: call("SELECT ready FROM public.guard"), Assertions: []automation.Assertion{{Pointer: "/rows/0/0", Equals: json.RawMessage(`true`)}}},
		automation.Check{Name: "unrelated", Block: "unused", Call: call(""), Reference: ptrCall(call(unused))},
	)
	f.onRequest = func(r *http.Request) {
		var body struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		var prompt string
		for _, m := range body.Messages {
			prompt += m.Content
		}
		if strings.Contains(prompt, "untouched_source") {
			t.Error("passing SQL was sent to the model")
		}
		for _, required := range []string{"active = false", "active = true", "FROM public.guard", "SELECT watermark", "distinct active records"} {
			if !strings.Contains(prompt, required) {
				t.Errorf("required repair evidence missing: %s", required)
			}
		}
	}
	if _, err := f.run(t, "focused-context", false); err == nil || f.calls != 1 {
		t.Fatal("expected one diagnosis with unresolved failure", err, f.calls)
	}
}

func (f *repairTestFixture) run(t *testing.T, id string, preview bool) (automation.Run, error) {
	t.Helper()
	r := automation.Run{ID: automation.Hash(id), Spec: f.spec, DryRun: preview}
	err := f.app.automationrun.RunAutomation(t.Context(), &r, func() error { return nil })
	return r, err
}

func TestRepairSuppressesUnchangedEvidenceAcrossRestart(t *testing.T) {
	f := newRepairTest(t)
	r, err := f.run(t, "first", false)
	if err == nil || f.calls != 1 || r.CheckReport.State != "waiting_upstream" {
		t.Fatal(err, f.calls, r.CheckReport)
	}
	f.app.automations.Queue, err = automation.Open(f.app.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.run(t, "unchanged", false); err == nil || f.calls != 1 {
		t.Fatal("unchanged failure repurchased AI", err, f.calls)
	}
	f.watermark++
	if _, err = f.run(t, "cooldown", false); err == nil || f.calls != 1 {
		t.Fatal("cooldown not enforced", err, f.calls)
	}
	memo, err := f.app.automations.Queue.RepairMemo(f.spec)
	if err != nil {
		t.Fatal(err)
	}
	memo.AttemptedAt = time.Now().Add(-2 * time.Minute)
	if err = f.app.automations.Queue.SaveRepairMemo(f.spec, memo); err != nil {
		t.Fatal(err)
	}
	if _, err = f.run(t, "changed", false); err == nil || f.calls != 2 {
		t.Fatal("new evidence did not reopen investigation", err, f.calls)
	}
}

func TestRepairIgnoresUnrelatedPassingMetricChanges(t *testing.T) {
	f := newRepairTest(t)
	f.spec.Repair.Evidence = nil
	call := automation.Call{Connection: "metrics", Operation: "query", Arguments: map[string]any{"sql": "SELECT watermark FROM public.events"}}
	f.spec.Checks = append(f.spec.Checks, automation.Check{Name: "unrelated", Call: call, Reference: &call})
	if _, err := f.run(t, "first", false); err == nil || f.calls != 1 {
		t.Fatal("initial investigation", err, f.calls)
	}
	memo, err := f.app.automations.Queue.RepairMemo(f.spec)
	if err != nil {
		t.Fatal(err)
	}
	memo.AttemptedAt = time.Now().Add(-2 * time.Minute)
	if err = f.app.automations.Queue.SaveRepairMemo(f.spec, memo); err != nil {
		t.Fatal(err)
	}
	f.watermark++
	if _, err = f.run(t, "unrelated-change", false); err == nil || f.calls != 1 {
		t.Fatal("passing metric repurchased investigation", err, f.calls)
	}
}

func TestRepairBoundariesPreserveRecipe(t *testing.T) {
	for _, test := range []struct {
		name, answer string
		setup        func(*repairTestFixture)
	}{
		{"wrong-result", `{"summary":"Wrong fix","sql":{"count":"SELECT count(*) AS n FROM public.events WHERE active = false"}}`, nil},
		{"unapproved-block", `{"summary":"Bypass","sql":{"reference":"SELECT n FROM public.events"}}`, nil},
		{"constant", `{"summary":"Bypass","sql":{"count":"SELECT 2 AS n"}}`, nil},
		{"template-escape", `{"summary":"Bypass","sql":{"count":"SELECT n FROM public.events) SELECT true --"}}`, nil},
		{"definition-edit", `{"summary":"Bypass","sql":{},"assertions":[]}`, nil},
		{"upstream", `{"summary":"Fix","sql":{"count":"SELECT count(*) AS n FROM public.events WHERE active = true"}}`, func(f *repairTestFixture) { f.available = false }},
		{"dependency", `{"summary":"Fix","sql":{"count":"SELECT count(*) AS n FROM public.events WHERE active = true"}}`, func(f *repairTestFixture) {
			f.guard = false
			f.spec.Checks[0].DependsOn = []string{"freshness"}
			f.spec.Checks = append(f.spec.Checks, automation.Check{Name: "freshness", Call: automation.Call{Connection: "metrics", Operation: "query", Arguments: map[string]any{"sql": "SELECT guard FROM public.events"}}, Assertions: []automation.Assertion{{Pointer: "/rows/0/0", Equals: json.RawMessage(`true`)}}})
		}},
		{"changing-evidence", `{"summary":"Fix","sql":{"count":"SELECT count(*) AS n FROM public.events WHERE active = true"}}`, func(f *repairTestFixture) { f.onModel = func() { f.watermark++ } }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRepairTest(t)
			f.answer = test.answer
			if test.setup != nil {
				test.setup(f)
			}
			if _, err := f.run(t, test.name, false); err == nil {
				t.Fatal("invalid repair accepted")
			}
			doc, err := f.store.Read(t.Context(), f.before.Path, "")
			if err != nil || doc.Revision != f.before.Revision {
				t.Fatal("original changed", err)
			}
			if test.name == "upstream" && f.calls != 0 {
				t.Fatal("outage triggered AI")
			}
		})
	}
}

func TestRepairPreviewAndConcurrentEdit(t *testing.T) {
	for _, mode := range []string{"preview", "concurrent", "revoke"} {
		t.Run(mode, func(t *testing.T) {
			f := newRepairTest(t)
			f.answer = `{"summary":"Fix","sql":{"count":"SELECT count(*) AS n FROM public.events WHERE active = true"}}`
			if mode == "concurrent" {
				f.onModel = func() {
					if _, err := f.store.Save(t.Context(), f.before.Path, []byte("Human edit"), f.before.Revision, "alice"); err != nil {
						t.Error(err)
					}
				}
			}
			if mode == "revoke" {
				f.onModel = func() {
					if err := f.app.catalog.Member(catalog.Bootstrap, "default", "alice", "read"); err != nil {
						t.Error(err)
					}
				}
			}
			r, err := f.run(t, mode, mode == "preview")
			if mode == "preview" && (err != nil || r.CheckReport.State != "repair_validated") {
				t.Fatal(err, r.CheckReport)
			}
			if mode != "preview" && err == nil {
				t.Fatal("publication bypassed concurrent changes")
			}
			doc, readErr := f.store.Read(t.Context(), f.before.Path, "")
			if readErr != nil {
				t.Fatal(readErr)
			}
			if mode == "concurrent" {
				if string(doc.Content) != "Human edit" {
					t.Fatal("concurrent edit overwritten")
				}
			} else if doc.Revision != f.before.Revision {
				t.Fatal("original changed")
			}
			if mode == "preview" {
				memo, e := f.app.automations.Queue.RepairMemo(f.spec)
				if e != nil || !memo.AttemptedAt.IsZero() {
					t.Fatal("preview changed paid-attempt memo", e)
				}
			}
		})
	}
}

func TestRepairImprovesIndependentQueryAndRetainsUpstreamFailure(t *testing.T) {
	f := newRepairTest(t)
	f.guard = false
	f.spec.Checks = append(f.spec.Checks, automation.Check{Name: "upstream", Call: automation.Call{Connection: "metrics", Operation: "query", Arguments: map[string]any{"sql": "SELECT guard FROM public.events"}}, Assertions: []automation.Assertion{{Pointer: "/rows/0/0", Equals: json.RawMessage(`true`)}}})
	f.answer = `{"summary":"Repaired count; upstream guard still fails.","sql":{"count":"SELECT count(*) AS n FROM public.events WHERE active = true"}}`
	r, err := f.run(t, "partial", false)
	if err == nil || r.CheckReport.State != "waiting_upstream" || r.CheckReport.Checks[0].State != "passed" || len(r.CheckReport.Original) != 2 {
		t.Fatal("upstream failure hidden", r.CheckReport, err)
	}
	doc, e := f.store.Read(t.Context(), f.before.Path, "")
	if e != nil || doc.Revision == f.before.Revision {
		t.Fatal("independent repair was not committed", e)
	}
	if err = f.app.automations.Definitions.SetFiles([]automation.Spec{f.spec}); err != nil {
		t.Fatal(err)
	}
	health, err := f.app.recipeHealth(f.spec.Project, f.spec.Recipe, workspace.ContentHash(doc.Content))
	if err != nil || health == nil || health.State != "waiting_upstream" || health.Checks["repair/upstream"] != "failed" {
		t.Fatal(health, err)
	}
	health, err = f.app.recipeHealth(f.spec.Project, f.spec.Recipe, "different-revision")
	if err != nil || health.State != "unverified" {
		t.Fatal("old checks qualified a new revision", health, err)
	}
}

func TestRepairResumesSavedPublicationWithoutAnotherModelCall(t *testing.T) {
	f := newRepairTest(t)
	f.answer = `{"summary":"Fix","sql":{"count":"SELECT count(*) AS n FROM public.events WHERE active = true"}}`
	r := automation.Run{ID: automation.Hash("interrupted-publication"), Spec: f.spec}
	var saved automation.Run
	err := f.app.automationrun.RunAutomation(t.Context(), &r, func() error {
		if r.Publication != nil && r.Publication.Content != "" {
			raw, _ := json.Marshal(r)
			json.Unmarshal(raw, &saved)
			return fmt.Errorf("injected stop before publication")
		}
		return nil
	})
	if err == nil || saved.Publication == nil || f.calls != 1 {
		t.Fatal("publication checkpoint was not reached", err, f.calls)
	}
	if err = f.app.automationrun.RunAutomation(t.Context(), &saved, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 || saved.CheckReport.State != "repaired" {
		t.Fatal("resume repurchased generation", f.calls, saved.CheckReport)
	}
	doc, err := f.store.Read(t.Context(), f.before.Path, "")
	if err != nil || doc.Revision == f.before.Revision {
		t.Fatal("recovered repair not committed", err)
	}
}

func TestRepairChecksContinueWhenAIWritingIsDisabled(t *testing.T) {
	f := newRepairTest(t)
	settings, _ := f.app.catalog.AISettings()
	settings.Writing = false
	if _, err := f.app.catalog.SetAISettings(catalog.Bootstrap, settings); err != nil {
		t.Fatal(err)
	}
	r, err := f.run(t, "disabled", false)
	if err == nil || f.calls != 0 || len(r.CheckReport.Checks) != 1 || len(r.Receipts) < 2 {
		t.Fatal("AI switch disabled checks or called model", r, err)
	}
}

func TestRepairNeverRegressesAPassingCheck(t *testing.T) {
	f := newRepairTest(t)
	f.spec.Checks = append(f.spec.Checks, automation.Check{Name: "held-out-case", Block: "count", Call: f.spec.Checks[0].Call, Assertions: []automation.Assertion{{Pointer: "/rows/0/0", Equals: json.RawMessage(`1`)}}})
	f.answer = `{"summary":"Bad fix that passes only the first case","sql":{"count":"SELECT count(*) AS n FROM public.events WHERE active = true"}}`
	r, err := f.run(t, "held-out", false)
	if err == nil || r.CheckReport.State != "repair_failed" {
		t.Fatal("regression accepted", r.CheckReport, err)
	}
	doc, err := f.store.Read(t.Context(), f.before.Path, "")
	if err != nil || doc.Revision != f.before.Revision {
		t.Fatal("regression committed", err)
	}
}

func TestRepairRollbackRequiresUnchangedPublicationAndEvidence(t *testing.T) {
	for _, edited := range []bool{false, true} {
		t.Run(fmt.Sprint(edited), func(t *testing.T) {
			f := newRepairTest(t)
			prior := strings.Replace(string(f.before.Content), "active = false", "active = true", 1)
			evidenceRun := automation.Run{Spec: f.spec}
			ctx, err := f.app.automationContext(t.Context(), f.spec)
			if err != nil {
				t.Fatal(err)
			}
			evidence, _, err := f.app.automationrun.RepairEvidence(ctx, &evidenceRun, nil)
			if err != nil {
				t.Fatal(err)
			}
			memo := automation.RepairMemo{Previous: prior, PublishedHash: workspace.ContentHash(f.before.Content), EvidenceHash: evidence, AttemptFingerprint: "before", AttemptedAt: time.Now()}
			if err = f.app.automations.Queue.SaveRepairMemo(f.spec, memo); err != nil {
				t.Fatal(err)
			}
			if edited {
				if _, err = f.store.Save(t.Context(), f.before.Path, append(f.before.Content, []byte("\nHuman context\n")...), f.before.Revision, "alice"); err != nil {
					t.Fatal(err)
				}
			}
			r, err := f.run(t, "rollback", false)
			if !edited && (err != nil || r.CheckReport.State != "rolled_back") {
				t.Fatal(r.CheckReport, err)
			}
			doc, err := f.store.Read(t.Context(), f.before.Path, "")
			if err != nil {
				t.Fatal(err)
			}
			if edited && !strings.Contains(string(doc.Content), "Human context") {
				t.Fatal("rollback overwrote edit")
			}
			if !edited && string(doc.Content) != prior {
				t.Fatal("proven rollback not committed")
			}
			if f.calls != 0 {
				t.Fatal("rollback bought AI")
			}
		})
	}
}

func TestCheckSuiteRepairsOnlySQLAndSkipsHealthyModelCalls(t *testing.T) {
	aiCalls := 0
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		aiCalls++
		answer := `{"summary":"Restore the documented active-record filter.","sql":{"count":"SELECT count(*) AS n FROM public.events WHERE active = true"}}`
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": answer}, "finish_reason": "stop"}}})
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var args map[string]any
		json.NewDecoder(r.Body).Decode(&args)
		sql := fmt.Sprint(args["sql"])
		n := 1
		if strings.Contains(sql, "active = true") {
			n = 2
		}
		json.NewEncoder(w).Encode(map[string]any{"rows": [][]int{{n}}})
	}))
	defer server.Close()
	var err error
	app.dataConnections, err = connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "metrics", Kind: "http", Endpoint: server.URL, ExternalProcessing: true, Operations: []connectors.Operation{{Name: "query", Method: "POST", Body: true, Path: "/", ReadOnly: true, InputSchema: json.RawMessage(`{"type":"object","properties":{"sql":{"type":"string"}},"required":["sql"],"additionalProperties":false}`)}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "metrics", Users: map[string][]string{"alice": {"query"}}}); err != nil {
		t.Fatal(err)
	}
	store, _ := app.catalog.Access("alice", "default", "edit")
	const original = "---\ntags: [recipe]\n---\n# Active records\n\nCount active records; preserve this definition.\n\n```sql count\nSELECT count(*) AS n FROM public.events WHERE active = false\n```\n"
	before, err := store.Save(t.Context(), "recipe.md", []byte(original), "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	var spec automation.Spec
	err = json.Unmarshal([]byte(`{"id":"repair","kind":"checks","actor":"alice","project":"default","connections":["metrics"],"recipe":"recipe.md","externalProcessing":true,"repair":{"cooldownSeconds":3600},"checks":[{"name":"active-count","block":"count","call":{"connection":"metrics","operation":"query","arguments":{}},"reference":{"connection":"metrics","operation":"query","arguments":{"sql":"SELECT count(*) AS n FROM public.events WHERE active = true"}}}]}`), &spec)
	if err != nil {
		t.Fatal(err)
	}
	run := automation.Run{ID: automation.Hash("repair"), Spec: spec}
	if err = app.automationrun.RunAutomation(t.Context(), &run, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	after, err := store.Read(t.Context(), "recipe.md", "")
	if err != nil || after.Revision == before.Revision || string(after.Content) != strings.Replace(original, "active = false", "active = true", 1) || aiCalls != 1 {
		t.Fatalf("validated SQL-only repair not published: calls=%d content=%q error=%v", aiCalls, after.Content, err)
	}
	run = automation.Run{ID: automation.Hash("healthy"), Spec: spec}
	if err = app.automationrun.RunAutomation(t.Context(), &run, func() error { return nil }); err != nil || aiCalls != 1 {
		t.Fatalf("healthy checks bought generation: calls=%d error=%v", aiCalls, err)
	}
}
