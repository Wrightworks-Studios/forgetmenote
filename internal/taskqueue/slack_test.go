package taskqueue

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

func TestEveryLineOfATaskListShowsTextAsWritten(t *testing.T) {
	source := "https://acme.slack.com/archives/C1/p1700000000000100"
	list := List{Filed: []View{{Task: tasks.Task{ID: "filed", Text: hostileTask, Source: source}, Owner: PersonView{Name: "Dan <Ops>"}, Requester: PersonView{You: true}}}}
	// The first taskListButtons tasks are sections with buttons; the rest
	// are lines of the text.
	for range taskListButtons + 1 {
		list.Mine = append(list.Mine, View{Task: tasks.Task{ID: "mine", Text: hostileTask, Source: source}, Owner: PersonView{You: true}, Requester: PersonView{Name: "Bob & Sons"}})
	}
	reply := (&Service{}).taskListReply(list)
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

	confirmation := taskAddedText([]View{{Task: tasks.Task{ID: "t1", Text: hostileTask}, Owner: PersonView{Name: "Dan <Ops>", SlackUser: "UDAN"}}})
	if !shownAsWritten(confirmation) || !strings.Contains(confirmation, "Added to Dan &lt;Ops&gt;'s queue") {
		t.Fatalf("the confirmation = %q", confirmation)
	}
}
