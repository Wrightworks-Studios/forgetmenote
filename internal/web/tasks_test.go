package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/taskqueue"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newTaskFixture enables task queues for the Slack workspace T123, where alice
// is member UALICE and bob is UBOB. carol has an account and no member.
func newTaskFixture(t *testing.T) *slackLinkFixture {
	t.Helper()
	f := newSlackLinkFixture(t)
	app := f.app
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "carol"}); err != nil {
		t.Fatal(err)
	}
	for user, member := range map[string]string{"alice": "UALICE", "bob": "UBOB"} {
		if err := app.catalog.LinkMessageUser(user, "slack", "T123", member); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := app.catalog.SetTaskQueues(catalog.Bootstrap, catalog.TaskQueues{Client: "slack"}); err != nil {
		t.Fatal(err)
	}
	return f
}

// taskClients connects one MCP session per user to a single server.
func taskClients(t *testing.T, app *Server, users ...string) []*mcp.ClientSession {
	t.Helper()
	ts := httptest.NewServer(app)
	t.Cleanup(ts.Close)
	app.origin = ts.URL
	out := []*mcp.ClientSession{}
	for _, user := range users {
		scope := "notes:read notes:write"
		token := "tasks-" + digest(user)
		app.oauth.Mu.Lock()
		app.oauth.Access[digest(token)] = withOAuthFamily(accessGrant{Subject: user, Client: "tasks-client", Scope: scope, Resource: ts.URL + "/mcp", Expires: time.Now().Add(time.Hour).Unix(), Epoch: app.catalog.Epoch(user)})
		app.oauth.Mu.Unlock()
		client := mcp.NewClient(&mcp.Implementation{Name: "tasks", Version: "1"}, nil)
		session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: token}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { session.Close() })
		out = append(out, session)
	}
	return out
}

func decodeTask[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return out
}

// postsTo returns the messages sent to a member's direct messages.
func (f *slackLinkFixture) postsTo(member string) []map[string]any {
	out := []map[string]any{}
	for _, p := range f.posts {
		if p["channel"] == member {
			out = append(out, p)
		}
	}
	return out
}

func postButtons(post map[string]any) []map[string]any {
	out := []map[string]any{}
	blocks, _ := post["blocks"].([]any)
	for _, block := range blocks {
		b, _ := block.(map[string]any)
		if accessory, ok := b["accessory"].(map[string]any); ok {
			out = append(out, accessory)
		}
		elements, _ := b["elements"].([]any)
		for _, e := range elements {
			if button, ok := e.(map[string]any); ok {
				out = append(out, button)
			}
		}
	}
	return out
}

