package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/messaging"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSlackWritesRunOnlyAfterTheRequesterConfirms(t *testing.T) {
	step := 0
	// Models receive call_data_operation's arguments as a JSON-encoded string,
	// so strict tool schemas stay closed; send them the same way.
	operationArgs, _ := json.Marshal(map[string]any{"owner": "example-org", "repo": "example-service", "issue": map[string]any{"title": "TITLE_MARKER"}})
	callArgs, _ := json.Marshal(map[string]any{"project": "default", "connection": "github", "operation": "create_issue", "arguments": string(operationArgs)})
	proposeBody, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": "tool_calls", "message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "file", "type": "function", "function": map[string]any{"name": "call_data_operation", "arguments": string(callArgs)}}}}}}})
	propose := string(proposeBody)
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		step++
		switch step {
		case 1, 3, 5, 7:
			w.Write([]byte(propose))
		case 2, 4, 6, 8:
			var input struct {
				Messages []struct{ Role, Content string }
			}
			json.NewDecoder(r.Body).Decode(&input)
			if result := input.Messages[len(input.Messages)-1].Content; !strings.Contains(result, "confirm") || strings.Contains(result, `"error"`) {
				t.Errorf("model was not told the write awaits confirmation: %s", result)
			}
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"I drafted the issue."},"finish_reason":"stop"}]}`))
		default:
			t.Error("a confirmation reached the model")
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Unexpected."},"finish_reason":"stop"}]}`))
		}
	})
	writes := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if r.Method != "POST" || r.URL.Path != "/repos/example-org/example-service/issues" || body["title"] != "TITLE_MARKER" {
			t.Errorf("unexpected write %s %s %v", r.Method, r.URL.Path, body)
		}
		writes++
		w.WriteHeader(201)
		w.Write([]byte(`{"number":9,"html_url":"https://github.com/example-org/example-service/issues/9"}`))
	}))
	defer upstream.Close()
	m, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "github", Kind: "http", Endpoint: upstream.URL, ExternalProcessing: true, Operations: []connectors.Operation{{Name: "create_issue", ReadOnly: false, Method: "POST", Path: "/repos/{owner}/{repo}/issues", BodyArgument: "issue", InputSchema: json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"issue":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}},"required":["owner","repo","issue"],"additionalProperties":false}`)}}}}})
	if err != nil {
		t.Fatal(err)
	}
	app.dataConnections = m
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "github", Users: map[string][]string{"alice": {"create_issue"}}}); err != nil {
		t.Fatal(err)
	}
	configureSlackFixture(t, app)
	if err = app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "bob"}); err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", "bob", "read"); err != nil {
		t.Fatal(err)
	}
	app.slack.config.Users["U456"] = "bob"
	b := app.slack.config.Channels["C123"]
	b.Connections = []string{"github"}
	app.slack.config.Channels["C123"] = b
	transport := &fixtureChatTransport{}
	app.messageClients["slack"] = transport
	send := func(id, user, text string) string {
		t.Helper()
		d := messaging.Address{Client: "slack", Workspace: "T123", Channel: "C123", Thread: "1.0", Message: id, User: user}
		if err := app.enqueueMessage(t.Context(), messaging.Event{ID: id, Text: text, Address: d}); err != nil {
			t.Fatal(err)
		}
		if ran, err := app.automations.Queue.RunNext(t.Context(), app.runAutomation); err != nil || !ran {
			t.Fatal("run", ran, err)
		}
		if len(transport.replies) == 0 {
			runs, _ := app.automations.Queue.List()
			t.Fatalf("no reply: %+v", runs)
		}
		return transport.replies[len(transport.replies)-1].Text
	}
	reply := send("1.1", "U123", "File an issue for the wrong API key logging.")
	code := regexp.MustCompile("confirm ([A-Z2-7]{8})").FindStringSubmatch(reply)
	if writes != 0 || code == nil || !strings.Contains(reply, "TITLE_MARKER") || !strings.Contains(reply, "create_issue") {
		t.Fatalf("proposal ran or did not show the exact action: writes=%d reply=%q", writes, reply)
	}
	if reply = send("1.2", "U456", "confirm "+code[1]); writes != 0 || strings.Contains(reply, "issues/9") {
		t.Fatalf("another member confirmed the requester's action: %q", reply)
	}
	if reply = send("1.3", "U123", "<@UBOT> confirm "+code[1]); writes != 1 || !strings.Contains(reply, "https://github.com/example-org/example-service/issues/9") {
		t.Fatalf("confirmed write: writes=%d reply=%q", writes, reply)
	}
	if reply = send("1.4", "U123", "confirm "+code[1]); writes != 1 {
		t.Fatalf("a repeated confirmation wrote again: %q", reply)
	}
	proposal := func(id string) string {
		t.Helper()
		reply := send(id, "U123", "File it again.")
		m := regexp.MustCompile("confirm ([A-Z2-7]{8})").FindStringSubmatch(reply)
		if m == nil || m[1] == code[1] {
			t.Fatalf("new proposal: %q", reply)
		}
		return m[1]
	}
	expired := proposal("1.5")
	conversations, err := app.chat.Store.List("alice", "default")
	if err != nil || len(conversations) == 0 {
		t.Fatal(err)
	}
	change := func(code string, f func(*chat.Operation)) {
		for _, c := range conversations {
			app.chat.Store.Update("alice", "default", c.ID, func(v *chat.Conversation) error {
				for i := range v.Runs {
					for j := range v.Runs[i].Operations {
						if op := &v.Runs[i].Operations[j]; op.Confirmation == code {
							f(op)
						}
					}
				}
				return nil
			})
		}
	}
	change(expired, func(op *chat.Operation) {
		op.Result = json.RawMessage(strings.Replace(string(op.Result), `"expires":"2`, `"expires":"1`, 1))
	})
	if reply = send("1.6", "U123", "confirm "+expired); writes != 1 || !strings.Contains(reply, "expired") {
		t.Fatalf("an expired proposal: writes=%d reply=%q", writes, reply)
	}
	// A write interrupted after it was claimed has an unknown outcome.
	interrupted := proposal("1.7")
	change(interrupted, func(op *chat.Operation) { op.State = "running" })
	if reply = send("1.8", "U123", "confirm "+interrupted); writes != 1 || !strings.Contains(reply, "unknown") {
		t.Fatalf("an interrupted write: writes=%d reply=%q", writes, reply)
	}
	revoked := proposal("1.9")
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "github", Revision: 1, Users: map[string][]string{}}); err != nil {
		t.Fatal(err)
	}
	if reply = send("2.0", "U123", "confirm "+revoked); writes != 1 || !strings.Contains(reply, "no longer have access") {
		t.Fatalf("a revoked grant: writes=%d reply=%q", writes, reply)
	}
	if step != 8 {
		t.Fatal("model calls", step)
	}
}

func TestSlackTellsUnconfiguredRequestersInsteadOfStayingSilent(t *testing.T) {
	app := chatFixture(t)
	configureSlackFixture(t, app)
	var posts []map[string]any
	var methods []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		posts = append(posts, body)
		methods = append(methods, r.URL.Path)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	app.slack.endpoint = upstream.URL + "/chat.postEphemeral"
	send := func(id string, event map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"type": "event_callback", "team_id": "T123", "event_id": id, "event": event})
		stamp := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, []byte("fixture-signing"))
		mac.Write([]byte("v0:" + stamp + ":" + string(raw)))
		r := httptest.NewRequest("POST", "http://localhost/integrations/slack/events", strings.NewReader(string(raw)))
		r.Header.Set("X-Slack-Request-Timestamp", stamp)
		r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: HTTP %d %s", id, w.Code, w.Body)
		}
	}
	send("dm", map[string]any{"type": "message", "channel_type": "im", "channel": "DSTRANGER", "user": "USTRANGER", "text": "Hello?", "ts": "1.1"})
	send("mention", map[string]any{"type": "app_mention", "channel": "CUNSET", "user": "U123", "text": "<@UBOT> hello?", "ts": "2.1"})
	if runs, _ := app.automations.Queue.List(); len(runs) != 0 {
		t.Fatal("an unconfigured request was queued", runs)
	}
	if len(posts) != 2 || methods[0] != "/chat.postMessage" || posts[0]["channel"] != "DSTRANGER" || methods[1] != "/chat.postEphemeral" || posts[1]["user"] != "U123" {
		t.Fatalf("notices: %v %v", methods, posts)
	}
	for _, p := range posts {
		if !strings.Contains(p["text"].(string), "not set up") {
			t.Fatalf("unclear notice: %v", p["text"])
		}
	}
}

func TestSlackModelSeesTheWriteOperationsItMayPropose(t *testing.T) {
	var listed string
	step := 0
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		step++
		if step == 1 {
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"ops","type":"function","function":{"name":"list_data_operations","arguments":"{\"connection\":\"github\"}"}}]},"finish_reason":"tool_calls"}]}`))
			return
		}
		var input struct {
			Messages []struct{ Role, Content string }
		}
		json.NewDecoder(r.Body).Decode(&input)
		listed = input.Messages[len(input.Messages)-1].Content
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Listed."},"finish_reason":"stop"}]}`))
	})
	schema := json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"}},"required":["owner"],"additionalProperties":false}`)
	m, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "github", Kind: "http", Endpoint: "https://api.github.com", ExternalProcessing: true, Operations: []connectors.Operation{
		{Name: "get_issue", ReadOnly: true, Method: "GET", Path: "/repos/{owner}", InputSchema: schema},
		{Name: "create_issue", Method: "POST", Path: "/repos/{owner}/issues", InputSchema: schema},
		{Name: "update_issue", Method: "PATCH", Path: "/repos/{owner}/issues", InputSchema: schema},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	app.dataConnections = m
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "github", Users: map[string][]string{"alice": {"get_issue", "create_issue"}}}); err != nil {
		t.Fatal(err)
	}
	configureSlackFixture(t, app)
	b := app.slack.config.Channels["C123"]
	b.Connections = []string{"github"}
	app.slack.config.Channels["C123"] = b
	transport := &fixtureChatTransport{}
	app.messageClients["slack"] = transport
	d := messaging.Address{Client: "slack", Workspace: "T123", Channel: "C123", Thread: "1.0", Message: "1.1", User: "U123"}
	if err = app.enqueueMessage(t.Context(), messaging.Event{ID: "1.1", Text: "File a test issue.", Address: d}); err != nil {
		t.Fatal(err)
	}
	if ran, err := app.automations.Queue.RunNext(t.Context(), app.runAutomation); err != nil || !ran {
		t.Fatal("run", ran, err)
	}
	// A write the requester may confirm must be discoverable, or the model
	// never proposes it. Ungranted operations stay hidden.
	if !strings.Contains(listed, `"get_issue"`) || !strings.Contains(listed, `"create_issue"`) || strings.Contains(listed, `"update_issue"`) {
		t.Fatalf("operations shown to the model: %s", listed)
	}
}

