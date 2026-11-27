package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/taskqueue"
)

// command sends the /task slash command as Slack does: a form signed with the
// signing secret, or with a wrong one when signed is false.
func (f *slackLinkFixture) command(t *testing.T, team, user, text string, signed bool) *httptest.ResponseRecorder {
	t.Helper()
	return f.commandNamed(t, "/task", team, user, text, signed)
}

func (f *slackLinkFixture) commandNamed(t *testing.T, name, team, user, text string, signed bool) *httptest.ResponseRecorder {
	t.Helper()
	body := url.Values{"team_id": {team}, "user_id": {user}, "command": {name}, "text": {text}, "trigger_id": {"trigger-cmd"}, "channel_id": {"C1"}, "response_url": {f.upstream + "/commands/T123/1/hook"}}.Encode()
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	secret := "fixture-signing"
	if !signed {
		secret = "someone-else"
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + stamp + ":" + body))
	r := httptest.NewRequest("POST", "http://localhost/integrations/slack/commands", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Slack-Request-Timestamp", stamp)
	r.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	f.app.ServeHTTP(w, r)
	return w
}

// taskForm is what a member chose in the /task form.
type taskForm struct {
	people, channels     []string
	everyone             bool
	text, due, pri, link string
}

// submitTaskForm submits the /task form from view as Slack does and returns
// the answer.
func (f *slackLinkFixture) submitTaskForm(t *testing.T, team, user, view string, form taskForm) map[string]any {
	t.Helper()
	values := map[string]any{
		"people":   map[string]any{"people": map[string]any{"type": "multi_users_select", "selected_users": form.people}},
		"text":     map[string]any{"text": map[string]any{"type": "plain_text_input", "value": form.text}},
		"due":      map[string]any{"due": map[string]any{"type": "datepicker", "selected_date": nil}},
		"priority": map[string]any{"priority": map[string]any{"type": "static_select", "selected_option": nil}},
	}
	if form.due != "" {
		values["due"] = map[string]any{"due": map[string]any{"type": "datepicker", "selected_date": form.due}}
	}
	if form.pri != "" {
		values["priority"] = map[string]any{"priority": map[string]any{"type": "static_select", "selected_option": map[string]any{"value": form.pri}}}
	}
	values["link"] = map[string]any{"link": map[string]any{"type": "url_text_input", "value": form.link}}
	values["channels"] = map[string]any{"channels": map[string]any{"type": "multi_conversations_select", "selected_conversations": form.channels}}
	values["everyone"] = map[string]any{"everyone": map[string]any{"type": "checkboxes", "selected_options": []any{}}}
	if form.everyone {
		values["everyone"] = map[string]any{"everyone": map[string]any{"type": "checkboxes", "selected_options": []any{map[string]any{"value": "everyone"}}}}
	}
	return f.submitView(t, team, user, map[string]any{"id": view, "callback_id": taskqueue.AddView, "private_metadata": "", "state": map[string]any{"values": values}})
}

// submitView submits a modal view as Slack does and returns the answer.
func (f *slackLinkFixture) submitView(t *testing.T, team, user string, view map[string]any) map[string]any {
	t.Helper()
	w := f.interact(t, map[string]any{"type": "view_submission", "team": map[string]any{"id": team}, "user": map[string]any{"id": user, "team_id": team}, "view": view})
	if w.Code != 200 {
		return map[string]any{"status": w.Code}
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("submission: HTTP %d %q", w.Code, w.Body)
	}
	return out
}

// formElement returns the element of the form block with id.
func formElement(t *testing.T, view map[string]any, id string) (block, element map[string]any) {
	t.Helper()
	blocks, _ := view["blocks"].([]any)
	for _, b := range blocks {
		block, _ := b.(map[string]any)
		if block["block_id"] == id {
			element, _ := block["element"].(map[string]any)
			return block, element
		}
	}
	t.Fatalf("the form has no %s block: %v", id, view["blocks"])
	return nil, nil
}

