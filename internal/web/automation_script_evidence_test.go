package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
)

func TestScriptTransientReadRetryReusesCompletedReceipts(t *testing.T) {
	code := "def main(host):\n    host.call('first', 'connectors.read', connection='api', operation='read', arguments={})\n    host.call('second', 'connectors.read', connection='api', operation='read', arguments={})\n    return {'checked': True}\n"
	app, e := scriptFixture(t, code, []string{"connectors.read"}, map[string][]string{"api": {"read"}}, nil)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 2 {
			http.Error(w, "synthetic unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	bindScriptConnector(t, app, e, upstream.URL)
	e.run.Spec.Retry = &automation.RetryPolicy{MaxAttempts: 2, DelaySeconds: 5}
	if err := refreshScriptContract(t, app, e); err != nil {
		t.Fatal(err)
	}
	if err := app.runScript(e.ctx, e.run, e.save); !errors.Is(err, connectors.ErrUnavailable) {
		t.Fatalf("lost transient connector cause at the Python boundary: %v", err)
	}
	if e.run.Script.Steps["first"].State != "completed" || e.run.Script.Steps["second"].State != "failed" {
		t.Fatal("read receipts did not preserve the failed boundary")
	}
	if err := app.runScript(e.ctx, e.run, e.save); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || !e.run.Script.Complete {
		t.Fatal("retry repeated a completed read or failed to complete", calls.Load())
	}
}

func TestScriptIncidentUsesExistingOwnerAndPreviewSuppression(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"incidents.report"}, nil, nil)
	e.run.Spec.Alert = &automation.AlertPolicy{ActiveAfter: 2, ResolvedAfter: 2, NotifyResolved: true}
	if err := refreshScriptContract(t, app, e); err != nil {
		t.Fatal(err)
	}
	bad := map[string]any{"active": true, "severity": "warning", "summary": "two probes failed"}
	e.run.DryRun = true
	if _, err := hostStep(t, app, e, "finding", "incidents.report", bad); err != nil {
		t.Fatal(err)
	}
	if e.run.Alert == nil || e.run.Alert.Notify {
		t.Fatal("preview changed incident threshold")
	}
	e.run.DryRun = false
	e.run.ID = automation.Hash("first-real")
	e.run.Script.Steps = map[string]*automation.ScriptStep{}
	if _, err := hostStep(t, app, e, "finding", "incidents.report", bad); err != nil {
		t.Fatal(err)
	}
	if e.run.Alert.Notify {
		t.Fatal("preview counted as real observation")
	}
	e.run.ID = automation.Hash("second-real")
	e.run.Script.Steps = map[string]*automation.ScriptStep{}
	if _, err := hostStep(t, app, e, "finding", "incidents.report", bad); err != nil {
		t.Fatal(err)
	}
	if !e.run.Alert.Notify || e.run.Answer == "" {
		t.Fatal("finding bypassed notification owner")
	}
	if _, err := hostStep(t, app, e, "different", "incidents.report", bad); err == nil {
		t.Fatal("multiple findings allowed for one run")
	}
}

func refreshScriptContract(t *testing.T, app *Server, e *scriptExecution) error {
	t.Helper()
	current, _ := app.automations.Definitions.Get(e.run.Spec.ID)
	if _, err := app.automations.Definitions.Save(e.run.Spec, current.Revision); err != nil {
		return err
	}
	ctx, err := app.automationContext(t.Context(), e.run.Spec)
	if err != nil {
		return err
	}
	e.ctx = ctx
	e.run.Script.Contract, err = app.scriptContract(e.run.Spec)
	return err
}

func TestScriptNotebookEvidenceAndOptionalMissing(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"notebooks.read", "notebooks.publish"}, nil, map[string]string{"notes": "edit"})
	v, err := hostStep(t, app, e, "missing", "notebooks.read", map[string]any{"notebook": "notes", "path": "new.md", "optional": true})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	var missing map[string]any
	_ = json.Unmarshal(b, &missing)
	if missing["missing"] != true {
		t.Fatalf("missing read is not explicit: %s", b)
	}
	if _, err = hostStep(t, app, e, "save", "notebooks.publish", map[string]any{"notebook": "notes", "path": "new.md", "revision": "", "content": "original"}); err != nil {
		t.Fatal(err)
	}
	v, err = hostStep(t, app, e, "read", "notebooks.read", map[string]any{"notebook": "notes", "path": "new.md"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(v)
	var doc documentOutput
	_ = json.Unmarshal(b, &doc)
	if doc.Citation == "" || doc.Hash == "" || doc.Revision == "" || len(e.run.Citations) != 1 {
		t.Fatalf("untracked evidence: %s", b)
	}
}

func TestScriptSuppressedFindingDoesNotAttemptDelivery(t *testing.T) {
	app, e := scriptFixture(t, "def main(host):\n    host.call('finding', 'incidents.report', active=False, severity='warning', summary='Observed checks passed')\n    return {'checked': True}\n", []string{"incidents.report"}, nil, nil)
	e.run.Spec.NotifyChannel = "channel"
	e.run.Script = nil
	if err := app.runScript(e.ctx, e.run, e.save); err != nil {
		t.Fatal(err)
	}
	if e.run.Alert == nil || e.run.Alert.Notify || e.run.DeliveryState != "" {
		t.Fatal("suppressed incident was delivered")
	}
}

func TestScriptMissingFindingStatusDoesNotClearIncident(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"incidents.report"}, nil, nil)
	if _, err := hostStep(t, app, e, "bad", "incidents.report", map[string]any{"severity": "warning", "summary": "missing evidence"}); err == nil {
		t.Fatal("missing status treated as healthy")
	}
	if e.run.Alert != nil {
		t.Fatal("invalid finding changed the incident")
	}
}

func TestScriptRepositoryReadsBindRevisionAndRejectChangedGrants(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"notebooks.read", "notebooks.map", "repositories.changes"}, nil, map[string]string{"source": "read"})
	if _, err := hostStep(t, app, e, "info", "run.info", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	source := e.run.Script.Sources["source"]
	if source.Source == "" || source.Revision == "" || source.PreviousRevision != "" {
		t.Fatal("source snapshot missing", source)
	}
	if _, err := hostStep(t, app, e, "read", "notebooks.read", map[string]any{"notebook": "source", "origin": "repository", "path": "health.go", "revision": source.Revision, "startLine": 1, "endLine": 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := hostStep(t, app, e, "wrong", "notebooks.read", map[string]any{"notebook": "source", "origin": "repository", "path": "health.go", "revision": "unapproved"}); err == nil {
		t.Fatal("unbound revision accepted")
	}
	if _, err := hostStep(t, app, e, "unbound", "notebooks.map", map[string]any{"notebook": "private"}); err == nil {
		t.Fatal("unbound notebook accepted")
	}
	v, err := hostStep(t, app, e, "changes", "repositories.changes", map[string]any{"notebook": "source"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(v)
	if !strings.Contains(string(raw), `"baselineRequired":true`) {
		t.Fatal("invented a comparison", string(raw))
	}
	state, _ := app.repositories.Status("default")
	cfg := state.IndexConfig
	cfg.Exclude = []string{"health.go"}
	if _, err := app.repositories.ConfigureIndex("default", catalog.Bootstrap, state.Generation, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := hostStep(t, app, e, "read", "notebooks.read", map[string]any{"notebook": "source", "origin": "repository", "path": "health.go", "revision": source.Revision, "startLine": 1, "endLine": 3}); err == nil {
		t.Fatal("replay released excluded evidence")
	}
}
