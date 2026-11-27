package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/automationrun"
	"forgetmenote/internal/chat"
	"strings"
	"testing"
	"time"
)

func TestScriptRenderedFetchRequiresVisionGrant(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"web.fetch"}, nil, nil)
	raw, _ := json.Marshal(map[string]any{"url": "https://example.org", "render": true})
	if err := app.automationrun.ScriptCallAuthority(e.host(), automationrun.ScriptRequest{Operation: "web.fetch", Arguments: raw}); err == nil {
		t.Fatal("rendered website evidence accepted without ai.images grant")
	}
}

func TestScriptRenderedFetchPreservesExternalConsentAndFetchBudget(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"web.fetch", "ai.images"}, nil, nil)
	args := map[string]any{"url": "https://example.org", "render": true}
	e.run.Spec.ExternalProcessing = false
	raw, _ := json.Marshal(args)
	if err := app.automationrun.ScriptCallAuthority(e.host(), automationrun.ScriptRequest{Operation: "web.fetch", Arguments: raw}); err == nil {
		t.Fatal("render accepted without external consent")
	}
	e.run.Spec.ExternalProcessing = true
	if _, err := app.automationrun.ScriptOperation(e.host(), automationrun.ScriptRequest{Operation: "web.fetch", Arguments: raw}, time.Now()); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("missing configuration silently fell back: %v", err)
	}
	for i := 0; i < 6; i++ {
		e.run.Script.Steps[fmt.Sprintf("fetch-%d", i)] = &automation.ScriptStep{Operation: "web.fetch", Input: raw}
	}
	if _, err := app.automationrun.ScriptOperation(e.host(), automationrun.ScriptRequest{Operation: "web.fetch", Arguments: raw}, time.Now()); err == nil || !strings.Contains(err.Error(), "fetch budget exhausted") {
		t.Fatalf("render bypassed fetch budget: %v", err)
	}
}

func TestScriptRenderedFetchInterruptedPurchaseIsNotRepeated(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"web.fetch", "ai.images"}, nil, nil)
	saved := e.save
	e.save = func() error { return errors.New("synthetic interruption after recording intent") }
	args := map[string]any{"url": "https://example.org", "render": true}
	if _, err := hostStep(t, app, e, "capture", "web.fetch", args); err == nil {
		t.Fatal("interruption lost")
	}
	if !e.run.Script.Steps["capture"].Effect || !e.run.Script.Uncertain() {
		t.Fatal("paid capture missing recovery fence")
	}
	e.save = saved
	if _, err := hostStep(t, app, e, "capture", "web.fetch", args); err == nil || !strings.Contains(err.Error(), "outcome unknown") {
		t.Fatalf("interrupted capture repurchased: %v", err)
	}
}

func TestScriptRenderedFetchDefinitiveFailureContinuesWithoutRepurchase(t *testing.T) {
	app, e := scriptFixture(t, "def main(host): return {}", []string{"web.fetch", "ai.images"}, nil, nil)
	calls := 0
	app.automationrun.SetScriptWebsiteCapture(func(context.Context, string) (chat.WebsiteEvidence, error) {
		calls++
		return chat.WebsiteEvidence{}, errors.Join(chat.ErrWebsiteCaptureSettled, errors.New("synthetic returned HTTP 403"))
	})
	args := map[string]any{"url": "https://example.org", "render": true}
	if _, err := hostStep(t, app, e, "capture", "web.fetch", args); err == nil {
		t.Fatal("capture failure hidden")
	}
	step := e.run.Script.Steps["capture"]
	if step.State != "completed" || step.Error == "" || !step.Effect || e.run.Script.Uncertain() {
		t.Fatal("definitive capture failure left an unknown purchase")
	}
	if _, err := hostStep(t, app, e, "capture", "web.fetch", args); err == nil || calls != 1 {
		t.Fatal("failed capture repurchased or replay lost failure")
	}
	if _, err := hostStep(t, app, e, "complete", "run.complete", map[string]any{"result": map[string]any{"uncertain": true}}); err != nil {
		t.Fatalf("definitive failure prevented completion: %v", err)
	}
}

func TestScriptRenderedFetchPrePurchaseRejectionsDoNotFenceLaterWork(t *testing.T) {
	for _, mode := range []string{"invalid-url", "budget"} {
		t.Run(mode, func(t *testing.T) {
			app, e := scriptFixture(t, "def main(host): return {}", []string{"web.fetch", "ai.images"}, nil, nil)
			target := "http://127.0.0.1/"
			if mode == "budget" {
				target = "https://example.org"
				for i := 0; i < 5; i++ {
					e.run.Script.Steps[fmt.Sprintf("previous-%d", i)] = &automation.ScriptStep{Operation: "web.fetch", Input: json.RawMessage(`{"url":"https://example.org"}`), State: "completed"}
				}
			}
			if _, err := hostStep(t, app, e, "capture", "web.fetch", map[string]any{"url": target, "render": true}); err == nil {
				t.Fatal("invalid capture accepted")
			}
			if e.run.Script.Uncertain() || e.run.Script.Steps["capture"].Effect {
				t.Fatal("pre-provider rejection fenced subsequent decisions")
			}
		})
	}
}
