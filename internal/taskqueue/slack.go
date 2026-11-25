package taskqueue

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/tasks"
	"forgetmenote/internal/webcore"
)

// taskCommandPerson is one person, channel, user group or everyone a task
// command names, and taskCommandPeople several, separated by spaces, commas or
// "and".
const (
	taskCommandPerson = `(?:<@[A-Z0-9]+(?:\|[^>]*)?>|<#[CG][A-Z0-9]+(?:\|[^>]*)?>|<!subteam\^[A-Z0-9]+(?:\|[^>]*)?>|<!everyone>|me|everyone)`
	taskCommandPeople = taskCommandPerson + `(?:(?:\s*,\s*|\s+)(?:and\s+)?` + taskCommandPerson + `)*`
)

// Slack task commands are answered here, never by the model, so a task's
// recipients and text are exactly what the requester typed:
//
//	task @person Review the Q4 deck due 2026-11-21
//	task me Draft holiday copy
//	task @alex, @sam and me Check the proofs
//	task #design Review the brief
//	task @leads Plan the offsite
//	task everyone Fill in the survey
//	tasks
//	task help
//
// Several people, separated by spaces, commas or "and", each get their own
// copy, as does each member of a channel, user group or the workspace named,
// other than the requester. A request for more than taskConfirmAbove copies
// asks the requester first, with buttons. A command that plainly is one but
// cannot be read, such as `task` alone, people with no task, or an @name Slack
// did not turn into a mention, gets the cheat sheet. Other messages that mention tasks still go to the
// assistant.
var (
	taskListCommand  = regexp.MustCompile(`(?i)^\s*(?:<@[A-Z0-9]+>\s*)?tasks\s*[.!?]?\s*$`)
	taskAddCommand   = regexp.MustCompile(`(?is)^\s*(?:<@[A-Z0-9]+>\s*)?task\s+(` + taskCommandPeople + `)\s+(.+?)(?:\s+due:?\s+([0-9]{4}-[0-9]{2}-[0-9]{2}))?\s*$`)
	taskHelpAsked    = regexp.MustCompile(`(?i)^\s*(?:<@[A-Z0-9]+>\s*)?tasks?\s+help\s*[.!?]?\s*$`)
	taskHelpCommand  = regexp.MustCompile(`(?is)^\s*(?:<@[A-Z0-9]+>\s*)?task(?:\s+(?:@\S.*|` + taskCommandPeople + `))?\s*[.!?]?\s*$`)
	taskCommandNames = regexp.MustCompile(`(?i)` + taskCommandPerson)
	// taskConfirmed is the request a confirm button or form queues: the
	// operation the unconfirmed request had, then the request.
	taskConfirmed = regexp.MustCompile(`(?s)^task-confirmed ([a-z0-9/-]{1,128})\n(\{.*\})$`)
)

const taskListButtons = 20

// AddView is the /task form, and ConfirmView the form asking to give
// more than taskConfirmAbove people a copy.
const (
	AddView     = "task_add"
	ConfirmView = "task_add_confirm"
)

// Shortcut is the message shortcut that opens the task form for a
// message.
const Shortcut = "task_from_message"

// A task links the Slack conversation it came from: a message link as Slack
// writes one in a message, <url> or <url|label>, or as pasted in the form.
const taskSlackURL = `https://[a-z0-9-]+(?:\.enterprise)?\.slack\.com/archives/[A-Z0-9]+/p[0-9]{10,20}(?:\?[^\s<>|]*)?`

var (
	taskSlackLink  = regexp.MustCompile(`^` + taskSlackURL + `$`)
	taskPastedLink = regexp.MustCompile(`<(` + taskSlackURL + `)(?:\|[^>]*)?>`)
	// slackMarkup is a mention, channel, special mention or link in Slack's
	// message format.
	slackMarkup = regexp.MustCompile(`<([@#!])?([^<>|]*)(?:\|([^<>]*))?>`)
)

// taskSourceLink links where a task came from, in Slack's format.
func taskSourceLink(source string) string {
	if source == "" || tasks.CheckSource(source) != nil || strings.ContainsAny(source, "<>|") {
		return ""
	}
	label := "source"
	if taskSlackLink.MatchString(source) {
		label = "Slack conversation"
	}
	return "<" + strings.ReplaceAll(source, "&", "&amp;") + "|" + label + ">"
}

