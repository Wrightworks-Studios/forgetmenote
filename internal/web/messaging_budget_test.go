package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/messaging"
)

type deadlineChatTransport struct {
	fixtureChatTransport
	deadline time.Time
}

func (f *deadlineChatTransport) Context(ctx context.Context, _ messaging.Address, _ messaging.Binding) (messaging.Context, error) {
	f.deadline, _ = ctx.Deadline()
	return messaging.Context{}, nil
}

func TestMessagingUsesConfiguredDeadline(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		connection, job, caller, want int
	}{
		{"connection budget", 900, 0, 0, 900},
		{"explicit shorter job", 900, 30, 0, 30},
		{"connection bounds job", 30, 900, 0, 30},
		{"caller cancellation", 900, 0, 5, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Complete."},"finish_reason":"stop"}]}`)
			})
			configureSlackFixture(t, app)
			config := *app.chat.Config
			config.Connections = append([]chat.Connection(nil), config.Connections...)
			config.Limits.RunSeconds = tc.connection
			config.Connections[0].Limits.RunSeconds = tc.connection
			raw, _ := json.Marshal(config)
			file := filepath.Join(t.TempDir(), "chat.json")
			if err := os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := app.ConfigureChat(file); err != nil {
				t.Fatal(err)
			}
			transport := &deadlineChatTransport{}
			app.messageClients["slack"] = transport
			run := automation.Run{ID: automation.Hash(tc.name), Spec: automation.Spec{ID: "message-deadline", Kind: "assistant", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", Instruction: "Answer this.", ExternalProcessing: true, TimeoutSeconds: tc.job, Delivery: &automation.Delivery{Channel: "C123", Thread: "1.0", User: "U123"}}}
			ctx := t.Context()
			start := time.Now()
			if tc.caller > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Duration(tc.caller)*time.Second)
				defer cancel()
			}
			if err := app.runAutomation(ctx, &run, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			got := transport.deadline.Sub(start)
			want := time.Duration(tc.want) * time.Second
			if got < want-time.Second || got > want+time.Second {
				t.Fatalf("transport received %s, want the configured %s deadline", got, want)
			}
			if len(transport.replies) != 1 || transport.replies[0].Text != "Complete." {
				t.Fatal("answer was not delivered", transport.replies)
			}
		})
	}
}

func TestMessagingReservesAnswerWithinConfiguredBudget(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		rounds, tools, calls int
	}{
		{"last round", 3, 16, 3},
		{"tool allowance", 8, 1, 2},
		{"one round", 1, 16, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				var request struct {
					Tools []json.RawMessage `json:"tools"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if len(request.Tools) == 0 {
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Answered from available evidence."},"finish_reason":"stop"}]}`)
					return
				}
				fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_%d","type":"function","function":{"name":"list_data_connections","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, calls)
			})
			configureSlackFixture(t, app)
			config := *app.chat.Config
			config.Connections = append([]chat.Connection(nil), config.Connections...)
			config.Connections[0].Limits.MaxRounds = tc.rounds
			config.Connections[0].Limits.MaxTools = tc.tools
			raw, _ := json.Marshal(config)
			file := filepath.Join(t.TempDir(), "chat.json")
			if err := os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := app.ConfigureChat(file); err != nil {
				t.Fatal(err)
			}
			transport := &fixtureChatTransport{}
			app.messageClients["slack"] = transport
			run := automation.Run{ID: automation.Hash(tc.name), Spec: automation.Spec{ID: "message-answer", Kind: "assistant", Actor: "alice", Project: "default", AIConnection: "local", Model: "synthetic", Instruction: "Answer this.", ExternalProcessing: true, Delivery: &automation.Delivery{Channel: "C123", Thread: "1.0", User: "U123"}}}
			if err := app.runAutomation(t.Context(), &run, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			if calls != tc.calls || len(transport.replies) != 1 || transport.replies[0].Text != "Answered from available evidence." {
				t.Fatal("allowance ended without a delivered answer", calls, transport.replies)
			}
		})
	}
}
