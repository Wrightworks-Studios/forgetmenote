package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
)

func TestMessagingUsesCurrentBoundNotebookInstructions(t *testing.T) {
	var requests []string
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests = append(requests, string(body))
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Ready."},"finish_reason":"stop"}]}`))
	})
	configureSlackFixture(t, app)
	app.messageClients["slack"] = &fixtureChatTransport{}
	store, _ := app.catalog.Access(catalog.Bootstrap, "default", "owner")
	first, err := store.WriteInstructions(t.Context(), []byte("FIRST_OWNER_DISPOSITION: Use our team's preferred vocabulary."), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	for i, marker := range []string{"FIRST_OWNER_DISPOSITION", "SECOND_OWNER_DISPOSITION"} {
		if i == 1 {
			if _, err = store.WriteInstructions(t.Context(), []byte("SECOND_OWNER_DISPOSITION: Explain implications in plain language."), first.Revision, "test"); err != nil {
				t.Fatal(err)
			}
		}
		id := automation.Hash(marker)
		run := automation.Run{ID: id, Spec: automation.Spec{ID: "guidance", Kind: "assistant", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", ExternalProcessing: true, Instruction: "Help our team.", Delivery: &automation.Delivery{Channel: "C123", Thread: id, User: "U123"}}}
		if err = app.automationrun.RunAutomation(t.Context(), &run, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		if len(requests) != i+1 || !strings.Contains(requests[i], marker) {
			t.Fatalf("current notebook guidance missing: %v", requests)
		}
		if i == 1 && strings.Contains(requests[i], "FIRST_OWNER_DISPOSITION") {
			t.Fatal("new question retained stale disposition")
		}
		if strings.Contains(requests[i], "empty parent-assignment table") || strings.Contains(requests[i], "For conversational replies, lead with the direct business answer") {
			t.Fatal("business guidance remains embedded in the platform")
		}
	}
}

func TestMessagingRejectsInstructionChangeDuringGeneration(t *testing.T) {
	var change func()
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		change()
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Obsolete answer."},"finish_reason":"stop"}]}`))
	})
	configureSlackFixture(t, app)
	transport := &fixtureChatTransport{}
	app.messageClients["slack"] = transport
	store, _ := app.catalog.Access(catalog.Bootstrap, "default", "owner")
	doc, err := store.WriteInstructions(t.Context(), []byte("Use current owner guidance."), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	change = func() {
		if _, e := store.WriteInstructions(t.Context(), []byte("Owner changed this guidance."), doc.Revision, "test"); e != nil {
			t.Error(e)
		}
	}
	run := automation.Run{ID: automation.Hash("changed-guidance"), Spec: automation.Spec{ID: "guidance", Kind: "assistant", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", ExternalProcessing: true, Instruction: "Help our team.", Delivery: &automation.Delivery{Channel: "C123", Thread: "1.2", User: "U123"}}}
	if err = app.automationrun.RunAutomation(t.Context(), &run, func() error { return nil }); err == nil {
		t.Fatalf("stale guidance delivered: %v %v", err, transport.replies)
	}
	for _, reply := range transport.replies {
		if strings.Contains(reply.Text, "Obsolete answer.") {
			t.Fatal("obsolete answer was delivered", reply)
		}
	}
}

func TestMessagingRecoveryRejectsChangedNotebookInstructions(t *testing.T) {
	calls := 0
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Previously generated answer."},"finish_reason":"stop"}]}`))
	})
	configureSlackFixture(t, app)
	transport := &fixtureChatTransport{}
	app.messageClients["slack"] = transport
	store, _ := app.catalog.Access(catalog.Bootstrap, "default", "owner")
	doc, err := store.WriteInstructions(t.Context(), []byte("Initial disposition."), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	run := automation.Run{ID: automation.Hash("recover-guidance"), Spec: automation.Spec{ID: "guidance", Kind: "assistant", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", ExternalProcessing: true, Instruction: "Help our team.", Delivery: &automation.Delivery{Channel: "C123", Thread: "1.2", User: "U123"}}}
	var checkpoint []byte
	err = app.automationrun.RunAutomation(t.Context(), &run, func() error {
		if run.Answer != "" {
			return fmt.Errorf("checkpoint unavailable after conversation commit")
		}
		checkpoint, _ = json.Marshal(run)
		return nil
	})
	if err == nil || calls != 1 {
		t.Fatalf("expected interrupted saved answer: %v %d", err, calls)
	}
	if err = json.Unmarshal(checkpoint, &run); err != nil {
		t.Fatal(err)
	}
	if _, err = store.WriteInstructions(t.Context(), []byte("Revised disposition."), doc.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	if err = app.automationrun.RunAutomation(t.Context(), &run, func() error { return nil }); err == nil || calls != 1 {
		t.Fatalf("recovered stale answer or repeated model: %v %d", err, calls)
	}
	for _, reply := range transport.replies {
		if strings.Contains(reply.Text, "Previously generated answer.") {
			t.Fatal("recovered obsolete answer was delivered")
		}
	}
}