// taskSourceSuffix ends a list line with the task's link, if it has one.
func taskSourceSuffix(t View) string {
	if link := taskSourceLink(t.Source); link != "" {
		return " · " + link
	}
	return ""
}

// slackTaskInput reads a typed task command. It links the Slack message
// pasted in it, taking the link out of the task, or else the channel thread
// it was typed in. A thread the app cannot read is left unlinked.
func (s *Service) slackTaskInput(ctx context.Context, d messaging.Address, text string) (Input, bool) {
	people, task, due, ok := AddParts(text)
	if !ok {
		return Input{}, false
	}
	in := Input{People: people, Text: task, Due: due}
	if m := taskPastedLink.FindStringSubmatch(task); m != nil {
		in.Source = strings.ReplaceAll(m[1], "&amp;", "&")
		in.Text = strings.Join(strings.Fields(strings.Replace(task, m[0], " ", 1)), " ")
	} else if !d.Direct && d.Client == "slack" {
		in.Source = s.slackPermalink(ctx, d.Channel, cmp.Or(d.Thread, d.Message))
	}
	return in, true
}

// slackPermalink links a message the app can see, within a second and a half.
func (s *Service) slackPermalink(ctx context.Context, channel, ts string) string {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	link, err := s.hooks.Permalink(ctx, channel, ts)
	if err != nil {
		if !errors.Is(err, messaging.ErrAudienceHidden) {
			s.logAutomationError("Slack task link", err)
		}
		return ""
	}
	if !taskSlackLink.MatchString(link) || tasks.CheckSource(link) != nil {
		return ""
	}
	return link
}

// slackPlainText makes a Slack message readable as one line of task text:
// mentions become names, read within ctx, and links their labels.
func (s *Service) slackPlainText(ctx context.Context, text string) string {
	text = slackMarkup.ReplaceAllStringFunc(text, func(markup string) string {
		m := slackMarkup.FindStringSubmatch(markup)
		switch {
		case m[1] == "@":
			if name := s.MemberNameWithin(ctx, m[2]); name != "" {
				return "@" + name
			}
			return "@" + cmp.Or(m[3], "someone")
		case m[1] == "#":
			return "#" + cmp.Or(m[3], "channel")
		case m[3] != "":
			return m[3]
		case m[1] == "!":
			return "@" + strings.SplitN(m[2], "^", 2)[0]
		}
		return m[2]
	})
	text = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(text)
	return strings.Join(strings.Fields(text), " ")
}

// The buttons that confirm or cancel a request giving many people a copy.
const (
	ConfirmAction = "task_confirm"
	CancelAction  = "task_cancel"
)

// taskNoQueue answers a list from someone who has never had a task.
const taskNoQueue = "You have no open tasks. Give someone a task with `task @person what to do`, adding `due 2026-11-21` if it has a date."

func Command(text string) bool {
	return taskListCommand.MatchString(text) || taskAddCommand.MatchString(text) || taskHelpAsked.MatchString(text) || taskHelpCommand.MatchString(text) || taskConfirmed.MatchString(text)
}

// taskConfirmValue is the value of a confirm or cancel button: the run that
// asked, and the number of copies it would give.
type taskConfirmValue struct {
	Run   string `json:"run"`
	Count int    `json:"count"`
}

// taskConfirmedEvent names the request a confirmation queues, so confirming
// twice queues it once, and only the server can make one.
func taskConfirmedEvent(operation, request string) string {
	return "task-confirmed-" + automation.Hash([]string{operation, request})[:32]
}

// queueConfirmedTasks queues a confirmed request to run in the background,
// answering in the conversation d. The run reads the request back only if its
// ID shows this server queued it.
func (s *Service) queueConfirmedTasks(ctx context.Context, d messaging.Address, operation string, in Input) error {
	in.Confirm = true
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	d.Message, d.Files = "", nil
	return s.hooks.Enqueue(ctx, messaging.Event{ID: taskConfirmedEvent(operation, string(raw)), Text: "task-confirmed " + operation + "\n" + string(raw), Address: d})
}