func TestSlackButtonsConfirmOrCancelOnlyTheRequestersWrite(t *testing.T) {
	operationArgs, _ := json.Marshal(map[string]any{"owner": "example-org", "repo": "example-service", "issue": map[string]any{"title": "TITLE_MARKER"}})
	callArgs, _ := json.Marshal(map[string]any{"project": "default", "connection": "github", "operation": "create_issue", "arguments": string(operationArgs)})
	proposeBody, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": "tool_calls", "message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "file", "type": "function", "function": map[string]any{"name": "call_data_operation", "arguments": string(callArgs)}}}}}}})
	step := 0
	app, _ := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if step++; step%2 == 1 {
			w.Write(proposeBody)
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"I drafted the issue."},"finish_reason":"stop"}]}`))
	})
	writes := 0
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writes++
		w.WriteHeader(201)
		w.Write([]byte(`{"number":9,"html_url":"https://github.com/example-org/example-service/issues/9"}`))
	}))
	defer github.Close()
	m, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "github", Kind: "http", Endpoint: github.URL, ExternalProcessing: true, Operations: []connectors.Operation{{Name: "create_issue", ReadOnly: false, Method: "POST", Path: "/repos/{owner}/{repo}/issues", BodyArgument: "issue", InputSchema: json.RawMessage(`{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"issue":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}},"required":["owner","repo","issue"],"additionalProperties":false}`)}}}}})
	if err != nil {
		t.Fatal(err)
	}
	app.dataConnections = m
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "github", Users: map[string][]string{"alice": {"create_issue"}}}); err != nil {
		t.Fatal(err)
	}
	configureSlackFixture(t, app)
	if err = app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "bob"}); err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", "bob", "read"); err != nil {
		t.Fatal(err)
	}
	app.slack.config.Users["U456"] = "bob"
	b := app.slack.config.Channels["C123"]
	b.Connections = []string{"github"}
	app.slack.config.Channels["C123"] = b

	type post struct {
		path string
		body map[string]any
	}
	var mu sync.Mutex
	var posts []post
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		posts = append(posts, post{r.URL.Path, body})
		mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	}))
	defer slack.Close()
	app.slack.endpoint = slack.URL + "/chat.postEphemeral"
	sent := func(path string) []post {
		mu.Lock()
		defer mu.Unlock()
		var out []post
		for _, p := range posts {
			if p.path == path {
				out = append(out, p)
			}
		}
		return out
	}
	lastReply := func() string {
		t.Helper()
		replies := sent("/chat.postEphemeral")
		if len(replies) == 0 {
			t.Fatal("no reply")
		}
		text, _ := replies[len(replies)-1].body["text"].(string)
		return text
	}
	run := func() bool {
		t.Helper()
		ran, err := app.automations.Queue.RunNext(t.Context(), app.runAutomation)
		if err != nil {
			t.Fatal(err)
		}
		return ran
	}
	// propose asks for a write and returns the button values offered to the requester.
	propose := func(message string) (string, string) {
		t.Helper()
		d := messaging.Address{Client: "slack", Workspace: "T123", Channel: "C123", Thread: "1.0", Message: message, User: "U123"}
		if err := app.enqueueMessage(t.Context(), messaging.Event{ID: message, Text: "File an issue for the wrong API key logging.", Address: d}); err != nil {
			t.Fatal(err)
		}
		if !run() {
			t.Fatal("proposal did not run")
		}
		replies := sent("/chat.postEphemeral")
		buttons := replies[len(replies)-1].body
		raw, _ := json.Marshal(buttons["blocks"])
		var blocks []struct {
			Type     string
			Elements []struct {
				ActionID string `json:"action_id"`
				Value    string
			}
		}
		json.Unmarshal(raw, &blocks)
		values := map[string]string{}
		for _, block := range blocks {
			for _, e := range block.Elements {
				if block.Type == "actions" {
					values[e.ActionID] = e.Value
				}
			}
		}
		if buttons["user"] != "U123" || buttons["thread_ts"] != "1.0" || values["confirm_write"] == "" || values["cancel_write"] == "" {
			t.Fatalf("the requester was not offered Confirm and Cancel buttons: %v", buttons)
		}
		return values["confirm_write"], values["cancel_write"]
	}
	click := func(user, message, action, value, signature string) int {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"type": "block_actions", "team": map[string]any{"id": "T123"}, "user": map[string]any{"id": user, "team_id": "T123"},
			"channel": map[string]any{"id": "C123"}, "container": map[string]any{"type": "message", "message_ts": message, "channel_id": "C123", "is_ephemeral": true},
			"response_url": slack.URL + "/actions/T123/1/hook", "actions": []any{map[string]any{"type": "button", "action_id": action, "block_id": "write", "value": value, "action_ts": "9.9"}}})
		body := url.Values{"payload": {string(payload)}}.Encode()
		stamp := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, []byte(signature))
		mac.Write([]byte("v0:" + stamp + ":" + body))
		r := httptest.NewRequest("POST", "http://localhost/integrations/slack/interactions", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("X-Slack-Request-Timestamp", stamp)
		r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		return w.Code
	}

	confirm, _ := propose("1.1")
	if code := click("U123", "1.2", "confirm_write", confirm, "forged-secret"); code != 401 || run() || writes != 0 {
		t.Fatalf("an unsigned click was accepted: HTTP %d, writes=%d", code, writes)
	}
	// Bob cannot see Alice's ephemeral buttons, but a click naming them as
	// another member must still run nothing.
	if code := click("U456", "1.2", "confirm_write", confirm, "fixture-signing"); code != 200 || !run() || writes != 0 || !strings.Contains(lastReply(), "Nothing ran") {
		t.Fatalf("another member's click: HTTP %d writes=%d reply=%q", code, writes, lastReply())
	}
	if code := click("U123", "1.2", "confirm_write", confirm, "fixture-signing"); code != 200 {
		t.Fatalf("requester's click: HTTP %d", code)
	}
	hooks := sent("/actions/T123/1/hook")
	if len(hooks) == 0 || hooks[len(hooks)-1].body["replace_original"] != true || strings.Contains(fmt.Sprint(hooks[len(hooks)-1].body), "confirm_write") {
		t.Fatalf("the clicked buttons were not replaced: %v", hooks)
	}
	if !run() || writes != 1 || !strings.Contains(lastReply(), "https://github.com/example-org/example-service/issues/9") {
		t.Fatalf("confirmed write: writes=%d reply=%q", writes, lastReply())
	}
	// A double click is the same request, not a second confirmation.
	if code := click("U123", "1.2", "confirm_write", confirm, "fixture-signing"); code != 200 || run() || writes != 1 {
		t.Fatalf("a repeated click: HTTP %d writes=%d", code, writes)
	}

	_, cancel := propose("1.3")
	if code := click("U123", "1.4", "cancel_write", cancel, "fixture-signing"); code != 200 || !run() || writes != 1 || !strings.Contains(lastReply(), "Cancelled") {
		t.Fatalf("cancel: HTTP %d writes=%d reply=%q", code, writes, lastReply())
	}
	var value struct{ Code string }
	json.Unmarshal([]byte(cancel), &value)
	d := messaging.Address{Client: "slack", Workspace: "T123", Channel: "C123", Thread: "1.0", Message: "1.5", User: "U123"}
	if err := app.enqueueMessage(t.Context(), messaging.Event{ID: "1.5", Text: "confirm " + value.Code, Address: d}); err != nil {
		t.Fatal(err)
	}
	if !run() || writes != 1 || !strings.Contains(lastReply(), "cancelled") {
		t.Fatalf("a cancelled change ran: writes=%d reply=%q", writes, lastReply())
	}
}
