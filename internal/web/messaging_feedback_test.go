package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/messaging"
)

type feedbackTransport struct {
	fixtureChatTransport
	statuses []string
}

func (f *feedbackTransport) Status(_ context.Context, _ messaging.Address, text string) error {
	f.statuses = append(f.statuses, text)
	return nil
}

func TestMessagingReportsFailureAndClearsProgressWithoutRepeating(t *testing.T) {
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "private provider detail", 500) })
	configureSlackFixture(t, app)
	transport := &feedbackTransport{}
	app.messageClients["slack"] = transport
	run := automation.Run{ID: automation.Hash("failed-message"), Spec: automation.Spec{ID: "failed-message", Kind: "assistant", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", Instruction: "Answer this.", ExternalProcessing: true, Delivery: &automation.Delivery{Channel: "C123", Thread: "1.0", User: "U123"}}}
	if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err == nil {
		t.Fatal("provider failure disappeared")
	}
	if len(transport.replies) != 1 || !strings.Contains(transport.replies[0].Text, "couldn't finish") || strings.Contains(transport.replies[0].Text, "private provider detail") {
		t.Fatal("failure not reported safely", transport.replies)
	}
	if len(transport.statuses) < 2 || transport.statuses[0] == "" || transport.statuses[len(transport.statuses)-1] != "" {
		t.Fatal("progress not started and cleared", transport.statuses)
	}
	app.runAutomation(t.Context(), &run, func() error { return nil })
	if len(transport.replies) != 1 {
		t.Fatal("terminal failure notification repeated")
	}
}

func TestSlackContextPaginatesWithoutByteCutoff(t *testing.T) {
	app := chatFixture(t)
	configureSlackFixture(t, app)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "next" {
			w.Write([]byte(`{"ok":true,"messages":[{"ts":"1.2","user":"U123","text":"LAST_THREAD_MESSAGE"}],"has_more":false}`))
			return
		}
		w.Write([]byte(`{"ok":true,"messages":[{"ts":"1.1","user":"U123","text":"` + strings.Repeat("x", 30000) + `"}],"has_more":true,"response_metadata":{"next_cursor":"next"}}`))
	}))
	defer upstream.Close()
	app.slack.endpoint = upstream.URL
	got, err := app.slack.Context(t.Context(), messaging.Address{Channel: "C123", Thread: "1.0", Message: "1.3"}, messaging.Binding{ThreadHistory: true})
	if err != nil || calls != 2 || len(got.Messages) != 2 || got.Messages[1].Text != "LAST_THREAD_MESSAGE" {
		t.Fatalf("thread incomplete: calls=%d messages=%d error=%v", calls, len(got.Messages), err)
	}
}