func TestTaskQueuesAreADropBoxThroughMCP(t *testing.T) {
	f := newTaskFixture(t)
	app := f.app
	clients := taskClients(t, app, "alice", "bob", "carol")
	alice, bob, carol := clients[0], clients[1], clients[2]

	added := decodeTask[taskqueue.View](t, actionCall(t, bob, "add_task", map[string]any{"person": "<@UALICE>", "text": "Review the Q4 pricing deck", "due": "2026-12-01"}, false))
	if added.Owner.SlackUser != "UALICE" || added.Requester.SlackUser != "UBOB" || !added.Requester.You || added.Revision == "" {
		t.Fatalf("added = %+v", added)
	}
	// The owner hears at once, with a button to close it.
	posts := f.postsTo("UALICE")
	if len(posts) != 1 || !strings.Contains(posts[0]["text"].(string), "Review the Q4 pricing deck") {
		t.Fatalf("owner notices = %v", f.replies)
	}
	if buttons := postButtons(posts[0]); len(buttons) != 1 || buttons[0]["action_id"] != taskqueue.DoneAction {
		t.Fatalf("the notice offers %v", buttons)
	}

	mine := decodeTask[taskqueue.List](t, actionCall(t, alice, "list_tasks", map[string]any{}, false))
	if len(mine.Mine) != 1 || mine.Mine[0].ID != added.ID || len(mine.Filed) != 0 {
		t.Fatalf("alice's queue = %+v", mine)
	}
	filed := decodeTask[taskqueue.List](t, actionCall(t, bob, "list_tasks", map[string]any{}, false))
	if len(filed.Mine) != 0 || len(filed.Filed) != 1 || filed.Filed[0].ID != added.ID {
		t.Fatalf("bob's filed tasks = %+v", filed)
	}
	// Someone else sees nothing and changes nothing.
	if other := decodeTask[taskqueue.List](t, actionCall(t, carol, "list_tasks", map[string]any{}, false)); len(other.Mine)+len(other.Filed) != 0 {
		t.Fatalf("carol sees %+v", other)
	}
	actionCall(t, carol, "update_task", map[string]any{"id": added.ID, "revision": added.Revision, "outcome": "done"}, true)
	actionCall(t, carol, "task_history", map[string]any{"id": added.ID}, true)
	// A colleague's task gets no priority from its requester, who cannot close it.
	actionCall(t, bob, "update_task", map[string]any{"id": added.ID, "revision": added.Revision, "priority": "high"}, true)
	actionCall(t, bob, "update_task", map[string]any{"id": added.ID, "revision": added.Revision, "outcome": "done"}, true)
	// The requester edits; an edit from a stale revision is refused.
	text := "Review the Q4 pricing deck and the appendix"
	edited := decodeTask[taskqueue.Result](t, actionCall(t, bob, "update_task", map[string]any{"id": added.ID, "revision": added.Revision, "text": text}, false))
	if edited.Task == nil || edited.Task.Text != text || edited.Task.Revision == added.Revision {
		t.Fatalf("edit = %+v", edited)
	}
	actionCall(t, alice, "update_task", map[string]any{"id": added.ID, "revision": added.Revision, "outcome": "done"}, true)
	actionCall(t, alice, "update_task", map[string]any{"id": added.ID, "revision": edited.Task.Revision, "outcome": "not_applicable"}, true)

	// The owner closes it; the requester hears, and the task leaves the queue.
	closed := decodeTask[taskqueue.Result](t, actionCall(t, alice, "update_task", map[string]any{"id": added.ID, "revision": edited.Task.Revision, "outcome": "done"}, false))
	if closed.Closed != "done" || closed.Task != nil {
		t.Fatalf("close = %+v", closed)
	}
	if posts = f.postsTo("UBOB"); len(posts) != 1 || !strings.Contains(posts[0]["text"].(string), "marked your task done") {
		t.Fatalf("requester notices = %v", f.replies)
	}
	if mine = decodeTask[taskqueue.List](t, actionCall(t, alice, "list_tasks", map[string]any{}, false)); len(mine.Mine) != 0 {
		t.Fatalf("a closed task stayed: %+v", mine)
	}
	for _, client := range []string{"alice", "bob"} {
		session := alice
		if client == "bob" {
			session = bob
		}
		history := decodeTask[[]taskqueue.HistoryEntry](t, actionCall(t, session, "task_history", map[string]any{"id": added.ID}, false))
		if len(history) != 3 || !strings.HasPrefix(history[0].Change, "done by ") || !strings.HasPrefix(history[1].Change, "changed text by ") || !strings.HasPrefix(history[2].Change, "added to ") {
			t.Fatalf("%s reads history %+v", client, history)
		}
	}

	// Only task actions write the notebook, even for an admin.
	_, store, _ := app.catalog.TaskQueues()
	if _, err := app.catalog.Access(catalog.Bootstrap, catalog.TaskQueuesProject, "edit"); !errors.Is(err, workspace.ErrReadOnly) {
		t.Fatalf("an admin may edit the task queues: %v", err)
	}
	book, err := taskqueue.ReadBook(t.Context(), store)
	if err != nil || len(book.Queues) != 1 {
		t.Fatalf("queues = %+v %v", book.Queues, err)
	}
}

