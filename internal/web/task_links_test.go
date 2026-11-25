package web

import (
	"fmt"
	"strings"
	"testing"

	"forgetmenote/internal/messaging"
	"forgetmenote/internal/taskqueue"
)

// channelMessage sends a message from a member in a channel thread and
// returns the reply posted there.
func (f *slackLinkFixture) channelMessage(t *testing.T, user, channel, thread, id, text string) string {
	t.Helper()
	before := len(f.replies)
	d := messaging.Address{Client: "slack", Workspace: "T123", Channel: channel, Thread: thread, Message: id, User: user}
	if err := f.app.enqueueMessage(t.Context(), messaging.Event{ID: id, Text: text, Address: d}); err != nil {
		t.Fatal(err)
	}
	if ran, err := f.app.automations.Queue.RunNext(t.Context(), f.app.runAutomation); err != nil || !ran {
		t.Fatalf("the message %q did not run: %v %v", text, ran, err)
	}
	for _, reply := range f.replies[before:] {
		if strings.HasPrefix(reply, channel+": ") {
			return reply
		}
	}
	t.Fatalf("no reply in %s: %v", channel, f.replies[before:])
	return ""
}

func TestTaskMessagesNameSlackMembers(t *testing.T) {
	f := newTaskFixture(t)
	f.members = []map[string]any{slackMember("UALICE"), slackMember("UBOB"), slackMember("UDAN")}

	if reply := f.memberDM(t, "UBOB", "8.1", "task <@UDAN> Review the deck"); !strings.Contains(reply, "Added to Dan Ortiz's queue") {
		t.Fatalf("bob's confirmation = %q", reply)
	}
	notice := f.lastPostTo(t, "UDAN")
	if text := notice["text"].(string); !strings.Contains(text, "New task from Bob Ray") {
		t.Fatalf("dan's notice = %q", text)
	}
	if reply := f.memberDM(t, "UBOB", "8.2", "tasks"); !strings.Contains(reply, "Review the deck _(Dan Ortiz)_") {
		t.Fatalf("bob's list = %q", reply)
	}
	if text := f.press(t, "UDAN", "8.3", postButtons(notice)[0]); !strings.HasPrefix(text, "Done.") {
		t.Fatalf("dan's press = %q", text)
	}
	if text := f.lastPostTo(t, "UBOB")["text"].(string); !strings.Contains(text, "Dan Ortiz marked your task done") {
		t.Fatalf("bob's closure notice = %q", text)
	}
	for _, reply := range f.replies {
		if strings.Contains(reply, "Slack member") || strings.Contains(reply, "Contributor") {
			t.Fatalf("a message named someone by ID or a placeholder: %q", reply)
		}
	}
}

func TestTasksLinkTheSlackConversationTheyCameFrom(t *testing.T) {
	f := newTaskFixture(t)
	f.members = []map[string]any{slackMember("UALICE"), slackMember("UBOB"), slackMember("UDAN")}
	thread := "<https://acme.slack.com/archives/C1/p1700000000000100|Slack conversation>"

	// A task typed in a channel thread links that thread.
	reply := f.channelMessage(t, "UBOB", "C1", "1700000000.000100", "1700000000.000200", "<@UAPP> task <@UDAN> Fix the typo in the brief")
	if !strings.Contains(reply, "Added to Dan Ortiz's queue") || !strings.Contains(reply, thread) {
		t.Fatalf("the thread request = %q", reply)
	}
	if text := f.lastPostTo(t, "UDAN")["text"].(string); !strings.Contains(text, thread) || !strings.Contains(text, "Fix the typo in the brief") {
		t.Fatalf("dan's notice = %q", text)
	}

	// A pasted message link becomes the link, not part of the task.
	pasted := "<https://acme.slack.com/archives/C7/p1700000000000300|Slack conversation>"
	if reply = f.memberDM(t, "UBOB", "9.1", "task <@UDAN> Answer <https://acme.slack.com/archives/C7/p1700000000000300> due 2026-12-01"); !strings.Contains(reply, "> Answer\n") || !strings.Contains(reply, pasted) {
		t.Fatalf("the pasted link = %q", reply)
	}
	if text := f.lastPostTo(t, "UDAN")["text"].(string); !strings.Contains(text, pasted) || strings.Contains(text, "Answer <") {
		t.Fatalf("dan's notice = %q", text)
	}

	// A task from a direct message without a link has none.
	f.memberDM(t, "UBOB", "1700000000.000600", "task <@UDAN> Book the room")
	if text := f.lastPostTo(t, "UDAN")["text"].(string); strings.Contains(text, "Slack conversation") {
		t.Fatalf("a DM task links something: %q", text)
	}

	// The owner's list links each task's conversation.
	if reply = f.memberDM(t, "UDAN", "9.3", "tasks"); !strings.Contains(fmt.Sprint(f.lastPostTo(t, "DUDAN")["blocks"]), thread) {
		t.Fatalf("dan's list = %q %v", reply, f.lastPostTo(t, "DUDAN")["blocks"])
	}
	// A channel the app cannot read still adds the task, without a link.
	if reply = f.channelMessage(t, "UBOB", "CHIDDEN", "1700000000.000500", "1700000000.000500", "<@UAPP> task <@UDAN> Water the plants"); !strings.Contains(reply, "Added to") || strings.Contains(reply, "Slack conversation") {
		t.Fatalf("a hidden channel = %q", reply)
	}
}

