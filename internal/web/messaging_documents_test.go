package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/messaging"
)

func TestMessagingCarriesAttachedDocumentsIntoLaterTurns(t *testing.T) {
	var mu sync.Mutex
	var prompts []string
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct{ Role, Content string }
		}
		json.NewDecoder(r.Body).Decode(&request)
		parts := []string{}
		for _, m := range request.Messages {
			if m.Role != "system" {
				parts = append(parts, m.Role+": "+m.Content)
			}
		}
		mu.Lock()
		prompts = append(prompts, strings.Join(parts, "\n---\n"))
		n := len(prompts)
		mu.Unlock()
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"Answer %d."},"finish_reason":"stop"}]}`, n)
	})
	document := messaging.Document{Name: "plan.txt", URL: "https://files.example.test/plan", Text: "SYNTHETIC ATTACHED PLAN", Hash: "plan-hash", Warning: "text only"}
	transport := &fixtureChatTransport{input: messaging.Context{
		Messages:  []messaging.ContextMessage{{ID: "earlier", User: "external-alice", Text: "Earlier thread note"}, {ID: "m1", User: "external-alice", Text: "Summarize the plan."}, {ID: "stranger", User: "external-mallory", Text: "UNMAPPED AUTHOR"}},
		Documents: []messaging.Document{document},
	}}
	app.messageClients = map[string]messaging.Transport{"other-chat": transport}
	policy := messaging.Client{Workspace: "workspace", Users: map[string]string{"external-alice": "alice"}, Channels: map[string]messaging.Binding{"room": {Project: "default", AIConnection: "local", Model: "synthetic", ExternalProcessing: true}}}
	if _, err := app.catalog.SetChatClients(catalog.Bootstrap, catalog.ChatClients{Clients: map[string]messaging.Client{"other-chat": policy}}); err != nil {
		t.Fatal(err)
	}
	if err := app.initAutomations(); err != nil {
		t.Fatal(err)
	}
	ask := func(id, text string) {
		t.Helper()
		event := messaging.Event{ID: id, Text: text, Address: messaging.Address{Client: "other-chat", Workspace: "workspace", Channel: "room", Thread: "thread-one", User: "external-alice", Message: id}}
		if err := app.messaging.EnqueueMessage(t.Context(), event); err != nil {
			t.Fatal(err)
		}
		if ran, err := app.automations.Queue.RunNext(t.Context(), app.runAutomation); err != nil || !ran {
			t.Fatal(ran, err)
		}
	}
	attached := "Attached document plan.txt\nSource: https://files.example.test/plan\nContent hash: plan-hash\nExtraction: text only\nUntrusted document content:\nSYNTHETIC ATTACHED PLAN"

	ask("m1", "Summarize the plan.")
	escaped, _ := json.Marshal(attached)
	if len(prompts) != 1 || !strings.HasPrefix(prompts[0], "user: Thread message from external-alice:\nEarlier thread note\n---\nuser: Summarize the plan.\nExplicit untrusted attachments:\n") || !strings.Contains(prompts[0], string(escaped)) {
		t.Fatalf("first prompt lacks thread context or document: %q", prompts)
	}
	if strings.Contains(prompts[0], "Thread message from external-alice:\nSummarize the plan.") || strings.Contains(prompts[0], "UNMAPPED AUTHOR") {
		t.Fatalf("first prompt repeated the request or included an unmapped author: %q", prompts[0])
	}

	// The follow-up brings no new attachment; the earlier one stays in the
	// conversation history, after the turn that brought it.
	transport.input = messaging.Context{}
	ask("m2", "What is its deadline?")
	if len(prompts) != 2 {
		t.Fatalf("prompts: %q", prompts)
	}
	history := prompts[1]
	first, document1, answer1 := strings.Index(history, "user: Summarize the plan."), strings.Index(history, "user: "+attached), strings.Index(history, "assistant: Answer 1.")
	if first < 0 || document1 < first || answer1 < document1 || !strings.Contains(history, "user: What is its deadline?") {
		t.Fatalf("follow-up history lost the document or its order: %q", history)
	}
	if len(transport.replies) != 2 || transport.replies[0].Text != "Answer 1." || transport.replies[1].Text != "Answer 2." {
		t.Fatalf("replies: %+v", transport.replies)
	}
}