func TestRecurringTasksMoveToTheirNextDate(t *testing.T) {
	f := newTaskFixture(t)
	alice := actionClient(t, f.app, "alice", "notes:read notes:write")
	actionCall(t, alice, "add_task", map[string]any{"text": "Email the Acme client", "every": "quarterly"}, true)
	actionCall(t, alice, "add_task", map[string]any{"text": "Email the Acme client", "due": "2026-01-31", "lead": 10}, true)
	added := decodeTask[taskqueue.View](t, actionCall(t, alice, "add_task", map[string]any{"text": "Email the Acme client", "due": "2026-01-31", "every": "quarterly", "priority": "high", "lead": 10}, false))
	if added.Every == nil || added.Priority != "high" || added.Lead != 10 || len(added.NextDates) != 2 || added.NextDates[0] != "2026-01-31" || added.NextDates[1] != "2026-04-30" {
		t.Fatalf("added = %+v", added)
	}
	if len(f.postsTo("UALICE")) != 0 {
		t.Fatal("a task you gave yourself sent you a message")
	}
	done := decodeTask[taskqueue.Result](t, actionCall(t, alice, "update_task", map[string]any{"id": added.ID, "revision": added.Revision, "outcome": "done"}, false))
	if done.Occurrence == nil || done.Task == nil || done.Task.Due != done.Occurrence.Next || done.Occurrence.Missed == 0 || done.Closed != "" {
		t.Fatalf("closing an occurrence = %+v", done)
	}
	// Stopping the series needs a reason and keeps the task.
	actionCall(t, alice, "update_task", map[string]any{"id": added.ID, "revision": done.Task.Revision, "every": ""}, true)
	shorter := decodeTask[taskqueue.Result](t, actionCall(t, alice, "update_task", map[string]any{"id": added.ID, "revision": done.Task.Revision, "lead": 3}, false))
	if shorter.Task == nil || shorter.Task.Lead != 3 {
		t.Fatalf("lead change = %+v", shorter)
	}
	stopped := decodeTask[taskqueue.Result](t, actionCall(t, alice, "update_task", map[string]any{"id": added.ID, "revision": shorter.Task.Revision, "every": "", "reason": "Acme signed a yearly contract"}, false))
	if stopped.Task == nil || stopped.Task.Every != nil || stopped.Task.Lead != 0 {
		t.Fatalf("stop = %+v", stopped)
	}
	history := decodeTask[[]taskqueue.HistoryEntry](t, actionCall(t, alice, "task_history", map[string]any{"id": added.ID}, false))
	if len(history) != 4 || !strings.Contains(history[1].Change, "lead time") || !strings.Contains(history[2].Change, "done for ") || !strings.Contains(history[2].Change, "next "+done.Occurrence.Next) {
		t.Fatalf("history = %+v", history)
	}
}

// memberDM sends a direct message from a Slack member and runs it.
func (f *slackLinkFixture) memberDM(t *testing.T, user, id, text string) string {
	t.Helper()
	before := len(f.replies)
	d := messaging.Address{Client: "slack", Workspace: "T123", Channel: "D" + user, Thread: id, Message: id, User: user, Direct: true}
	if err := f.app.messaging.EnqueueMessage(t.Context(), messaging.Event{ID: id, Text: text, Address: d}); err != nil {
		t.Fatal(err)
	}
	if ran, err := f.app.automations.Queue.RunNext(t.Context(), f.app.runAutomation); err != nil || !ran || len(f.replies) <= before {
		t.Fatalf("the DM %q was not answered: %v %v %v", text, ran, err, f.replies[before:])
	}
	for _, reply := range f.replies[before:] {
		if strings.HasPrefix(reply, "D"+user+": ") {
			return reply
		}
	}
	t.Fatalf("no reply in the DM: %v", f.replies[before:])
	return ""
}

// interact signs an interaction payload from Slack and serves it.
func (f *slackLinkFixture) interact(t *testing.T, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(payload)
	body := url.Values{"payload": {string(raw)}}.Encode()
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte("fixture-signing"))
	mac.Write([]byte("v0:" + stamp + ":" + body))
	origin := f.app.origin
	if origin == "" {
		origin = "http://localhost"
	}
	r := httptest.NewRequest("POST", origin+"/integrations/slack/interactions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Slack-Request-Timestamp", stamp)
	r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	f.app.ServeHTTP(w, r)
	return w
}

func buttonPress(user, message string, button map[string]any, response string) map[string]any {
	return map[string]any{"type": "block_actions", "team": map[string]any{"id": "T123"}, "user": map[string]any{"id": user, "team_id": "T123"},
		"channel": map[string]any{"id": "D" + user}, "container": map[string]any{"type": "message", "message_ts": message, "channel_id": "D" + user},
		"trigger_id": "trigger-" + message, "response_url": response, "actions": []any{map[string]any{"type": "button", "action_id": button["action_id"], "block_id": "b", "value": button["value"], "action_ts": "9.9"}}}
}

// press signs a button press from a member and returns the replacing text.
func (f *slackLinkFixture) press(t *testing.T, user, message string, button map[string]any) string {
	t.Helper()
	before := len(f.responses)
	w := f.interact(t, buttonPress(user, message, button, f.upstream+"/actions/T123/1/hook"))
	if w.Code != 200 || len(f.responses) != before+1 {
		t.Fatalf("press: HTTP %d %s, responses %v", w.Code, w.Body, f.responses[before:])
	}
	return f.responses[before]
}

