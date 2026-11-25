package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func runnerFixture(t *testing.T, handler http.HandlerFunc) (*Server, chat.Conversation) {
	t.Helper()
	app := chatFixture(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	file := filepath.Join(t.TempDir(), "chat.json")
	raw, _ := json.Marshal(map[string]any{"connections": []any{map[string]any{"id": "local", "label": "Synthetic", "format": "chat-completions", "baseURL": srv.URL, "models": []string{"synthetic"}, "local": true, "capabilities": map[string]bool{"tools": true}, "limits": map[string]int{"runSeconds": 10}}}})
	if e := os.WriteFile(file, raw, 0600); e != nil {
		t.Fatal(e)
	}
	if e := app.ConfigureChat(file); e != nil {
		t.Fatal(e)
	}
	w := proxyCall(app, "alice", "POST", "/p/default/api/chat", `{"connection":"local","model":"synthetic","mode":"ask"}`)
	var c chat.Conversation
	if json.Unmarshal(w.Body.Bytes(), &c) != nil || c.ID == "" {
		t.Fatal(w.Body.String())
	}
	return app, c
}
func waitChatRun(t *testing.T, app *Server, id string) chat.Conversation {
	t.Helper()
	c, err := app.chat.Store.Read("alice", "default", id)
	if err != nil {
		t.Fatal(err)
	}
	opts := c.Options
	if len(c.Runs) > 0 {
		opts = c.Runs[len(c.Runs)-1].Options
	}
	connection, err := app.chat.Snapshot().Config.Resolve(opts.Connection, opts.Model)
	if err != nil {
		t.Fatal(err)
	}
	// The worker owns the configured execution deadline. Allow bounded time
	// afterward for its terminal state to be committed, including under -race.
	until := time.Now().Add(time.Duration(connection.Limits.RunSeconds)*time.Second + 2*time.Second)
	if deadline, ok := t.Deadline(); ok && deadline.Before(until) {
		until = deadline.Add(-100 * time.Millisecond)
	}
	for time.Now().Before(until) {
		c, err = app.chat.Store.Read("alice", "default", id)
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Runs) > 0 && !chat.Active(c.Runs[len(c.Runs)-1].State) {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("chat did not settle after its configured %ds allowance: runs=%+v", connection.Limits.RunSeconds, c.Runs)
	return chat.Conversation{}
}
func TestChatRunDurableAnswerAndSubmissionReplay(t *testing.T) {
	var requests atomic.Int32
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid provider request")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"A durable answer."},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":5}}`)
	})
	body := `{"submissionId":"submit-1","text":"What evidence is available?","options":{"mode":"ask"}}`
	path := "/p/default/api/chat/" + c.ID + "/turns"
	w := proxyCall(app, "alice", "POST", path, body)
	if w.Code != 200 {
		t.Fatalf("send failed: %d %s", w.Code, w.Body)
	}
	done := waitChatRun(t, app, c.ID)
	if len(done.Turns) != 2 || done.Turns[1].Content != "A durable answer." || done.Runs[0].State != "completed" || done.Runs[0].Usage.InputTokens != 30 {
		t.Fatalf("wrong durable result %+v", done)
	}
	w = proxyCall(app, "alice", "POST", path, body)
	if w.Code != 200 || requests.Load() != 1 {
		t.Fatalf("submission repeated provider work %d %d", w.Code, requests.Load())
	}
	w = proxyCall(app, "alice", "POST", path, strings.Replace(body, "available?", "missing?", 1))
	if w.Code != 409 {
		t.Fatalf("changed replay accepted %d", w.Code)
	}
	if len(done.Events) < 3 {
		t.Fatal("durable stream events missing")
	}
}
func TestChatAskCannotMutate(t *testing.T) {
	var requests atomic.Int32
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var body any
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"evil","type":"function","function":{"name":"save_note","arguments":"{\"path\":\"notes/injected.md\",\"content\":\"injected\",\"revision\":\"\"}"}}]},"finish_reason":"tool_calls"}]}`)
	})
	if e := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); e != nil {
		t.Fatal(e)
	}
	w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"ask-1","text":"Read notes","options":{"mode":"ask"}}`)
	if w.Code != 200 {
		t.Fatalf("send %d %s", w.Code, w.Body)
	}
	done := waitChatRun(t, app, c.ID)
	if done.Runs[0].State != "failed" {
		t.Fatalf("unexpected state %+v", done.Runs)
	}
	store, _ := app.catalog.Access("alice", "default", "read")
	if _, e := store.Read(t.Context(), "notes/injected.md", ""); e == nil {
		t.Fatal("Ask mutated notebook")
	}
}

func TestChatAnswerWithinConfiguredDeadlineOutlastsShortFixtureWait(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		timer := time.NewTimer(6 * time.Second)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Answer within the configured ten-second allowance."},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	})
	response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"allowed-deadline","text":"Wait for the synthetic answer."}`)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	completed := waitChatRun(t, app, c.ID)
	if completed.Runs[0].State != "completed" {
		t.Fatalf("response within configured allowance rejected: %s %s", completed.Runs[0].State, completed.Runs[0].Error)
	}
}

func TestChatConfiguredDeadlineCancelsBlockedProvider(t *testing.T) {
	cancelled := make(chan struct{})
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		<-r.Context().Done()
		close(cancelled)
	})
	app.chat.Mu.Lock()
	config := *app.chat.Config
	config.Connections = append([]chat.Connection(nil), config.Connections...)
	config.Connections[0].Limits.RunSeconds = 1
	app.chat.Config = &config
	app.chat.Mu.Unlock()
	started := time.Now()
	response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"short-deadline","text":"Wait for the blocked synthetic provider."}`)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	completed := waitChatRun(t, app, c.ID)
	if completed.Runs[0].State != "interrupted" || time.Since(started) > 3*time.Second {
		t.Fatalf("execution exceeded its one-second deadline: state=%s elapsed=%s", completed.Runs[0].State, time.Since(started))
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("provider request was not cancelled at the configured deadline")
	}
}
