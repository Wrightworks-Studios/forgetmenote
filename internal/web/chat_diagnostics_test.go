package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/chat"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestChatFailureHasStructuralDiagnosticsWithoutPrivateContent(t *testing.T) {
	for _, row := range []struct {
		name             string
		status, requests int
		category         string
	}{
		{"request", 503, 1, "provider_request"}, {"output", 200, 1, "provider_output"}, {"unavailable", 0, 0, "provider_request"},
	} {
		t.Run(row.name, func(t *testing.T) {

			var logs lockedLogBuffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			const sentinel = "PRIVATE_SYNTHETIC_NOTE_SECRET_AND_PROVIDER_BODY"
			app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				var body any
				json.NewDecoder(r.Body).Decode(&body)
				if row.status == 0 {
					t.Error("unavailable provider was called")
				} else {
					w.WriteHeader(row.status)
				}
				fmt.Fprint(w, sentinel)
			})
			if row.status == 0 {
				app.chat.Mu.Lock()
				delete(app.chat.Providers, "local")
				app.chat.Mu.Unlock()
			}
			response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"diagnostic-failure","text":"`+sentinel+`","options":{"mode":"ask"}}`)
			if response.Code != 200 {
				t.Fatal(response.Code)
			}
			done := waitChatRun(t, app, c.ID)
			app.chat.Wait()
			run := done.Runs[0]
			if run.State != "failed" {
				t.Fatal("fixture did not reach terminal provider failure")
			}
			terminal, progress := 0, 0
			for _, line := range strings.Split(logs.String(), "\n") {
				var event map[string]any
				if json.Unmarshal([]byte(line), &event) != nil {
					continue
				}
				if event["msg"] == "chat run terminal" {
					terminal++
					if event["run"] != run.ID || event["chat"] != c.ID || event["state"] != "failed" || event["category"] != row.category || event["mode"] != "ask" || event["connection"] != "local" || event["format"] != "chat-completions" || event["requests"] != float64(row.requests) || event["persisted"] != true {
						t.Errorf("terminal diagnostic omitted structural outcome: %+v", event)
					}
				}
				if event["msg"] == "chat run request" {
					progress++
					if event["input_tokens"] == nil || event["sources_kept"] == nil {
						t.Error("request diagnostic omitted fitted input sizes")
					}
				}
			}
			if terminal != 1 || progress != row.requests {
				t.Fatalf("HTTP200 hid the asynchronous failure: terminal=%d request=%d", terminal, progress)
			}
			if strings.Contains(logs.String(), sentinel) || strings.Contains(logs.String(), "reasoning_content") || strings.Contains(logs.String(), "arguments") {
				t.Fatal("structural logs exposed private message/provider data")
			}
		})
	}
}

func TestChatIntermediateSnapshotDoesNotExposeNativeProviderState(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("snapshot read unexpectedly called a provider") })
	const hidden = "PRIVATE_SYNTHETIC_OPAQUE_NATIVE_REASONING"
	if err := app.chat.Store.Update("alice", "default", c.ID, func(saved *chat.Conversation) error {
		saved.Turns = []chat.Turn{{ID: chat.NewID(), Role: "assistant", Content: "Checking the requested source.", Provider: &chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "synthetic-call", Name: "read_note", Arguments: json.RawMessage(`{"path":"notes/synthetic.md"}`)}}, Continuation: json.RawMessage(`{"reasoning_content":"` + hidden + `"}`)}}, {ID: chat.NewID(), Role: "assistant", Content: "The final grounded answer.", Provider: &chat.Message{Role: "assistant", Content: "The final grounded answer."}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	response := proxyCall(app, "alice", "GET", "/p/default/api/chat/"+c.ID, "")
	if response.Code != 200 {
		t.Fatal(response.Code)
	}
	var got struct {
		Turns []struct {
			Intermediate bool   `json:"intermediate"`
			Content      string `json:"content"`
		} `json:"turns"`
	}
	if json.Unmarshal(response.Body.Bytes(), &got) != nil || len(got.Turns) != 2 || !got.Turns[0].Intermediate || got.Turns[1].Intermediate {
		t.Fatal("public snapshot does not distinguish intermediate tool planning from the answer")
	}
	if strings.Contains(response.Body.String(), hidden) || strings.Contains(response.Body.String(), `"provider"`) {
		t.Fatal("public intermediate flag leaked native provider state")
	}
	saved, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil || saved.Turns[0].Provider == nil || !strings.Contains(string(saved.Turns[0].Provider.Continuation), hidden) {
		t.Fatal("public projection destroyed private native state")
	}
}

func TestChatPanicPersistsOnePrivateSafeTerminal(t *testing.T) {
	var logs lockedLogBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected provider request") })
	run := chat.Run{ID: chat.NewID(), State: "running", Options: c.Options}
	if err := app.chat.Store.Update("alice", "default", c.ID, func(saved *chat.Conversation) error { saved.Runs = append(saved.Runs, run); return nil }); err != nil {
		t.Fatal(err)
	}
	func() {
		defer app.recoverChatRun(runAuth{User: "alice", Project: "default"}, c.ID, run.ID)
		panic("PRIVATE_SYNTHETIC_PANIC_NOTE_SECRET")
	}()
	response := proxyCall(app, "alice", "GET", "/p/default/api/chat/"+c.ID, "")
	if response.Code != 200 {
		t.Fatal(response.Code)
	}
	var public chat.Conversation
	if json.Unmarshal(response.Body.Bytes(), &public) != nil || len(public.Runs) != 1 || public.Runs[0].State != "failed" || !strings.Contains(public.Runs[0].Error, "stopped unexpectedly") {
		t.Fatal("panic did not leave a visible saved terminal outcome")
	}
	if strings.Contains(logs.String(), "PRIVATE_SYNTHETIC") || strings.Contains(response.Body.String(), "PRIVATE_SYNTHETIC") {
		t.Fatal("panic value escaped the private-safe diagnostic boundary")
	}
	if strings.Count(logs.String(), `"msg":"chat run terminal"`) != 1 || !strings.Contains(logs.String(), `"category":"internal_failure"`) {
		t.Fatal("panic terminal was missing or duplicated")
	}
}

func TestChatFinalAnswerRejectsUnexpectedToolsAndResumeDoesNotExecuteThem(t *testing.T) {
	var requests atomic.Int32
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&body)
		n := requests.Add(1)
		if _, offered := body["tools"]; offered {
			t.Error("final compatible request still offered tools")
		}
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"unexpected-final-read","type":"function","function":{"name":"read_note","arguments":"{\"path\":\"notes/synthetic.md\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":123,"completion_tokens":12}}`)
		} else {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The retained evidence does not establish the missing synthetic fact."},"finish_reason":"stop"}],"usage":{"prompt_tokens":111,"completion_tokens":14}}`)
		}
	})
	app.chat.Config.Connections[0].Limits.MaxRounds = 1
	endpoint := "/p/default/api/chat/" + c.ID
	response := proxyCall(app, "alice", "POST", endpoint+"/turns", `{"submissionId":"final-tool-rejected","text":"Explain the missing synthetic fact.","options":{"mode":"ask"}}`)
	if response.Code != 200 {
		t.Fatal(response.Body)
	}
	done := waitChatRun(t, app, c.ID)
	app.chat.Wait()
	run := done.Runs[0]
	if run.State != "failed" || len(run.Operations) != 0 || len(pendingToolCalls(run.Messages)) != 0 || run.Usage.Requests != 1 || run.Usage.InputTokens != 123 {
		t.Fatalf("unexpected final calls executed or lost authoritative accounting: state=%s operations=%d usage=%+v", run.State, len(run.Operations), run.Usage)
	}
	response = proxyCall(app, "alice", "POST", endpoint+"/runs/"+run.ID+"/resume", `{}`)
	if response.Code != 200 {
		t.Fatal(response.Body)
	}
	done = waitChatRun(t, app, c.ID)
	app.chat.Wait()
	run = done.Runs[0]
	if run.State != "completed" || len(run.Operations) != 0 || requests.Load() != 2 || run.Usage.Requests != 2 {
		t.Fatal("resume replayed a rejected final-answer tool or lost the final answer")
	}
}