func TestSlackMembersGiveAndCloseTasksWithoutTheModel(t *testing.T) {
	f := newTaskFixture(t)
	for _, text := range []string{"tasks for next week are piling up", "task alice send the deck", "can you task <@UALICE> with it?"} {
		if taskqueue.Command(text) {
			t.Errorf("%q was taken as a task command", text)
		}
	}
	reply := f.memberDM(t, "UBOB", "1.1", "task <@UALICE> Send the Denver lease to legal due 2026-12-05")
	if !strings.Contains(reply, "Added to") || !strings.Contains(reply, "Due 2026-12-05") {
		t.Fatalf("requester's confirmation = %q", reply)
	}
	posts := f.postsTo("UALICE")
	if len(posts) != 1 || !strings.Contains(posts[0]["text"].(string), "Send the Denver lease to legal") {
		t.Fatalf("owner notices = %v", f.replies)
	}
	done := postButtons(posts[0])[0]
	if reply = f.memberDM(t, "UBOB", "1.2", "tasks"); !strings.Contains(reply, "Tasks you gave others") || !strings.Contains(reply, "Send the Denver lease") {
		t.Fatalf("bob's list = %q", reply)
	}
	f.memberDM(t, "UALICE", "1.3", "tasks")
	list := f.posts[len(f.posts)-1]
	if buttons := postButtons(list); len(buttons) != 1 || buttons[0]["action_id"] != taskqueue.DoneAction {
		t.Fatalf("alice's list offers %v", buttons)
	}

	// Only the owner's press closes it: not the requester's, nor a stranger's.
	if text := f.press(t, "UBOB", "2.1", done); !strings.Contains(text, "Only the queue's owner") {
		t.Fatalf("bob's press of alice's button: %q", text)
	}
	if text := f.press(t, "USTRANGER", "2.1", done); !strings.Contains(text, "not in your queue") {
		t.Fatalf("a stranger's press: %q", text)
	}
	if text := f.press(t, "UALICE", "2.2", done); !strings.HasPrefix(text, "Done.") {
		t.Fatalf("alice's press: %q", text)
	}
	if posts = f.postsTo("UBOB"); len(posts) == 0 || !strings.Contains(posts[len(posts)-1]["text"].(string), "marked your task done") {
		t.Fatalf("the requester was not told: %v", f.replies)
	}
	if text := f.press(t, "UALICE", "2.2", done); !strings.Contains(text, "No open task") {
		t.Fatalf("a second press: %q", text)
	}
	if reply = f.memberDM(t, "UALICE", "1.4", "tasks"); !strings.Contains(reply, "no open tasks") {
		t.Fatalf("alice's list after closing = %q", reply)
	}
	if reply = f.memberDM(t, "UBOB", "1.5", "task me Draft holiday copy"); !strings.Contains(reply, "Added to your queue") {
		t.Fatalf("own task = %q", reply)
	}
}

func TestAddTaskGivesEachPersonTheirOwnCopy(t *testing.T) {
	f := newTaskFixture(t)
	clients := taskClients(t, f.app, "alice", "bob", "carol")
	alice, bob, carol := clients[0], clients[1], clients[2]

	// Nothing is filed when any name is wrong, both forms are given, or too
	// many people are named.
	actionCall(t, bob, "add_task", map[string]any{"people": []string{"<@UALICE>", "nobody"}, "text": "Count the stock"}, true)
	actionCall(t, bob, "add_task", map[string]any{"person": "carol", "people": []string{"<@UALICE>"}, "text": "Count the stock"}, true)
	crowd := []string{}
	for range taskqueue.PeopleLimit + 1 {
		crowd = append(crowd, "carol")
	}
	actionCall(t, bob, "add_task", map[string]any{"people": crowd, "text": "Count the stock"}, true)
	if mine := decodeTask[taskqueue.List](t, actionCall(t, alice, "list_tasks", map[string]any{}, false)); len(mine.Mine) != 0 {
		t.Fatalf("a refused request filed %+v", mine.Mine)
	}

	// Each person named once gets their own copy, in the order named.
	added := decodeTask[taskqueue.Added](t, actionCall(t, bob, "add_task", map[string]any{"people": []string{"<@UALICE>", "carol", "UALICE"}, "text": "Count the stock", "due": "2026-12-01"}, false))
	if len(added.Tasks) != 2 || added.ID != added.Tasks[0].ID || added.Tasks[0].ID == added.Tasks[1].ID {
		t.Fatalf("added = %+v", added)
	}
	if added.Tasks[0].Owner.SlackUser != "UALICE" || added.Tasks[1].Owner.SlackUser != "" || added.Tasks[1].Owner.ID == added.Tasks[0].Owner.ID || !added.Tasks[1].Requester.You || added.Tasks[1].Due != "2026-12-01" {
		t.Fatalf("copies = %+v", added.Tasks)
	}
	if posts := f.postsTo("UALICE"); len(posts) != 1 || !strings.Contains(posts[0]["text"].(string), "Count the stock") {
		t.Fatalf("alice's notices = %v", f.replies)
	}

	// Each owner sees and closes only their own; the requester follows both.
	first := decodeTask[taskqueue.List](t, actionCall(t, alice, "list_tasks", map[string]any{}, false))
	second := decodeTask[taskqueue.List](t, actionCall(t, carol, "list_tasks", map[string]any{}, false))
	if len(first.Mine) != 1 || first.Mine[0].ID != added.Tasks[0].ID || len(second.Mine) != 1 || second.Mine[0].ID != added.Tasks[1].ID {
		t.Fatalf("alice has %+v, carol has %+v", first.Mine, second.Mine)
	}
	actionCall(t, alice, "update_task", map[string]any{"id": first.Mine[0].ID, "revision": first.Mine[0].Revision, "outcome": "done"}, false)
	if filed := decodeTask[taskqueue.List](t, actionCall(t, bob, "list_tasks", map[string]any{}, false)); len(filed.Filed) != 1 || filed.Filed[0].ID != added.Tasks[1].ID {
		t.Fatalf("after alice closed hers, bob follows %+v", filed.Filed)
	}
	history := decodeTask[[]taskqueue.HistoryEntry](t, actionCall(t, carol, "task_history", map[string]any{"id": added.Tasks[1].ID}, false))
	if len(history) != 1 || !strings.HasPrefix(history[0].Change, "added to "+added.Tasks[1].Owner.ID+" by ") {
		t.Fatalf("carol's copy has history %+v", history)
	}
}

