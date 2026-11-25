package web

import (
	"encoding/json"
	"forgetmenote/internal/automation"
	"testing"
)

func TestScriptJobStateSurvivesSeparateRunsAndPreview(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"jobstate"}, nil, nil)
	get := func() automation.JobState {
		t.Helper()
		v, err := hostStep(t, app, e, "get", "jobstate.get", map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(v)
		var state automation.JobState
		json.Unmarshal(b, &state)
		return state
	}
	first := get()
	if _, err := hostStep(t, app, e, "set", "jobstate.set", map[string]any{"revision": first.Revision, "data": map[string]string{"history": "9007199254740995"}}); err != nil {
		t.Fatal(err)
	}
	e.run.ID = automation.Hash("next scheduled run")
	e.run.Script.Steps = map[string]*automation.ScriptStep{}
	second := get()
	if string(second.Data) != `{"history":"9007199254740995"}` {
		t.Fatal("state lost between runs", string(second.Data))
	}
	if _, err := hostStep(t, app, e, "stale", "jobstate.set", map[string]any{"revision": first.Revision, "data": map[string]any{}}); err == nil {
		t.Fatal("stale state write accepted")
	}
	e.run.DryRun = true
	if _, err := hostStep(t, app, e, "preview", "jobstate.set", map[string]any{"revision": second.Revision, "data": map[string]string{"history": "new"}}); err != nil {
		t.Fatal(err)
	}
	actual, err := app.automations.Queue.JobState(e.run.Spec)
	if err != nil || actual.Revision != second.Revision {
		t.Fatal("preview advanced live cursor", err)
	}
}