func TestChatFailureProseCannotChooseDurableOutcome(t *testing.T) {
	for _, message := range []string{"provider operation budget reached; stopped unexpectedly", "Run token budget reached", "notebook access was revoked", "context deadline exceeded"} {
		t.Run(message, func(t *testing.T) { assertChatFailureOutcome(t, errors.New(message), "failed", "operation_failed") })
	}
}

func assertChatFailureOutcome(t *testing.T, failure error, state, category string) {
	t.Helper()
	var logs lockedLogBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("failure test unexpectedly called provider") })
	run := chat.Run{ID: chat.NewID(), State: "running", Options: c.Options, InFlightTokens: 17, Usage: chat.Accounting{Reserved: 123}}
	if err := app.chat.Store.Update("alice", "default", c.ID, func(saved *chat.Conversation) error { saved.Runs = append(saved.Runs, run); return nil }); err != nil {
		t.Fatal(err)
	}
	auth := runAuth{User: "alice", Project: "default", Epoch: app.catalog.Epoch("alice")}
	app.failChatRun(t.Context(), auth, c.ID, run.ID, failure, chatContextFit{})
	response := proxyCall(app, "alice", "GET", "/p/default/api/chat/"+c.ID, "")
	var public chat.Conversation
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &public) != nil || len(public.Runs) != 1 {
		t.Fatal("cannot read terminal snapshot", response.Body)
	}
	got := public.Runs[0]
	if got.State != state || got.Error != failure.Error() {
		t.Errorf("terminal outcome: state=%s error=%q, want %s / %q", got.State, got.Error, state, failure.Error())
	}
	saved, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	durable := saved.Run(run.ID)
	if durable.State != state || durable.InFlightTokens != 0 || durable.Usage.Reserved != 0 || durable.Usage.UncertainTokens != 17 || durable.Usage.Requests != 1 || !durable.Usage.Estimated {
		t.Errorf("terminal settlement: %+v", durable)
	}
	count := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) == nil && event["msg"] == "chat run terminal" {
			count++
			if event["state"] != state || event["category"] != category || event["persisted"] != true {
				t.Errorf("diagnostic=%+v", event)
			}
		}
	}
	if count != 1 {
		t.Fatalf("terminal diagnostic count=%d", count)
	}
	if strings.Contains(logs.String(), failure.Error()) {
		t.Fatal("private error prose entered structural logs")
	}
}

func TestTypedChatFailuresKeepOutcomeWhenProseChanges(t *testing.T) {
	for _, row := range []struct {
		reason          chatFailureReason
		state, category string
	}{
		{chatContextLimit, "failed", "context_limit"},
		{chatRoundBudget, "interrupted", "round_budget"}, {chatToolBudget, "interrupted", "tool_budget"},
		{chatResearchBudget, "interrupted", "research_budget"}, {chatAccessRevoked, "failed", "access_revoked"},
	} {
		for _, message := range []string{"The saved allowance is exhausted.", "PRIVATE_SYNTHETIC alternate wording."} {
			t.Run(string(row.reason)+"/"+message, func(t *testing.T) {
				assertChatFailureOutcome(t, fmt.Errorf("operation: %w", chatFailureError(row.reason, message)), row.state, row.category)
			})
		}
	}
}
