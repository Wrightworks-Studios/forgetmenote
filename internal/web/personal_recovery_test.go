package web

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/workspace"
)

func TestPersonalChangedEvidenceSupersedesInsteadOfBlocking(t *testing.T) {
	old := personalObservation{Key: "email:t1", Revision: "old", Source: connectors.PersonalOperation{Provider: "google", Generation: 1, Operation: "email.thread", Arguments: map[string]any{"id": "t1"}}}
	next := old
	next.Revision = "new"
	id := personalFingerprint(old)
	state := personalState{Seen: map[string]string{}, Drafts: map[string]personalDraft{id: {ID: id, State: "ready", Source: old}}, Pending: []personalObservation{old}}
	supersedePersonal(&state, old)
	if len(state.Reads) != 1 || state.Drafts[id].State != "superseded" {
		t.Fatal("old work blocks new evidence")
	}
	observePersonal(&state, next)
	observePersonal(&state, next)
	if len(state.Pending) != 1 || state.Pending[0].Revision != "new" {
		t.Fatal("stale or duplicate input retained")
	}
	resolvePersonal(&state, old.Key, "resolved")
}

func TestPersonalInterruptedMemoryRecoveryIsOwnerAndRevisionBound(t *testing.T) {
	app, _ := fixture(t)
	app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true})
	settings, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, MaintainMemory: true, TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := app.personalSpec("alice", settings)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(personalMemoryState{Status: "started", Batch: []personalInteraction{{ID: "one", UserText: "Keep it concise"}}})
	stored, err := app.automations.Queue.SaveJobState(personalMemorySpec(spec), "", raw, false)
	if err != nil {
		t.Fatal(err)
	}
	action := app.actions["recover_assistant_work"]
	args, _ := json.Marshal(map[string]any{"id": "memory", "revision": stored.Revision, "retry": true})
	ctx := context.WithValue(t.Context(), identityKey{}, "alice")
	if _, err = action.Call(context.WithValue(ctx, apiContextKey{}, true), args); err == nil {
		t.Fatal("agent retried a paid call")
	}
	if _, err = action.Call(context.WithValue(ctx, identityKey{}, "bob"), args); err == nil {
		t.Fatal("cross-owner recovery")
	}
	if _, err = action.Call(ctx, args); err != nil {
		t.Fatal(err)
	}
	if _, err = action.Call(ctx, args); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("stale recovery: %v", err)
	}
	stored, err = app.automations.Queue.JobState(personalMemorySpec(spec))
	if err != nil {
		t.Fatal(err)
	}
	var memory personalMemoryState
	json.Unmarshal(stored.Data, &memory)
	if memory.Status != "" || len(memory.Pending) != 1 {
		t.Fatal("recovery did not preserve the interaction")
	}
	run := chat.Run{PersonalConsent: app.personalConsentStamp("alice")}
	settings.Enabled = false
	app.catalog.SaveAssistant("alice", settings)
	if err = app.validateChatSources(ctx, run, false); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatal("paused consent did not fence retained chat evidence", err)
	}
}

func TestPersonalCalendarReminderUsesEventAndEmployeeZone(t *testing.T) {
	for _, test := range []struct{ event, want string }{
		{`{"start":{"dateTime":"2026-10-09T10:00:00-06:00"}}`, "2026-10-09T15:30:00Z"},
		{`{"start":{"date":"2026-10-09"}}`, "2026-10-09T14:30:00Z"},
	} {
		got := personalCalendarReminder(personalObservation{Content: json.RawMessage(test.event)}, "America/Denver")
		if got != test.want {
			t.Fatalf("reminder=%s want=%s", got, test.want)
		}
	}
	if personalCalendarReminder(personalObservation{Content: json.RawMessage(`{}`)}, "UTC") != "" {
		t.Fatal("invented event time")
	}
}

