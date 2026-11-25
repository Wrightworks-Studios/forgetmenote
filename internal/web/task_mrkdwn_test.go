package web

import (
	"strings"
	"testing"

	"forgetmenote/internal/taskqueue"
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
	added := decodeTask[taskqueue.View](t, actionCall(t, bob, "add_task", map[string]any{"person": "<@UALICE>", "text": hostileTask}, false))
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
