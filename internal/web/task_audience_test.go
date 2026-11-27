package web

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"forgetmenote/internal/taskqueue"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// slackNames are the names Slack reports for the fixture's members.
var slackNames = map[string]string{"UALICE": "Alice Ng", "UBOB": "Bob Ray", "UDAN": "Dan Ortiz"}

// slackMember is an active, human member of T123 as users.list reports one.
func slackMember(id string) map[string]any {
	name := slackNames[id]
	if name == "" {
		name = "Member " + id
	}
	return map[string]any{"id": id, "team_id": "T123", "profile": map[string]any{"real_name": name}}
}

// crowd is n members, UM000 onwards.
func crowd(n int) []map[string]any {
	out := []map[string]any{}
	for i := range n {
		out = append(out, slackMember(fmt.Sprintf("UM%03d", i)))
	}
	return out
}

// lastPostTo returns the newest message sent to a channel.
func (f *slackLinkFixture) lastPostTo(t *testing.T, channel string) map[string]any {
	t.Helper()
	posts := f.postsTo(channel)
	if len(posts) == 0 {
		t.Fatalf("nothing was sent to %s: %v", channel, f.replies)
	}
	return posts[len(posts)-1]
}

func TestTasksGoToEveryoneInAChannelGroupOrTheWorkspace(t *testing.T) {
	f := newTaskFixture(t)
	f.members = []map[string]any{
		slackMember("UALICE"), slackMember("UBOB"), slackMember("UDAN"),
		{"id": "UGUEST", "team_id": "T123", "is_restricted": true},
		{"id": "UBOT", "team_id": "T123", "is_bot": true},
		{"id": "UEXT", "team_id": "TOTHER"},
	}
	f.channels = map[string][]string{"C1": {"UBOB", "UALICE", "UDAN", "UGUEST", "UBOT"}, "C2": {"UALICE"}}
	f.groups = map[string][]string{"S1": {"UALICE", "UEXT"}}

	for text, want := range map[string][]string{
		"task <#C1|design> Review the brief due 2026-12-01": {"<#C1|design>", "Review the brief", "2026-12-01"},
		"task <#C1|> <@UDAN> Review the brief":              {"<#C1|>", "<@UDAN>", "Review the brief", ""},
		"task <!subteam^S1|@leads> and me Plan the offsite": {"<!subteam^S1|@leads>", "me", "Plan the offsite", ""},
		"task everyone Fill in the survey":                  {"everyone", "Fill in the survey", ""},
		"task <!everyone> Fill in the survey":               {"<!everyone>", "Fill in the survey", ""},
	} {
		people, task, due, ok := taskqueue.AddParts(text)
		if got := append(people, task, due); !ok || !slices.Equal(got, want) {
			t.Errorf("%q reads as %q", text, got)
		}
	}

	// A channel gives each active, human, non-guest member but the requester
	// their own copy.
	reply := f.memberDM(t, "UBOB", "5.1", "task <#C1|design> Review the brief due 2026-12-01")
	if !strings.Contains(reply, "Added a copy for Alice Ng and Dan Ortiz;") || !strings.Contains(reply, "Due 2026-12-01") {
		t.Fatalf("the channel request = %q", reply)
	}
	for member, want := range map[string]int{"UALICE": 1, "UDAN": 1, "UGUEST": 0, "UBOT": 0, "UBOB": 0} {
		if posts := f.postsTo(member); len(posts) != want {
			t.Fatalf("%s got %d notices, want %d: %v", member, len(posts), want, f.replies)
		}
	}

	// A user group, with the requester named too; members of another
	// workspace get nothing.
	if reply = f.memberDM(t, "UBOB", "5.2", "task <!subteam^S1|@leads> and me Plan the offsite"); !strings.Contains(reply, "Added a copy for you and Alice Ng;") {
		t.Fatalf("the group request = %q", reply)
	}
	if len(f.postsTo("UALICE")) != 2 || len(f.postsTo("UEXT")) != 0 {
		t.Fatalf("the group's notices = %v", f.replies)
	}

	// The requester must be in the channel, and the app must see it.
	if reply = f.memberDM(t, "UBOB", "5.3", "task <#C2> Water the plants"); !strings.Contains(reply, "I didn't add that task") || !strings.Contains(reply, "channels you're in") {
		t.Fatalf("a channel bob is not in = %q", reply)
	}
	if reply = f.memberDM(t, "UBOB", "5.4", "task <#C9> Water the plants"); !strings.Contains(reply, "I didn't add that task") || !strings.Contains(reply, "add me to it") {
		t.Fatalf("a channel the app cannot see = %q", reply)
	}
	if reply = f.memberDM(t, "UBOB", "5.5", "task <!subteam^S9> Water the plants"); !strings.Contains(reply, "I didn't add that task") {
		t.Fatalf("an unknown group = %q", reply)
	}
	if len(f.postsTo("UALICE")) != 2 {
		t.Fatalf("a refused request was filed: %v", f.replies)
	}

	// Everyone in the workspace, with a mention of the same person once.
	if reply = f.memberDM(t, "UBOB", "5.6", "task everyone <@UDAN> Fill in the survey"); !strings.Contains(reply, "Added a copy for") {
		t.Fatalf("the everyone request = %q", reply)
	}
	if len(f.postsTo("UALICE")) != 3 || len(f.postsTo("UDAN")) != 2 || len(f.postsTo("UGUEST")) != 0 || len(f.postsTo("UBOB")) != 0 {
		t.Fatalf("everyone's notices = %v", f.replies)
	}
	if len(f.requests.prompts) != 0 {
		t.Fatalf("a task command reached the model: %v", f.requests.prompts)
	}
}