// confirmedTaskInput reads a confirmed request from its run, which must be
// the one queueConfirmedTasks made.
func confirmedTaskInput(r *automation.Run, d messaging.Address) (Input, string, bool) {
	m := taskConfirmed.FindStringSubmatch(r.Spec.Instruction)
	var in Input
	if m == nil || d.Message != "" || r.Spec.ID != "chat-"+automation.Hash([]string{d.Client, d.Workspace, taskConfirmedEvent(m[1], m[2])})[:32] || json.Unmarshal([]byte(m[2]), &in) != nil || !in.Confirm {
		return Input{}, "", false
	}
	return in, m[1], true
}

// AddParts reads a task command: the people named, the task and its due
// date.
func AddParts(text string) (people []string, task, due string, ok bool) {
	m := taskAddCommand.FindStringSubmatch(text)
	if m == nil {
		return nil, "", "", false
	}
	return taskCommandNames.FindAllString(m[1], -1), strings.TrimSpace(m[2]), m[3], true
}

// slackTaskQueues returns the task queue settings when task queues serve the
// Slack workspace team.
func (s *Service) SlackQueues(team string) (catalog.TaskQueues, bool) {
	settings, _, err := s.taskQueues()
	return settings, err == nil && settings.Client == "slack" && settings.Workspace == team
}

// errNotTaskMember refuses someone the messaging client does not confirm as
// an active, human, non-guest member of the task workspace.
var errNotTaskMember = errors.New("not a member of the task workspace")

// slackTaskMember is the Slack member pressing a task button or submitting a
// task form, confirmed with Slack, and their queue's person, made when create
// is set.
func (s *Service) slackTaskMember(ctx context.Context, settings catalog.TaskQueues, user string, create bool) (Caller, error) {
	if err := s.verifyTaskMember(ctx, settings, user); err != nil {
		return Caller{}, fmt.Errorf("%w: %w", errNotTaskMember, err)
	}
	return s.MemberCaller(settings, user, create)
}

// memberTaskCaller is the member writing in Slack, whose membership the
// conversation already verified.
func (s *Service) MemberCaller(settings catalog.TaskQueues, user string, create bool) (Caller, error) {
	person, err := s.catalog.TaskPersonForMember(settings.Client, settings.Workspace, user, create)
	if err != nil {
		return Caller{}, err
	}
	account, _ := s.catalog.TaskPersonAccount(person.ID)
	return Caller{Person: person, Account: account}, nil
}

// taskCommandReply answers a task command in a conversation. ok is false when
// task queues do not serve this client, so the assistant answers instead.
func (s *Service) CommandReply(ctx context.Context, r *automation.Run, d messaging.Address) (messaging.Reply, bool, error) {
	settings, _, err := s.taskQueues()
	if err != nil || settings.Client != d.Client || settings.Workspace != d.Workspace {
		return messaging.Reply{}, false, nil
	}
	text := r.Spec.Instruction
	if taskHelpAsked.MatchString(text) || taskHelpCommand.MatchString(text) && !taskAddCommand.MatchString(text) {
		return messaging.Reply{Text: taskHelpReply(text)}, true, nil
	}
	if taskListCommand.MatchString(text) {
		if !d.Direct || d.Shared {
			return messaging.Reply{Text: "Your queue is private: send me `tasks` in a direct message."}, true, nil
		}
		caller, err := s.MemberCaller(settings, d.User, false)
		if errors.Is(err, catalog.ErrNoTaskPerson) {
			return messaging.Reply{Text: taskNoQueue}, true, nil
		}
		if err != nil {
			return messaging.Reply{}, true, err
		}
		list, err := s.List(ctx, caller)
		if err != nil {
			return messaging.Reply{}, true, err
		}
		return s.taskListReply(list), true, nil
	}
	in, operation, ok := Input{}, "slack-"+r.ID, false
	if taskConfirmed.MatchString(text) {
		if in, operation, ok = confirmedTaskInput(r, d); !ok {
			return messaging.Reply{Text: "That isn't a request I can add. Send the task command again."}, true, nil
		}
	} else if in, ok = s.slackTaskInput(ctx, d, text); !ok {
		return messaging.Reply{}, false, nil
	}
	caller, err := s.MemberCaller(settings, d.User, true)
	if err != nil {
		return messaging.Reply{}, true, err
	}
	views, err := s.addTasks(ctx, caller, in, operation)
	var confirm taskConfirmation
	var input webcore.Error
	switch {
	case errors.As(err, &confirm):
		value, _ := json.Marshal(taskConfirmValue{Run: r.ID, Count: confirm.Count})
		copies := "Give " + strconv.Itoa(confirm.Count) + " copies"
		return messaging.Reply{Text: confirm.question() + " Go ahead?\n" + taskQuote(in.Text, in.Due, in.Source), Buttons: []messaging.Button{
			{Action: ConfirmAction, Label: copies, Value: string(value), Style: "primary"},
			{Action: CancelAction, Label: "Cancel", Value: string(value)},
		}}, true, nil
	case errors.As(err, &input) && input.Status < 500:
		return messaging.Reply{Text: "I didn't add that task: " + input.Message}, true, nil
	case err != nil:
		return messaging.Reply{}, true, err
	}
	return messaging.Reply{Text: taskAddedText(views)}, true, nil
}