func TestSlashTaskOpensAFormThatGivesEachPersonACopy(t *testing.T) {
	f := newTaskFixture(t)
	if w := f.command(t, "T123", "UBOB", "  Review the Q4 deck ", true); w.Code != 200 || len(f.views) != 1 {
		t.Fatalf("/task: HTTP %d %q, views %v", w.Code, w.Body, f.views)
	}
	if f.views[0]["trigger_id"] != "trigger-cmd" {
		t.Fatalf("the form opened with %v", f.views[0])
	}
	view := f.views[0]["view"].(map[string]any)
	if view["type"] != "modal" || view["callback_id"] != taskqueue.AddView {
		t.Fatalf("the form = %v", view)
	}
	_, people := formElement(t, view, "people")
	if people["type"] != "multi_users_select" || fmt.Sprint(people["initial_users"]) != "[UBOB]" || people["max_selected_items"] != float64(20) {
		t.Fatalf("people = %v", people)
	}
	_, text := formElement(t, view, "text")
	if text["type"] != "plain_text_input" || text["max_length"] != float64(500) || text["initial_value"] != "Review the Q4 deck" {
		t.Fatalf("text = %v", text)
	}
	block, due := formElement(t, view, "due")
	if due["type"] != "datepicker" || block["optional"] != true {
		t.Fatalf("due = %v", block)
	}
	block, priority := formElement(t, view, "priority")
	if priority["type"] != "static_select" || block["optional"] != true || !strings.Contains(fmt.Sprint(priority["options"]), "high") || !strings.Contains(fmt.Sprint(priority["options"]), "low") {
		t.Fatalf("priority = %v", block)
	}

	submission := taskForm{people: []string{"UALICE", "UDAN"}, text: "Review the Q4 deck", due: "2026-12-05"}
	if out := f.submitTaskForm(t, "T123", "UBOB", "VFORM1", submission); out["response_action"] != "clear" {
		t.Fatalf("the submission: %v", out)
	}
	for _, member := range []string{"UALICE", "UDAN"} {
		if posts := f.postsTo(member); len(posts) != 1 || !strings.Contains(posts[0]["text"].(string), "Review the Q4 deck") || len(postButtons(posts[0])) != 1 {
			t.Fatalf("%s's notices = %v", member, f.replies)
		}
	}
	confirmed := f.postsTo("UBOB")
	if len(confirmed) != 1 || !strings.Contains(confirmed[0]["text"].(string), "Added a copy for") || !strings.Contains(confirmed[0]["text"].(string), "Due 2026-12-05") {
		t.Fatalf("bob's confirmation = %v", f.replies)
	}

	// Slack may send the same submission again; it adds nothing more.
	if out := f.submitTaskForm(t, "T123", "UBOB", "VFORM1", submission); out["response_action"] != "clear" {
		t.Fatalf("the repeated submission: %v", out)
	}
	if len(f.postsTo("UALICE")) != 1 || len(f.postsTo("UDAN")) != 1 || len(f.postsTo("UBOB")) != 1 {
		t.Fatalf("a repeated submission sent %v", f.replies)
	}
	if reply := f.memberDM(t, "UBOB", "5.1", "tasks"); strings.Count(reply, "Review the Q4 deck") != 1 || !strings.Contains(reply, "(Contributor and Slack member UDAN)") {
		t.Fatalf("bob follows %q", reply)
	}

	// Priority is for a task the requester gives only themself.
	if out := f.submitTaskForm(t, "T123", "UBOB", "VFORM2", taskForm{people: []string{"UBOB"}, text: "Draft holiday copy", pri: "high"}); out["response_action"] != "clear" {
		t.Fatalf("own task with priority: %v", out)
	}
	f.memberDM(t, "UBOB", "5.2", "tasks")
	if list := fmt.Sprint(f.posts[len(f.posts)-1]["blocks"]); !strings.Contains(list, "Draft holiday copy") || !strings.Contains(list, "high priority") {
		t.Fatalf("bob's queue = %s", list)
	}
}

func TestSlashTaskFormRefusesBadInputAndAddsNothing(t *testing.T) {
	f := newTaskFixture(t)
	for _, c := range []struct {
		form  taskForm
		field string
	}{
		{taskForm{people: []string{"UALICE"}, text: "Review the deck", pri: "high"}, "priority"},
		{taskForm{people: []string{"UBOB", "UALICE"}, text: "Review the deck", pri: "low"}, "priority"},
		{taskForm{people: []string{"UALICE"}, text: "   "}, "text"},
		{taskForm{people: []string{"UALICE"}, text: strings.Repeat("x", 501)}, "text"},
		{taskForm{people: []string{}, text: "Review the deck"}, "people"},
		{taskForm{people: []string{"UALICE"}, text: "Review the deck", due: "next week"}, "due"},
	} {
		out := f.submitTaskForm(t, "T123", "UBOB", "VBAD", c.form)
		errs, _ := out["errors"].(map[string]any)
		if out["response_action"] != "errors" || errs[c.field] == nil || errs[c.field] == "" {
			t.Errorf("%+v: %v", c.form, out)
		}
	}
	if len(f.posts) != 0 {
		t.Fatalf("bad input sent %v", f.replies)
	}
	if reply := f.memberDM(t, "UBOB", "6.1", "tasks"); !strings.Contains(reply, "no open tasks") {
		t.Fatalf("bad input added %q", reply)
	}
}