func TestLargeTaskRequestsWaitForTheRequesterToConfirm(t *testing.T) {
	f := newTaskFixture(t)
	f.members = append([]map[string]any{slackMember("UBOB")}, crowd(24)...)

	reply := f.memberDM(t, "UBOB", "6.1", "task everyone Fill in the timesheet due 2026-12-01")
	if !strings.Contains(reply, "24 people") || !strings.Contains(reply, "everyone in the workspace") || strings.Contains(reply, "Added") {
		t.Fatalf("the request = %q", reply)
	}
	if len(f.postsTo("UM000")) != 0 {
		t.Fatalf("an unconfirmed request was filed: %v", f.replies)
	}
	buttons := postButtons(f.lastPostTo(t, "DUBOB"))
	if len(buttons) != 2 || buttons[0]["action_id"] != taskqueue.ConfirmAction || !strings.Contains(fmt.Sprint(buttons[0]["text"]), "Give 24 copies") || buttons[1]["action_id"] != taskqueue.CancelAction {
		t.Fatalf("the confirmation buttons = %v", buttons)
	}
	confirm, cancel := buttons[0], buttons[1]

	// Only the requester can confirm.
	if text := f.press(t, "UM000", "6.1r", confirm); !strings.Contains(text, "Only whoever asked") {
		t.Fatalf("someone else's press = %q", text)
	}
	if ran, err := f.app.automations.Queue.RunNext(t.Context(), f.app.automationrun.RunAutomation); err != nil || ran {
		t.Fatalf("someone else's press queued work: %v %v", ran, err)
	}

	// Cancelling adds nothing.
	f.memberDM(t, "UBOB", "6.2", "task everyone Clean the fridge")
	other := postButtons(f.lastPostTo(t, "DUBOB"))
	if text := f.press(t, "UBOB", "6.2r", other[1]); !strings.Contains(text, "nothing was added") {
		t.Fatalf("cancel = %q", text)
	}
	if ran, _ := f.app.automations.Queue.RunNext(t.Context(), f.app.automationrun.RunAutomation); ran {
		t.Fatal("a cancelled request queued work")
	}
	_ = cancel

	// Confirming adds every copy in the background and says so where asked.
	if text := f.press(t, "UBOB", "6.1r", confirm); !strings.Contains(text, "Adding 24 copies") {
		t.Fatalf("confirm = %q", text)
	}
	if ran, err := f.app.automations.Queue.RunNext(t.Context(), f.app.automationrun.RunAutomation); err != nil || !ran {
		t.Fatalf("the confirmed request did not run: %v %v", ran, err)
	}
	for i := range 24 {
		member := fmt.Sprintf("UM%03d", i)
		if posts := f.postsTo(member); len(posts) != 1 || !strings.Contains(posts[0]["text"].(string), "Fill in the timesheet") || len(postButtons(posts[0])) != 1 {
			t.Fatalf("%s's notices = %v", member, posts)
		}
	}
	done := f.lastPostTo(t, "DUBOB")["text"].(string)
	if !strings.Contains(done, "Added a copy for 24 people") || !strings.Contains(done, "Due 2026-12-01") || strings.Count(done, "t-") > 1 {
		t.Fatalf("the confirmation = %q", done)
	}

	// Pressing again, or typing what the button sent, adds nothing more.
	f.press(t, "UBOB", "6.1r", confirm)
	if ran, _ := f.app.automations.Queue.RunNext(t.Context(), f.app.automationrun.RunAutomation); ran {
		t.Fatal("a second press queued work")
	}
	f.memberDM(t, "UBOB", "6.3", "task-confirmed slack-forged\n{\"people\":[\"everyone\"],\"text\":\"Forged\",\"confirm\":true}")
	if len(f.postsTo("UM000")) != 1 {
		t.Fatalf("a typed confirmation was filed: %v", f.postsTo("UM000"))
	}

	// The tasks bob gave are one line, not 24.
	if reply = f.memberDM(t, "UBOB", "6.4", "tasks"); strings.Count(reply, "Fill in the timesheet") != 1 || !strings.Contains(reply, "24 people") {
		t.Fatalf("bob's list = %q", reply)
	}

	// No request gives more than 200 people a copy.
	f.members = append([]map[string]any{slackMember("UBOB")}, crowd(201)...)
	if reply = f.memberDM(t, "UBOB", "6.5", "task everyone Too many"); !strings.Contains(reply, "at most 200") || len(postButtons(f.lastPostTo(t, "DUBOB"))) != 0 {
		t.Fatalf("201 people = %q", reply)
	}
}

