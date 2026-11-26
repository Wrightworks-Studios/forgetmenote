package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/personal"
	"forgetmenote/internal/workspace"
)

// The hooks web gives the personal worker refuse what their checks refuse.
// Each hook is called with the refusing case of its check's direct test.
func TestPersonalHooksRefuseWhatTheirChecksRefuse(t *testing.T) {
	t.Run("Context", func(t *testing.T) {
		app, ctx, memory := assistantMemoryAfterTurnOff(t)
		got, err := app.personalHooks().Context(ctx)
		if err == nil || strings.Contains(fmt.Sprint(got), memory) {
			t.Fatal("the Context hook read assistant memory after the instance turned assistants off", err)
		}
	})
	t.Run("CheckSources", func(t *testing.T) {
		app, ctx, run, items := disconnectedPersonalSource(t)
		if err := app.personalHooks().CheckSources(ctx, run, items); err == nil {
			t.Fatal("the CheckSources hook accepted an observation from a disconnected Google account")
		}
	})
	t.Run("MailboxDraft", func(t *testing.T) {
		app, ctx, draft, drafts := automaticDraftWithoutConsent(t)
		if _, err := app.personalHooks().MailboxDraft(ctx, draft, true); !errors.Is(err, workspace.ErrForbidden) || drafts.Load() != 0 {
			t.Fatalf("the MailboxDraft hook created %d mailbox drafts without consent: %v", drafts.Load(), err)
		}
	})
	t.Run("CheckAutomationAuth", func(t *testing.T) {
		app, ctx := automationAfterConsentWithdrawn(t)
		if err := app.personalHooks().CheckAutomationAuth(ctx); err == nil {
			t.Fatal("the CheckAutomationAuth hook kept a run's authority after alice withdrew consent")
		}
	})
}

var errAuthorityRefused = errors.New("synthetic: the run's authority was refused")

// workerWithRefusedAuthority is a second personal worker on app whose
// CheckAutomationAuth hook always refuses. Every other hook is web's own, and
// the run's real authority stays intact, so only the worker's own calls to
// that hook can stop it.
func workerWithRefusedAuthority(t *testing.T, app *Server) (*personal.Service, *atomic.Int32) {
	t.Helper()
	refusals := new(atomic.Int32)
	hooks := app.personalHooks()
	hooks.CheckAutomationAuth = func(ctx context.Context) error {
		refusals.Add(1)
		return errAuthorityRefused
	}
	worker, err := personal.New(app.catalog, app.automations, app.messaging, hooks)
	if err != nil {
		t.Fatal(err)
	}
	return worker, refusals
}

// The worker checks the run's authority before each save of its state. Once
// that check refuses, a run saves nothing.
func TestPersonalRunSavesNoStateOnceItsAuthorityIsRefused(t *testing.T) {
	app, spec := personalRunFixture(t, personalRunOptions{})
	worker, refusals := workerWithRefusedAuthority(t, app)
	before, _, err := worker.ReadState(spec)
	if err != nil {
		t.Fatal(err)
	}
	run := automation.Run{ID: automation.Hash(t.Name()), Spec: spec, Created: time.Now()}
	err = worker.Run(personalAutomationContext(t, app, spec), &run, func() error { return nil })
	after, state, e := worker.ReadState(spec)
	if e != nil {
		t.Fatal(e)
	}
	if after.Revision != before.Revision {
		t.Fatalf("saved the run's state after its authority was refused: %d pending, %d seen", len(state.Pending), len(state.Seen))
	}
	if !errors.Is(err, errAuthorityRefused) || refusals.Load() == 0 {
		t.Fatal("the run did not stop at its authority check", err)
	}
}

// The worker checks the run's authority before it writes each profile memory
// file. Once that check refuses, no memory file changes.
func TestPersonalMemoryIsNotWrittenOnceItsAuthorityIsRefused(t *testing.T) {
	app, _ := personalRunFixture(t, personalRunOptions{})
	settings, err := app.catalog.Assistant("alice")
	if err != nil {
		t.Fatal(err)
	}
	settings.MaintainMemory = true
	if settings, err = app.catalog.SaveAssistant("alice", settings); err != nil {
		t.Fatal(err)
	}
	spec, err := app.personalSpec("alice", settings)
	if err != nil {
		t.Fatal(err)
	}
	ctx := personalAutomationContext(t, app, spec)
	if err = app.personal.Seed(ctx, settings); err != nil {
		t.Fatal(err)
	}
	store, err := app.catalog.Access("alice", spec.Project, "read")
	if err != nil {
		t.Fatal(err)
	}
	// A generated update is waiting to be written over the seeded files.
	state := personal.MemoryState{Status: "generated", Batch: []personal.Interaction{{ID: "i1", UserText: "Keep replies short.", Response: "Noted.", Source: "chat", At: time.Now()}}, Hashes: map[string]string{}, Content: map[string]string{}}
	revisions := map[string]string{}
	for _, f := range personal.MemoryFiles {
		doc, err := store.Read(t.Context(), f.Path, "")
		if err != nil {
			t.Fatal(err)
		}
		revisions[f.Path] = doc.Revision
		state.Hashes[f.Path] = workspace.ContentHash(doc.Content)
		state.Content[f.Path] = "# Updated\n\nKeep replies short.\n"
	}
	raw, _ := json.Marshal(state)
	scope := personal.MemorySpec(spec)
	stored, err := app.automations.Queue.JobState(scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.automations.Queue.SaveJobState(scope, stored.Revision, raw, false); err != nil {
		t.Fatal(err)
	}
	worker, refusals := workerWithRefusedAuthority(t, app)
	err = worker.MaintainMemory(ctx, &automation.Run{Spec: spec})
	for _, f := range personal.MemoryFiles {
		doc, e := store.Read(t.Context(), f.Path, "")
		if e != nil || doc.Revision != revisions[f.Path] {
			t.Fatalf("wrote %s after the run's authority was refused: %v", f.Path, e)
		}
	}
	if !errors.Is(err, errAuthorityRefused) || refusals.Load() == 0 {
		t.Fatal("memory upkeep did not stop at its authority check", err)
	}
}
