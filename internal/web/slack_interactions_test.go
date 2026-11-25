package web

import (
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"testing"

	"forgetmenote/internal/messaging"
)

// taskInteractions are one of each task interaction Slack sends, from user,
// about the task a Done button acts on.
func taskInteractions(f *slackLinkFixture, user string, done map[string]any) map[string]map[string]any {
	hook := f.upstream + "/actions/T123/1/hook"
	value := done["value"]
	press := func(action string, value any) map[string]any {
		return buttonPress(user, "5.1", map[string]any{"action_id": action, "value": value}, hook)
	}
	view := func(callback, metadata string, values map[string]any) map[string]any {
		return map[string]any{"type": "view_submission", "team": map[string]any{"id": "T123"}, "user": map[string]any{"id": user, "team_id": "T123"},
			"view": map[string]any{"id": "V" + callback, "callback_id": callback, "private_metadata": metadata, "state": map[string]any{"values": values}}}
	}
	return map[string]map[string]any{
		"done":           press(taskDoneAction, value),
		"snooze":         press(taskSnoozeAction, value),
		"not applicable": press(taskNotApplicableAction, value),
		"dismiss":        press(taskReplyDismissAction, `{"channel":"D1","ts":"1.2"}`),
		"confirm copies": press(taskConfirmAction, `{"run":"r1","count":30}`),
		"confirm write":  press(messaging.ConfirmAction, `{"code":"ABC123","thread":"1.1"}`),
		"reason":         view(taskNotApplicableView, value.(string), map[string]any{"reason": map[string]any{"reason": map[string]any{"type": "plain_text_input", "value": "Cancelled"}}}),
		"form": view(taskAddView, "", map[string]any{"text": map[string]any{"text": map[string]any{"type": "plain_text_input", "value": "Book the venue"}},
			"people": map[string]any{"people": map[string]any{"type": "multi_users_select", "selected_users": []string{"UBOB"}}}}),
		"confirm form": view(taskConfirmView, `{"user":"`+user+`","count":30,"input":{"people":["everyone"],"text":"Book the venue"}}`, nil),
		"shortcut": {"type": "message_action", "callback_id": taskShortcut, "team": map[string]any{"id": "T123"}, "user": map[string]any{"id": user},
			"channel": map[string]any{"id": "C1"}, "message": map[string]any{"ts": "1.2", "text": "Book the venue"}, "trigger_id": "trigger-1", "response_url": hook},
		"closed": {"type": "view_closed", "team": map[string]any{"id": "T123"}, "user": map[string]any{"id": user}},
	}
}

func TestSlackInteractionsFromAnotherWorkspaceOrAPausedInstanceDoNothing(t *testing.T) {
	f := newTaskFixture(t)
	bob := taskClients(t, f.app, "bob")[0]
	actionCall(t, bob, "add_task", map[string]any{"person": "<@UALICE>", "text": "Book the venue"}, false)
	done := postButtons(f.lastPostTo(t, "UALICE"))[0]
	posts, checks := len(f.posts), f.userInfos
	for name, payload := range taskInteractions(f, "UALICE", done) {
		foreign := maps.Clone(payload)
		foreign["team"] = map[string]any{"id": "TOTHER"}
		if w := f.interact(t, foreign); w.Code != http.StatusForbidden {
			t.Fatalf("%s from another workspace: HTTP %d %s", name, w.Code, w.Body)
		}
	}
	f.app.outgoing.Pause()
	for name, payload := range taskInteractions(f, "UALICE", done) {
		if w := f.interact(t, payload); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s on a paused instance: HTTP %d %s", name, w.Code, w.Body)
		}
	}
	if len(f.posts) != posts || f.userInfos != checks || len(f.responses) != 0 || len(f.views) != 0 {
		t.Fatalf("refused interactions reached Slack: %d posts, %d checks, responses %v, views %v", len(f.posts)-posts, f.userInfos-checks, f.responses, f.views)
	}
	if mine := decodeTask[taskList](t, actionCall(t, taskClients(t, f.app, "alice")[0], "list_tasks", map[string]any{}, false)); len(mine.Mine) != 1 {
		t.Fatalf("a refused interaction changed alice's queue: %+v", mine.Mine)
	}
}

func TestSlackTaskInteractionsAreRefusedToGuests(t *testing.T) {
	f := newTaskFixture(t)
	f.memberDM(t, "UBOB", "8.1", "task <@UALICE> Book the venue")
	done := postButtons(f.lastPostTo(t, "UALICE"))[0]
	f.guests = map[string]bool{"UALICE": true}
	interactions := taskInteractions(f, "UALICE", done)
	for _, name := range []string{"done", "snooze", "dismiss"} {
		before := len(f.responses)
		if w := f.interact(t, interactions[name]); w.Code != 200 || len(f.responses) != before+1 || !strings.HasPrefix(f.responses[before], "Only members of this workspace can") {
			t.Fatalf("%s from a guest: HTTP %d, responses %v", name, w.Code, f.responses[before:])
		}
	}
	for name, field := range map[string]string{"reason": "reason", "form": "people"} {
		out := f.submitView(t, "T123", "UALICE", interactions[name]["view"].(map[string]any))
		errors, _ := out["errors"].(map[string]any)
		if out["response_action"] != "errors" || !strings.HasPrefix(errors[field].(string), "Only members of this workspace can") {
			t.Fatalf("%s from a guest: %v", name, out)
		}
	}
	if out := f.commandReply(t, "/tasks", "UALICE", ""); !strings.Contains(out["text"].(string), "Only members of this workspace have task queues") {
		t.Fatalf("/tasks from a guest: %v", out)
	}
	settings, _, _ := f.app.taskQueues()
	caller, err := f.app.memberTaskCaller(settings, "UALICE", false)
	if err != nil {
		t.Fatal(err)
	}
	if mine, err := f.app.listTasks(t.Context(), caller); err != nil || len(mine.Mine) != 1 {
		t.Fatalf("a guest changed alice's queue: %+v %v", mine.Mine, err)
	}
}

func TestDoneAndNotApplicableAnswerAStaleTaskAlike(t *testing.T) {
	f := newTaskFixture(t)
	clients := taskClients(t, f.app, "alice", "bob")
	alice, bob := clients[0], clients[1]
	added := decodeTask[taskView](t, actionCall(t, bob, "add_task", map[string]any{"person": "<@UALICE>", "text": "Book the venue"}, false))
	done := postButtons(f.lastPostTo(t, "UALICE"))[0]
	actionCall(t, bob, "update_task", map[string]any{"id": added.ID, "revision": added.Revision, "text": "Book the bigger venue"}, false)
	interactions := taskInteractions(f, "UALICE", done)

	pressed := f.press(t, "UALICE", "5.2", done)
	out := f.submitView(t, "T123", "UALICE", interactions["reason"]["view"].(map[string]any))
	var answer struct {
		Action string            `json:"response_action"`
		Errors map[string]string `json:"errors"`
	}
	raw, _ := json.Marshal(out)
	json.Unmarshal(raw, &answer)
	if answer.Action != "errors" || answer.Errors["reason"] != pressed || !strings.HasSuffix(pressed, "Send me `tasks` to see your queue.") {
		t.Fatalf("a stale Done answered %q, a stale Not applicable %v", pressed, out)
	}
	if mine := decodeTask[taskList](t, actionCall(t, alice, "list_tasks", map[string]any{}, false)); len(mine.Mine) != 1 || mine.Mine[0].Text != "Book the bigger venue" {
		t.Fatalf("a stale press changed alice's queue: %+v", mine.Mine)
	}
}