func TestSlashTaskFormAndMessageShortcutLinkAConversation(t *testing.T) {
	f := newTaskFixture(t)
	f.members = []map[string]any{slackMember("UALICE"), slackMember("UBOB"), slackMember("UDAN")}
	link := "https://acme.slack.com/archives/C7/p1700000000000300"

	if w := f.command(t, "T123", "UBOB", "", true); w.Code != 200 || len(f.views) != 1 {
		t.Fatalf("/task: HTTP %d %q", w.Code, w.Body)
	}
	block, element := formElement(t, f.views[0]["view"].(map[string]any), "link")
	if element["type"] != "url_text_input" || block["optional"] != true {
		t.Fatalf("link = %v", block)
	}

	// The form takes a Slack message link and refuses anything else.
	form := taskForm{people: []string{"UDAN"}, text: "Answer the client", link: "https://example.com/somewhere"}
	if out := f.submitTaskForm(t, "T123", "UBOB", "VLINK0", form); out["response_action"] != "errors" || out["errors"].(map[string]any)["link"] == nil {
		t.Fatalf("a link elsewhere: %v", out)
	}
	form.link = link
	if out := f.submitTaskForm(t, "T123", "UBOB", "VLINK1", form); out["response_action"] != "clear" {
		t.Fatalf("the linked form: %v", out)
	}
	if text := f.lastPostTo(t, "UDAN")["text"].(string); !strings.Contains(text, "<"+link+"|Slack conversation>") {
		t.Fatalf("dan's notice = %q", text)
	}

	// The message shortcut opens the form with the message linked and its
	// text, made readable, as the task.
	shortcut := func(channel string) map[string]any {
		return map[string]any{"type": "message_action", "callback_id": taskqueue.Shortcut, "trigger_id": "trigger-shortcut", "team": map[string]any{"id": "T123"},
			"user": map[string]any{"id": "UBOB", "team_id": "T123"}, "channel": map[string]any{"id": channel}, "response_url": f.upstream + "/actions/T123/2/hook",
			"message": map[string]any{"ts": "1700000000.000400", "text": "Can <@UDAN> update <https://example.com/doc|the doc> &amp; tell <#C1|design>?\nThanks"}}
	}
	if w := f.interact(t, shortcut("C1")); w.Code != 200 || len(f.views) != 2 {
		t.Fatalf("the shortcut: HTTP %d %q, views %d", w.Code, w.Body, len(f.views))
	}
	view := f.views[1]["view"].(map[string]any)
	if f.views[1]["trigger_id"] != "trigger-shortcut" || view["callback_id"] != taskqueue.AddView {
		t.Fatalf("the shortcut opened %v", f.views[1])
	}
	if _, text := formElement(t, view, "text"); text["initial_value"] != "Can @Dan Ortiz update the doc & tell #design? Thanks" {
		t.Fatalf("the task text = %q", text["initial_value"])
	}
	if _, linked := formElement(t, view, "link"); linked["initial_value"] != "https://acme.slack.com/archives/C1/p1700000000000400" {
		t.Fatalf("the link = %v", linked)
	}

	// A message the app cannot see still opens the form, unlinked.
	if w := f.interact(t, shortcut("CHIDDEN")); w.Code != 200 || len(f.views) != 3 {
		t.Fatalf("a hidden message: HTTP %d, views %d", w.Code, len(f.views))
	}
	if _, linked := formElement(t, f.views[2]["view"].(map[string]any), "link"); linked["initial_value"] != nil {
		t.Fatalf("a hidden message was linked: %v", linked)
	}
	// Another workspace's shortcut opens nothing.
	other := shortcut("C1")
	other["team"] = map[string]any{"id": "TOTHER"}
	if w := f.interact(t, other); w.Code != 403 || len(f.views) != 3 {
		t.Fatalf("another workspace: HTTP %d, views %d", w.Code, len(f.views))
	}
}