func TestPersonalMemoryConflictKeepsEmployeeEditAndRecoverableProposal(t *testing.T) {
	app, _ := fixture(t)
	app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true})
	settings, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, MaintainMemory: true, TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := app.personalSpec("alice", settings)
	ctx, err := app.automationContext(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.seedAssistant(ctx, settings); err != nil {
		t.Fatal(err)
	}
	store, _ := app.catalog.Access("alice", settings.Project, "owner")
	state := personalMemoryState{Status: "generated", Hashes: map[string]string{}, Content: map[string]string{}, Batch: []personalInteraction{{ID: "one", UserText: "Use my preferred tone"}}}
	for _, seed := range assistantMemoryFiles {
		doc, err := store.Read(ctx, seed.Path, "")
		if err != nil {
			t.Fatal(err)
		}
		state.Hashes[seed.Path] = workspace.ContentHash(doc.Content)
		state.Content[seed.Path] = string(workspace.MarkdownBody(doc.Content))
	}
	state.Content["SOUL.md"] = "# Proposed preference\nBe formal."
	raw, _ := json.Marshal(state)
	if _, err = app.automations.Queue.SaveJobState(personalMemorySpec(spec), "", raw, false); err != nil {
		t.Fatal(err)
	}
	doc, _ := store.Read(ctx, "SOUL.md", "")
	saved, err := store.Save(ctx, "SOUL.md", []byte("# My preference\nBe casual."), doc.Revision, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err = app.maintainPersonalMemory(ctx, &automation.Run{Spec: spec}); err == nil {
		t.Fatal("concurrent edit overwritten")
	}
	after, _ := store.Read(ctx, "SOUL.md", "")
	if workspace.ContentHash(after.Content) != workspace.ContentHash(saved.Content) {
		t.Fatal("employee edit changed")
	}
	retained, _ := app.automations.Queue.JobState(personalMemorySpec(spec))
	if err = json.Unmarshal(retained.Data, &state); err != nil || state.Status != "generated" || state.Content["SOUL.md"] == "" {
		t.Fatal("proposal not recoverable", err)
	}
}

func TestPersonalEditedMailboxSuggestionDoesNotBlockPublication(t *testing.T) {
	app, _ := fixture(t)
	app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true})
	settings, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, MailboxDrafts: true, TimeZone: "UTC", Features: []string{"email"}})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := app.personalSpec("alice", settings)
	ctx, err := app.automationContext(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := app.catalog.Access("alice", settings.Project, "owner")
	old, err := store.Save(ctx, "drafts/edited.md", []byte("Original proposed reply"), "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.Save(ctx, old.Path, []byte("My edited reply"), old.Revision, "alice")
	if err != nil {
		t.Fatal(err)
	}
	state := personalState{Drafts: map[string]personalDraft{
		"edited": {ID: "edited", State: "ready", Reply: "Old reply", Path: old.Path, NoteRevision: old.Revision, NoteHash: workspace.ContentHash(old.Content), Source: personalObservation{Kind: "email"}},
		"other":  {ID: "other", State: "ready", Title: "Another item", Path: "drafts/other.md", Source: personalObservation{Kind: "briefing"}},
	}}
	if err = app.publishPersonalDrafts(ctx, &automation.Run{Spec: spec}, &state, func() error { return nil }, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if state.Drafts["edited"].State != "edited" || state.Drafts["other"].NoteRevision == "" {
		t.Fatal("edited suggestion blocked another publication")
	}
	after, err := store.Read(ctx, old.Path, "")
	if err != nil || workspace.ContentHash(after.Content) != workspace.ContentHash(current.Content) {
		t.Fatal("employee edit overwritten", err)
	}
}

func TestPersonalInterruptedMemoryDoesNotStopMonitoring(t *testing.T) {
	app, _ := fixture(t)
	app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true})
	settings, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, MaintainMemory: true, TimeZone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := app.personalSpec("alice", settings)
	raw, _ := json.Marshal(personalMemoryState{Status: "started", Batch: []personalInteraction{{ID: "one", UserText: "Keep it brief"}}})
	if _, err = app.automations.Queue.SaveJobState(personalMemorySpec(spec), "", raw, false); err != nil {
		t.Fatal(err)
	}
	if err = app.runAutomation(t.Context(), &automation.Run{Spec: spec}, func() error { return nil }); err != nil {
		t.Fatal("memory recovery blocked monitoring", err)
	}
	stored, err := app.automations.Queue.JobState(personalMemorySpec(spec))
	if err != nil {
		t.Fatal(err)
	}
	var state personalMemoryState
	if err = json.Unmarshal(stored.Data, &state); err != nil || state.Status != "started" || len(state.Batch) != 1 {
		t.Fatal("interrupted paid work was retried or discarded", err)
	}
}