// taskQuote quotes a task, its due date and where it came from.
func taskQuote(text, due, source string) string {
	quote := "> " + mrkdwnEscape.Replace(strings.TrimSpace(text))
	if due != "" {
		quote += "\n> Due " + due
	}
	if link := taskSourceLink(source); link != "" {
		quote += "\n> " + link
	}
	return quote
}

// slackTaskConfirm answers a confirm or cancel button under a request for
// many copies. Only the member who asked can press them; confirming queues
// the request to run in the background, in the same conversation.
func (s *Service) SlackConfirm(w http.ResponseWriter, r *http.Request, settings catalog.TaskQueues, user, action, raw, responseURL string) {
	var value taskConfirmValue
	if json.Unmarshal([]byte(raw), &value) != nil || value.Run == "" || len(value.Run) > 128 {
		http.Error(w, "Invalid interaction", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2500*time.Millisecond)
	defer cancel()
	run, err := s.automations.Queue.Get(value.Run)
	d := run.Spec.Delivery
	_, _, _, ok := AddParts(run.Spec.Instruction)
	respond, text := s.hooks.Respond, ""
	switch {
	case err != nil || d == nil || d.Client != settings.Client || d.Workspace != settings.Workspace || !ok:
		text = "That request has expired; send the task again."
	case d.User != user:
		respond, text = s.hooks.RespondAside, "Only whoever asked can confirm or cancel this."
	case action == CancelAction:
		text = "Cancelled; nothing was added."
	default:
		in, _ := s.slackTaskInput(ctx, *d, run.Spec.Instruction)
		if err = s.queueConfirmedTasks(ctx, *d, "slack-"+run.ID, in); err != nil {
			s.logAutomationError("Slack task confirmation", err)
			respond, text = s.hooks.RespondAside, "I couldn't start that just now; press the button again in a moment."
		} else {
			text = "Adding " + strconv.Itoa(value.Count) + " copies now. I'll confirm here when they're all added.\n" + taskQuote(in.Text, in.Due, in.Source)
		}
	}
	if err := respond(ctx, responseURL, text); err != nil {
		s.logAutomationError("Slack task confirmation response", err)
	}
	w.WriteHeader(http.StatusOK)
}

// taskNameList joins names as a sentence does: a, b and c.
func taskNameList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func taskName(p PersonView) string {
	switch {
	case p.You:
		return "you"
	case p.Name != "":
		return p.Name
	case p.SlackUser != "":
		return "Slack member " + p.SlackUser
	}
	return "a colleague"
}

// taskLine shows a task in Slack's message format, its text and who gave it
// escaped.
func taskLine(t View) string {
	line := mrkdwnEscape.Replace(t.Text)
	details := []string{}
	if t.Due != "" {
		details = append(details, "due "+t.Due)
	}
	if t.Every != nil {
		details = append(details, "every "+strings.TrimSuffix(t.Every.String(), " from "+t.Every.Start))
	}
	if t.Priority != "" {
		details = append(details, t.Priority+" priority")
	}
	if t.Snoozed != "" {
		details = append(details, "snoozed until "+t.Snoozed)
	}
	if !t.Requester.You {
		details = append(details, "from "+mrkdwnEscape.Replace(taskName(t.Requester)))
	}
	if len(details) > 0 {
		line += " _(" + strings.Join(details, ", ") + ")_"
	}
	return line
}

// taskListReply shows the member's queue, with a Done button beside each of
// the first tasks, and the tasks they gave others.
func (s *Service) taskListReply(list List) messaging.Reply {
	var b strings.Builder
	reply := messaging.Reply{}
	if len(list.Mine) == 0 {
		b.WriteString("You have no open tasks.")
	} else {
		fmt.Fprintf(&b, "You have %d open task", len(list.Mine))
		if len(list.Mine) != 1 {
			b.WriteString("s")
		}
		b.WriteString(":")
	}
	for i, t := range list.Mine {
		if i < taskListButtons {
			value, _ := json.Marshal(taskButtonValue{Task: t.ID, Revision: t.Revision})
			reply.Sections = append(reply.Sections, messaging.Section{Text: "• " + taskLine(t) + taskSourceSuffix(t), Buttons: []messaging.Button{{Action: DoneAction, Label: "Done", Value: string(value)}}})
			continue
		}
		b.WriteString("\n• " + taskLine(t) + taskSourceSuffix(t))
	}
	if len(list.Filed) > 0 {
		b.WriteString("\n\nTasks you gave others:")
		// Copies of one task are one line naming their owners.
		groups, order := map[string][]View{}, []string{}
		for _, t := range list.Filed {
			every := ""
			if t.Every != nil {
				every = t.Every.String()
			}
			key := t.Text + "\x00" + t.Due + "\x00" + every
			if groups[key] == nil {
				order = append(order, key)
			}
			groups[key] = append(groups[key], t)
		}
		for _, key := range order {
			copies, names := groups[key], []string{}
			for _, t := range copies {
				names = append(names, mrkdwnEscape.Replace(taskName(t.Owner)))
			}
			slices.Sort(names)
			whom := taskNameList(names)
			if len(names) > 5 {
				whom = strconv.Itoa(len(names)) + " people: " + strings.Join(names[:3], ", ") + " and " + strconv.Itoa(len(names)-3) + " others"
			}
			b.WriteString("\n• " + mrkdwnEscape.Replace(copies[0].Text) + " _(" + whom + ")_" + taskSourceSuffix(copies[0]))
		}
	}
	reply.Text = b.String()
	return reply
}

// mrkdwnEscape keeps Slack's mrkdwn from turning <url|label> in a colleague's
// text into a link. Every task text, reason and name in a Slack message goes
// through it.
var mrkdwnEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func validTaskButton(raw string) (taskButtonValue, bool) {
	var value taskButtonValue
	ok := json.Unmarshal([]byte(raw), &value) == nil && tasks.ValidID(value.Task) && len(value.Revision) <= 64 && (value.Until == "" || validTaskDate(value.Until))
	return value, ok
}

func validTaskDate(value string) bool {
	_, err := time.Parse(time.DateOnly, value)
	return err == nil
}

// slackTaskButton closes or snoozes a task from a Done or Snooze button.
func (s *Service) SlackButton(w http.ResponseWriter, r *http.Request, settings catalog.TaskQueues, user, action, raw, responseURL, pressID string) {
	value, valid := validTaskButton(raw)
	if !valid {
		http.Error(w, "Invalid interaction", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	update := Update{ID: value.Task, Revision: value.Revision, Outcome: "done"}
	if action == SnoozeAction {
		// A week from the day the reminder was sent, or from today when that
		// has passed.
		until := value.Until
		if today := s.taskTime().Format(time.DateOnly); until <= today {
			until = s.taskTime().AddDate(0, 0, 7).Format(time.DateOnly)
		}
		update = Update{ID: value.Task, Revision: value.Revision, SnoozeUntil: &until}
	}
	text, _ := s.slackTaskChange(ctx, settings, user, update, pressID)
	if err := s.hooks.Respond(ctx, responseURL, text); err != nil {
		s.logAutomationError("Slack task button response", err)
	}
	w.WriteHeader(http.StatusOK)
}

// slackTaskChange closes or snoozes a task as the member who pressed a button
// or submitted a form, checked against the task as it is now. It returns what
// to tell them, and whether the task changed.
func (s *Service) slackTaskChange(ctx context.Context, settings catalog.TaskQueues, user string, update Update, pressID string) (string, bool) {
	caller, err := s.slackTaskMember(ctx, settings, user, false)
	switch {
	case errors.Is(err, errNotTaskMember):
		return "Only members of this workspace can change tasks.", false
	case err != nil:
		return "That task is not in your queue.", false
	}
	result, err := s.changeTask(ctx, caller, update, pressID)
	closed := "Done"
	if update.Outcome == "not_applicable" {
		closed = "Marked not applicable"
	}
	var known webcore.Error
	switch {
	case err == nil && update.SnoozeUntil != nil:
		day, _ := time.Parse(time.DateOnly, *update.SnoozeUntil)
		return "Snoozed until " + day.Format("Mon Jan 2") + ". It returns to your reminder that day.", true
	case err == nil && result.Occurrence != nil:
		return closed + " for " + result.Occurrence.Closed + ". It repeats; next due " + result.Occurrence.Next + ".", true
	case err == nil:
		return closed + ". I've let whoever gave it to you know.", true
	case errors.As(err, &known) && known.Status < 500:
		return known.Message + ". Send me `tasks` to see your queue.", false
	}
	s.logAutomationError("Slack task change", err)
	return "I couldn't change that task just now; try again in a moment.", false
}

// slackTaskReasonModal asks why a task does not apply. It shows nothing of the
// task: whoever submits it is checked against the task then.
func (s *Service) SlackReasonModal(w http.ResponseWriter, r *http.Request, raw, trigger string) {
	if _, valid := validTaskButton(raw); !valid {
		http.Error(w, "Invalid interaction", http.StatusBadRequest)
		return
	}
	view := map[string]any{
		"type": "modal", "callback_id": NotApplicableView, "private_metadata": raw,
		"title":  map[string]string{"type": "plain_text", "text": "Not applicable"},
		"submit": map[string]string{"type": "plain_text", "text": "Close task"},
		"close":  map[string]string{"type": "plain_text", "text": "Cancel"},
		"blocks": []map[string]any{{
			"type": "input", "block_id": "reason",
			"label":   map[string]string{"type": "plain_text", "text": "Why doesn't this task apply?"},
			"hint":    map[string]string{"type": "plain_text", "text": "Whoever gave you the task sees this reason."},
			"element": map[string]any{"type": "plain_text_input", "action_id": "reason", "max_length": 200},
		}},
	}
	// Slack accepts a modal only within three seconds of the press.
	ctx, cancel := context.WithTimeout(r.Context(), 2500*time.Millisecond)
	defer cancel()
	if err := s.hooks.OpenView(ctx, trigger, view); err != nil {
		s.logAutomationError("Slack task modal", err)
	}
	w.WriteHeader(http.StatusOK)
}

// slackTaskReason closes a task as not applicable from the submitted modal,
// as the submitting member, and answers in the modal.
func (s *Service) SlackReason(w http.ResponseWriter, r *http.Request, settings catalog.TaskQueues, user, raw, reason, pressID string) {
	value, valid := validTaskButton(raw)
	if !valid {
		http.Error(w, "Invalid interaction", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	text, closed := s.slackTaskChange(ctx, settings, user, Update{ID: value.Task, Revision: value.Revision, Outcome: "not_applicable", Reason: strings.TrimSpace(reason)}, pressID)
	answer := map[string]any{"response_action": "errors", "errors": map[string]string{"reason": text}}
	if closed {
		answer = map[string]any{"response_action": "update", "view": map[string]any{
			"type": "modal", "title": map[string]string{"type": "plain_text", "text": "Not applicable"},
			"close":  map[string]string{"type": "plain_text", "text": "Close"},
			"blocks": []map[string]any{{"type": "section", "text": map[string]string{"type": "plain_text", "text": text}}},
		}}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(answer)
}