func TestSlashTaskRefusesUnsignedAndForeignRequests(t *testing.T) {
	f := newTaskFixture(t)
	if w := f.command(t, "T123", "UBOB", "", false); w.Code != 401 {
		t.Fatalf("an unsigned command: HTTP %d %q", w.Code, w.Body)
	}
	if w := f.command(t, "T999", "UBOB", "", true); w.Code != 403 {
		t.Fatalf("another workspace's command: HTTP %d %q", w.Code, w.Body)
	}
	if len(f.views) != 0 {
		t.Fatalf("a refused command opened %v", f.views)
	}
	if out := f.submitTaskForm(t, "T999", "UBOB", "VFOREIGN", taskForm{people: []string{"UALICE"}, text: "Review the deck"}); out["status"] != 403 {
		t.Fatalf("another workspace's submission: %v", out)
	}
	if len(f.posts) != 0 {
		t.Fatalf("a refused submission sent %v", f.replies)
	}
}

func TestTaskHelpIsAnsweredWithoutTheModel(t *testing.T) {
	f := newTaskFixture(t)
	for i, text := range []string{"task help", "Tasks help", "task", "task <@UALICE>", "task @alice send the deck"} {
		reply := f.memberDM(t, "UBOB", "7."+strconv.Itoa(i), text)
		if !strings.Contains(reply, "task @person") || !strings.Contains(reply, "/task") || !strings.Contains(reply, "tasks`") {
			t.Errorf("%q: %q", text, reply)
		}
	}
	f.requests.mu.Lock()
	asked := len(f.requests.prompts)
	f.requests.mu.Unlock()
	if asked != 0 {
		t.Fatalf("the model was asked %d times", asked)
	}
	for _, text := range []string{"tasks for next week are piling up", "task alice send the deck", "task force notes from Monday", "can you task <@UALICE> with it?"} {
		if taskqueue.Command(text) {
			t.Errorf("%q was taken as a task command", text)
		}
	}
	// /task help answers only the member who asked, without a form.
	w := f.command(t, "T123", "UBOB", "help", true)
	var out map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out["response_type"] != "ephemeral" || !strings.Contains(fmt.Sprint(out["text"]), "task @person") || len(f.views) != 0 {
		t.Fatalf("/task help: HTTP %d %q, views %v", w.Code, w.Body, f.views)
	}
}

// commandReply sends a signed slash command from a member and returns the
// message Slack shows them.
func (f *slackLinkFixture) commandReply(t *testing.T, name, user, text string) map[string]any {
	t.Helper()
	w := f.commandNamed(t, name, "T123", user, text, true)
	var out map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("%s %s: HTTP %d %q", name, text, w.Code, w.Body)
	}
	return out
}

func TestTasksCommandShowsTheQueueOnlyToWhoeverAsks(t *testing.T) {
	f := newTaskFixture(t)
	if out := f.commandReply(t, "/tasks", "UALICE", ""); out["response_type"] != "ephemeral" || !strings.Contains(fmt.Sprint(out["text"]), "no open tasks") {
		t.Fatalf("an empty queue: %v", out)
	}
	f.memberDM(t, "UBOB", "8.1", "task <@UALICE> Send the Denver lease to legal")

	mine := f.commandReply(t, "/tasks", "UALICE", "")
	if buttons := postButtons(mine); mine["response_type"] != "ephemeral" || !strings.Contains(fmt.Sprint(mine["blocks"]), "Send the Denver lease") || len(buttons) != 1 || buttons[0]["action_id"] != taskqueue.DoneAction {
		t.Fatalf("alice's list: %v", mine)
	}
	if len(f.views) != 0 {
		t.Fatalf("the list opened a form: %v", f.views)
	}
	// The requester follows the task, without a button to close it.
	if out := f.commandReply(t, "/tasks", "UBOB", ""); !strings.Contains(fmt.Sprint(out["text"]), "Tasks you gave others") || len(postButtons(out)) != 0 {
		t.Fatalf("bob's list: %v", out)
	}
	// Done in the private list closes the task.
	if text := f.press(t, "UALICE", "9.1", postButtons(mine)[0]); !strings.HasPrefix(text, "Done.") {
		t.Fatalf("alice's press: %q", text)
	}
	if out := f.commandReply(t, "/tasks", "UALICE", ""); !strings.Contains(fmt.Sprint(out["text"]), "no open tasks") {
		t.Fatalf("after Done: %v", out)
	}
}