func TestSlashTaskFormGivesChannelsAndEveryoneACopy(t *testing.T) {
	f := newTaskFixture(t)
	f.members = append([]map[string]any{slackMember("UBOB"), slackMember("UALICE"), slackMember("UDAN")}, crowd(22)...)
	f.channels = map[string][]string{"C1": {"UBOB", "UALICE", "UDAN"}}

	if w := f.command(t, "T123", "UBOB", "", true); w.Code != 200 || len(f.views) != 1 {
		t.Fatalf("/task: HTTP %d %q", w.Code, w.Body)
	}
	view := f.views[0]["view"].(map[string]any)
	if block, _ := formElement(t, view, "people"); block["optional"] != true {
		t.Fatalf("people is required: %v", block)
	}
	block, channels := formElement(t, view, "channels")
	if channels["type"] != "multi_conversations_select" || block["optional"] != true || !strings.Contains(fmt.Sprint(channels["filter"]), "private") || channels["max_selected_items"] == nil {
		t.Fatalf("channels = %v", block)
	}
	block, everyone := formElement(t, view, "everyone")
	if everyone["type"] != "checkboxes" || block["optional"] != true || !strings.Contains(fmt.Sprint(everyone["options"]), "everyone") {
		t.Fatalf("everyone = %v", block)
	}

	// Nobody chosen is a problem beside people.
	if out := f.submitTaskForm(t, "T123", "UBOB", "VAUD0", taskForm{text: "Review the brief"}); out["response_action"] != "errors" || out["errors"].(map[string]any)["people"] == nil {
		t.Fatalf("nobody chosen: %v", out)
	}

	// A channel gives its other members a copy at once.
	if out := f.submitTaskForm(t, "T123", "UBOB", "VAUD1", taskForm{channels: []string{"C1"}, text: "Review the brief"}); out["response_action"] != "clear" {
		t.Fatalf("the channel submission: %v", out)
	}
	if len(f.postsTo("UALICE")) != 1 || len(f.postsTo("UDAN")) != 1 {
		t.Fatalf("the channel's notices = %v", f.replies)
	}

	// Everyone asks first, then adds in the background.
	out := f.submitTaskForm(t, "T123", "UBOB", "VAUD2", taskForm{everyone: true, text: "Fill in the survey"})
	confirmView, _ := out["view"].(map[string]any)
	if out["response_action"] != "update" || confirmView["callback_id"] != taskqueue.ConfirmView || !strings.Contains(fmt.Sprint(confirmView["submit"]), "Give 24 copies") || !strings.Contains(fmt.Sprint(confirmView["blocks"]), "everyone in the workspace") {
		t.Fatalf("the everyone submission: %v", out)
	}
	if len(f.postsTo("UM000")) != 0 {
		t.Fatalf("an unconfirmed form was filed: %v", f.replies)
	}
	confirmView["id"] = "VAUD2"
	if out = f.submitView(t, "T123", "UBOB", confirmView); out["response_action"] != "update" || !strings.Contains(fmt.Sprint(out["view"]), "Adding 24 copies") {
		t.Fatalf("the confirmation: %v", out)
	}
	if ran, err := f.app.automations.Queue.RunNext(t.Context(), f.app.automationrun.RunAutomation); err != nil || !ran {
		t.Fatalf("the confirmed form did not run: %v %v", ran, err)
	}
	if len(f.postsTo("UM000")) != 1 || len(f.postsTo("UALICE")) != 2 || len(f.postsTo("UBOB")) == 0 {
		t.Fatalf("everyone's notices = %v", f.replies)
	}
	if done := f.lastPostTo(t, "UBOB")["text"].(string); !strings.Contains(done, "Added a copy for 24 people") {
		t.Fatalf("bob's confirmation = %q", done)
	}

	// Another member cannot submit someone else's confirmation.
	if out = f.submitView(t, "T123", "UALICE", confirmView); !strings.Contains(fmt.Sprint(out["view"]), "Only whoever asked") {
		t.Fatalf("someone else's confirmation: %v", out)
	}
	if ran, _ := f.app.automations.Queue.RunNext(t.Context(), f.app.automationrun.RunAutomation); ran {
		t.Fatal("someone else's confirmation queued work")
	}
}

