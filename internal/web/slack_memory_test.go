package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"forgetmenote/internal/agentmemory"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
)

// slackMemoryRequests records what each model request offered and contained.
type slackMemoryRequests struct {
	mu       sync.Mutex
	prompts  []string
	tools    [][]string
	remember string // arguments for one remember call; empty answers at once
}

func (m *slackMemoryRequests) handle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []json.RawMessage `json:"messages"`
		Tools    []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, _ := json.Marshal(body.Messages)
	names := []string{}
	for _, tool := range body.Tools {
		names = append(names, tool.Function.Name)
	}
	m.prompts, m.tools = append(m.prompts, string(raw)), append(m.tools, names)
	if len(m.prompts) == 1 && m.remember != "" {
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "tool_calls", "message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call-memory", "type": "function", "function": map[string]string{"name": "remember", "arguments": m.remember}}}}}}})
		return
	}
	w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Private DM answer"},"finish_reason":"stop"}]}`))
}

// slackMemoryFixture maps Slack user U123 to alice, with her DM bound to an
// ordinary notebook, and seeds one item she wrote in her memory.
func slackMemoryFixture(t *testing.T, requests *slackMemoryRequests, memory bool) (*Server, *int) {
	t.Helper()
	app, _ := runnerFixture(t, requests.handle)
	configureSlackFixture(t, app)
	app.slack.config.DirectMessages = map[string]SlackChannel{"U123": app.slack.config.Channels["C123"]}
	settings, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{TimeZone: "UTC", MaintainMemory: memory})
	if err != nil {
		t.Fatal(err)
	}
	store, err := app.catalog.Access("alice", settings.Project, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(t.Context(), agentmemory.Path, []byte(agentmemory.Header("")+"\n## Facts\n\n- Alice owns the Denver office rollout.\n"), "", "alice"); err != nil {
		t.Fatal(err)
	}
	posts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" || r.URL.Path == "/chat.postEphemeral" {
			posts++
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)
	app.slack.endpoint = upstream.URL + "/chat.postEphemeral"
	return app, &posts
}

func TestSlackDirectMessagesUseAndKeepThePersonsMemory(t *testing.T) {
	requests := &slackMemoryRequests{remember: `{"items":[{"text":"Alice reviews service health every Monday.","kind":"fact"}]}`}
	app, posts := slackMemoryFixture(t, requests, true)
	signedSlackMessage(t, app, "EvMemory", "message", "D123", "", "123.456", "What do I usually check on Mondays?")
	if _, err := app.automations.Queue.RunNext(t.Context(), app.runAutomation); err != nil || *posts != 1 {
		t.Fatal("the DM was not answered", err, *posts)
	}
	if len(requests.prompts) != 2 || !strings.Contains(requests.prompts[0], "Alice owns the Denver office rollout.") || !strings.Contains(strings.Join(requests.tools[0], ","), "remember") {
		t.Fatalf("the DM run did not recall memory or offer remember: tools %v\n%v", requests.tools, requests.prompts)
	}

	settings, _ := app.catalog.Assistant("alice")
	store, err := app.catalog.Access("alice", settings.Project, "read")
	if err != nil {
		t.Fatal(err)
	}
	file, err := agentmemory.Read(t.Context(), store, "")
	if err != nil {
		t.Fatal(err)
	}
	var kept *agentmemory.Item
	for i, item := range file.Items {
		if item.Text == "Alice reviews service health every Monday." {
			kept = &file.Items[i]
		}
	}
	if kept == nil || kept.Via != "slack" || kept.From != "agent" || kept.Pinned {
		t.Fatalf("the remembered item lost its Slack label: %+v in %+v", kept, file.Items)
	}
	doc, _ := store.Read(t.Context(), agentmemory.Path, "")
	if !strings.Contains(string(doc.Content), "· via: slack") {
		t.Fatalf("the notebook does not show where the item came from:\n%s", doc.Content)
	}

	conversations, err := app.chat.Store.List("alice", "default")
	if err != nil {
		t.Fatal(err)
	}
	marked := false
	for _, c := range conversations {
		if c.Title == "Slack conversation" {
			conversation, err := app.chat.Store.Read("alice", "default", c.ID)
			marked = err == nil && chat.PersonalContext(conversation)
		}
	}
	if !marked {
		t.Fatal("the Slack conversation is not marked as carrying personal memory")
	}
}

func TestSlackMemoryStaysOutOfChannelsAndOffAccounts(t *testing.T) {
	for name, send := range map[string]struct {
		memory  bool
		kind    string
		channel string
	}{
		"memory off":      {false, "message", "D123"},
		"channel mention": {true, "app_mention", "C123"},
	} {
		t.Run(name, func(t *testing.T) {
			requests := &slackMemoryRequests{}
			app, posts := slackMemoryFixture(t, requests, send.memory)
			signedSlackMessage(t, app, "EvNoMemory", send.kind, send.channel, "", "123.456", "<@UBOT> What do I usually check on Mondays?")
			if _, err := app.automations.Queue.RunNext(t.Context(), app.runAutomation); err != nil || *posts != 1 {
				t.Fatal("the message was not answered", err, *posts)
			}
			if len(requests.prompts) != 1 || strings.Contains(requests.prompts[0], "Denver office rollout") || strings.Contains(strings.Join(requests.tools[0], ","), "remember") {
				t.Fatalf("memory reached the run: tools %v\n%v", requests.tools, requests.prompts)
			}
		})
	}
}