func TestSlackMembersGiveOneTaskToSeveralPeople(t *testing.T) {
	f := newTaskFixture(t)
	for text, want := range map[string][]string{
		"task <@UALICE>, <@UDAN|dan> and me Check the proofs due 2026-12-05": {"<@UALICE>", "<@UDAN|dan>", "me", "Check the proofs", "2026-12-05"},
		"task <@UALICE> <@UDAN> Check the proofs":                            {"<@UALICE>", "<@UDAN>", "Check the proofs", ""},
		"task <@UALICE> and review the deck":                                 {"<@UALICE>", "and review the deck", ""},
		"task me meet the vendor":                                            {"me", "meet the vendor", ""},
	} {
		people, task, due, ok := taskqueue.AddParts(text)
		if got := append(people, task, due); !ok || !slices.Equal(got, want) {
			t.Errorf("%q reads as %q", text, got)
		}
	}

	reply := f.memberDM(t, "UBOB", "3.1", "task <@UALICE>, <@UDAN> and me Check the proofs due 2026-12-05")
	if !strings.Contains(reply, "Added a copy for") || !strings.Contains(reply, "you") || !strings.Contains(reply, "Due 2026-12-05") {
		t.Fatalf("requester's confirmation = %q", reply)
	}
	for _, member := range []string{"UALICE", "UDAN"} {
		posts := f.postsTo(member)
		if len(posts) != 1 || !strings.Contains(posts[0]["text"].(string), "Check the proofs") || len(postButtons(posts[0])) != 1 {
			t.Fatalf("%s's notices = %v", member, f.replies)
		}
	}
	if posts := f.postsTo("UBOB"); len(posts) != 0 {
		t.Fatalf("bob was told about his own copy: %v", f.replies)
	}
	// The copies bob gave are one line naming both owners.
	if reply = f.memberDM(t, "UBOB", "3.2", "tasks"); !strings.Contains(reply, "You have 1 open task") || strings.Count(reply, "Check the proofs") != 1 || !strings.Contains(reply, "UDAN") || !strings.Contains(reply, " and ") {
		t.Fatalf("bob's list = %q", reply)
	}

	// Closing one copy leaves the others open.
	done := postButtons(f.postsTo("UALICE")[0])[0]
	if text := f.press(t, "UALICE", "4.1", done); !strings.HasPrefix(text, "Done.") {
		t.Fatalf("alice's press: %q", text)
	}
	if reply = f.memberDM(t, "UDAN", "3.3", "tasks"); !strings.Contains(reply, "You have 1 open task") {
		t.Fatalf("dan's list after alice closed hers = %q", reply)
	}

	// Naming someone twice gives them one copy.
	if reply = f.memberDM(t, "UBOB", "3.4", "task <@UALICE> <@UALICE> Sign the lease"); !strings.Contains(reply, "Added to ") || strings.Contains(reply, "copy") {
		t.Fatalf("a repeated name = %q", reply)
	}
	if posts := f.postsTo("UALICE"); len(posts) != 2 {
		t.Fatalf("alice's notices = %v", f.replies)
	}
}
