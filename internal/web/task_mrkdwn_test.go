package web

import (
	"strings"
	"testing"

	"forgetmenote/internal/tasks"
)

// hostileTask is task text in Slack's markup: shown as written, it is a link.
const hostileTask = "Pay <https://evil.example|the invoice> & file it"

// shownAsWritten reports whether Slack text shows hostileTask as typed.
func shownAsWritten(text string) bool {
	return strings.Contains(text, "Pay &lt;https://evil.example|the invoice&gt; &amp; file it") && !strings.Contains(text, "<https://evil.example")
}

func TestTaskTextReasonsAndNamesShowAsWrittenInSlack(t *testing.T) {
	f := newTaskFixture(t)
	f.members = []map[string]any{
		{"id": "UALICE", "team_id": "T123", "profile": map[string]any{"real_name": "Alice <Ops>"}},
		{"id": "UBOB", "team_id": "T123", "profile": map[string]any{"real_name": "Bob & Sons"}},
	}
	clients := taskClients(t, f.app, "alice", "bob")
	alice, bob := clients[0], clients[1]
	added := decodeTask[taskView](t, actionCall(t, bob, "add_task", map[string]any{"person": "<@UALICE>", "text": hostileTask}, false))
	notice := f.lastPostTo(t, "UALICE")["text"].(string)
	if !shownAsWritten(notice) || !strings.Contains(notice, "New task from Bob &amp; Sons") {
		t.Fatalf("alice's notice = %q", notice)
	}
	if list := f.memberDM(t, "UBOB", "9.1", "tasks"); !shownAsWritten(list) || !strings.Contains(list, "_(Alice &lt;Ops&gt;)_") {
		t.Fatalf("bob's list = %q", list)
	}
	actionCall(t, alice, "update_task", map[string]any{"id": added.ID, "revision": added.Revision, "outcome": "not_applicable", "reason": "Paid at <https://pay.example|the portal>"}, false)
	closed := f.lastPostTo(t, "UBOB")["text"].(string)
	if !shownAsWritten(closed) || !strings.Contains(closed, "Alice &lt;Ops&gt; marked your task not applicable") || !strings.Contains(closed, "Reason: Paid at &lt;https://pay.example|the portal&gt;") {
		t.Fatalf("bob's closure notice = %q", closed)
	}
}

func TestEveryLineOfATaskListShowsTextAsWritten(t *testing.T) {
	source := "https://acme.slack.com/archives/C1/p1700000000000100"
	list := taskList{Filed: []taskView{{Task: tasks.Task{ID: "filed", Text: hostileTask, Source: source}, Owner: taskPersonView{Name: "Dan <Ops>"}, Requester: taskPersonView{You: true}}}}
	// The first taskListButtons tasks are sections with buttons; the rest
	// are lines of the text.
	for range taskListButtons + 1 {
		list.Mine = append(list.Mine, taskView{Task: tasks.Task{ID: "mine", Text: hostileTask, Source: source}, Owner: taskPersonView{You: true}, Requester: taskPersonView{Name: "Bob & Sons"}})
	}
	reply := (&Server{}).taskListReply(list)
	lines := strings.Split(reply.Text, "\n")
	for _, section := range reply.Sections {
		lines = append(lines, section.Text)
	}
	tasksShown := 0
	for _, line := range lines {
		if !strings.Contains(line, "Pay ") {
			continue
		}
		tasksShown++
		if !shownAsWritten(line) || !strings.Contains(line, "|Slack conversation>") {
			t.Fatalf("a task line = %q", line)
		}
		if !strings.Contains(line, "from Bob &amp; Sons") && !strings.Contains(line, "_(Dan &lt;Ops&gt;)_") {
			t.Fatalf("a line names someone unescaped: %q", line)
		}
	}
	if tasksShown != taskListButtons+2 {
		t.Fatalf("the list shows %d tasks:\n%s", tasksShown, strings.Join(lines, "\n"))
	}

	confirmation := taskAddedText([]taskView{{Task: tasks.Task{ID: "t1", Text: hostileTask}, Owner: taskPersonView{Name: "Dan <Ops>", SlackUser: "UDAN"}}})
	if !shownAsWritten(confirmation) || !strings.Contains(confirmation, "Added to Dan &lt;Ops&gt;'s queue") {
		t.Fatalf("the confirmation = %q", confirmation)
	}
}