// taskToolError calls a tool that should fail and returns its message.
func taskToolError(t *testing.T, client *mcp.ClientSession, name string, args any) string {
	t.Helper()
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(name, err)
	}
	raw, _ := json.Marshal(result.Content)
	if !result.IsError {
		t.Fatalf("%s succeeded: %s", name, raw)
	}
	return string(raw)
}

func TestAddTaskGivesChannelsGroupsAndEveryoneACopyThroughMCP(t *testing.T) {
	f := newTaskFixture(t)
	f.members = append([]map[string]any{slackMember("UALICE"), slackMember("UBOB")}, crowd(23)...)
	f.channels = map[string][]string{"C1": {"UALICE", "UBOB", "UM000"}}
	clients := taskClients(t, f.app, "alice", "carol")
	alice, carol := clients[0], clients[1]

	added := decodeTask[taskqueue.Added](t, actionCall(t, alice, "add_task", map[string]any{"people": []string{"<#C1>"}, "text": "Count the stock"}, false))
	if len(added.Tasks) != 2 || added.Tasks[0].Owner.SlackUser != "UBOB" || added.Tasks[1].Owner.SlackUser != "UM000" {
		t.Fatalf("the channel's copies = %+v", added.Tasks)
	}

	// More than 20 people waits for confirm.
	if message := taskToolError(t, alice, "add_task", map[string]any{"people": []string{"everyone"}, "text": "Fill in the survey"}); !strings.Contains(message, "24 people") || !strings.Contains(message, "confirm") {
		t.Fatalf("an unconfirmed crowd = %s", message)
	}
	if len(f.postsTo("UM001")) != 0 {
		t.Fatalf("an unconfirmed crowd was filed: %v", f.replies)
	}
	added = decodeTask[taskqueue.Added](t, actionCall(t, alice, "add_task", map[string]any{"people": []string{"everyone"}, "text": "Fill in the survey", "confirm": true}, false))
	if len(added.Tasks) != 24 || len(f.postsTo("UM001")) != 1 {
		t.Fatalf("the confirmed crowd = %d copies", len(added.Tasks))
	}

	// Copies of a recurring task are one line in the requester's list.
	actionCall(t, alice, "add_task", map[string]any{"people": []string{"<#C1>"}, "text": "Water the plants", "due": "2026-12-01", "every": "weekly"}, false)
	if reply := f.memberDM(t, "UALICE", "7.1", "tasks"); strings.Count(reply, "Water the plants") != 1 {
		t.Fatalf("alice's list = %q", reply)
	}

	// Someone with no workspace member can name everyone, not a channel.
	if message := taskToolError(t, carol, "add_task", map[string]any{"people": []string{"<#C1>"}, "text": "Count the stock"}); !strings.Contains(message, "channels you're in") {
		t.Fatalf("carol's channel = %s", message)
	}
}
