package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/tasks"
	"forgetmenote/internal/webcore"
)

// taskHelp is the cheat sheet `task help` and `/task help` answer with.
const taskHelp = "*Tasks*\n" +
	"• `task @person what to do` gives someone a task. End with `due 2026-11-21` for a date.\n" +
	"• `task me what to do` adds one to your own queue.\n" +
	"• `task @alex, @sam and me what to do` gives each person their own copy, up to 20.\n" +
	"• `task #channel what to do`, `task @group …` or `task everyone …` gives everyone else there a copy. Over 20 people I ask first; 200 is the most.\n" +
	"• `/task` opens a form for the same, with a date picker and, for a task only you get, a priority.\n" +
	"• Type a task in a thread, or paste a message's link into it, to link that conversation. On any message, ⋯ → *Create task* opens the form linked to it.\n" +
	"• `tasks` lists your queue and the tasks you gave, with Done buttons. `/tasks` shows the same anywhere, to you alone.\n" +
	"Pick each person, channel and group from Slack's list as you type, so the name becomes a mention."

// taskHelpReply answers a request for help, or a task command that cannot be
// read, with the cheat sheet.
func taskHelpReply(text string) string {
	if taskHelpAsked.MatchString(text) {
		return taskHelp
	}
	return "I couldn't add that: name each person with a mention, then say what to do.\n\n" + taskHelp
}

// slackCommands answers the /task and /tasks slash commands, which Slack signs
// as it signs interactions and waits three seconds for: `help` with the cheat
// sheet; /tasks with the member's own list, which only they see; /task with
// the task form, the text filled in.
func (s *Server) slackCommands(w http.ResponseWriter, r *http.Request) {
	body, ok := s.slackRequest(w, r)
	if !ok {
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "Invalid command", http.StatusBadRequest)
		return
	}
	team, user, trigger := form.Get("team_id"), form.Get("user_id"), form.Get("trigger_id")
	if !s.slackWorkspace(w, team) {
		return
	}
	if !messaging.ValidID(user) || trigger == "" || len(trigger) > 256 {
		http.Error(w, "Invalid command", http.StatusBadRequest)
		return
	}
	settings, on := s.slackTaskQueues(team)
	text := strings.Join(strings.Fields(form.Get("text")), " ")
	word := strings.ToLower(strings.TrimRight(text, ".!?"))
	switch {
	case !on:
		slackEphemeral(w, "Tasks are not turned on in this workspace.")
		return
	case word == "help":
		slackEphemeral(w, taskHelp)
		return
	case strings.EqualFold(form.Get("command"), "/tasks"):
		s.slackTaskList(w, r, settings, user)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2500*time.Millisecond)
	defer cancel()
	if err := s.slack.OpenView(ctx, trigger, taskAddForm(user, text, "")); err != nil {
		s.logAutomationError("Slack task form", err)
		slackEphemeral(w, "I couldn't open the task form just now. Try again, or send me `task @person what to do`.")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// slackTaskList answers a list command with the member's queue, a Done button
// beside each task, and the tasks they gave, as the `tasks` message does.
func (s *Server) slackTaskList(w http.ResponseWriter, r *http.Request, settings catalog.TaskQueues, user string) {
	ctx, cancel := context.WithTimeout(r.Context(), 2500*time.Millisecond)
	defer cancel()
	reply := messaging.Reply{}
	if caller, err := s.slackTaskMember(ctx, settings, user, false); errors.Is(err, errNotTaskMember) {
		reply.Text = "Only members of this workspace have task queues."
	} else if errors.Is(err, catalog.ErrNoTaskPerson) {
		reply.Text = taskNoQueue
	} else if err != nil {
		s.logAutomationError("Slack task list", err)
		reply.Text = "I couldn't read your tasks just now; try again in a moment."
	} else if list, err := s.listTasks(ctx, caller); err != nil {
		s.logAutomationError("Slack task list", err)
		reply.Text = "I couldn't read your tasks just now; try again in a moment."
	} else {
		reply = s.taskListReply(list)
	}
	jsonResponse(w, messaging.SlackCommandReply(reply, "_The list goes on: send me `tasks` in a direct message to see all of it._"))
}

// slackTaskShortcut answers the Create task message shortcut with the task
// form, its text from the message and the message linked. A message the app
// cannot see opens the form unlinked.
func (s *Server) slackTaskShortcut(w http.ResponseWriter, r *http.Request, team, user, channel, ts, text, trigger, responseURL string) {
	if !messaging.ValidID(user) || trigger == "" || len(trigger) > 256 || len(text) > 40000 {
		http.Error(w, "Invalid interaction", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2500*time.Millisecond)
	defer cancel()
	if _, on := s.slackTaskQueues(team); !on {
		if err := s.slack.RespondAside(ctx, responseURL, "Tasks are not turned on in this workspace."); err != nil {
			s.logAutomationError("Slack task shortcut", err)
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	// Slack takes the form only within three seconds of the shortcut.
	link := s.slackPermalink(ctx, channel, ts)
	names, stop := context.WithTimeout(ctx, 800*time.Millisecond)
	text = s.slackPlainText(names, text)
	stop()
	if err := s.slack.OpenView(ctx, trigger, taskAddForm(user, text, link)); err != nil {
		s.logAutomationError("Slack task shortcut", err)
	}
	w.WriteHeader(http.StatusOK)
}

// slackEphemeral answers a slash command with a message only its sender sees.
func slackEphemeral(w http.ResponseWriter, text string) {
	jsonResponse(w, map[string]string{"response_type": "ephemeral", "text": text})
}

// taskAddForm is the /task form. It starts with the member who opened it, the
// text they typed after the command or the message they chose, and that
// message's link.
func taskAddForm(user, text, link string) map[string]any {
	plain := func(text string) map[string]string { return map[string]string{"type": "plain_text", "text": text} }
	option := func(value, label string) map[string]any { return map[string]any{"value": value, "text": plain(label)} }
	task := map[string]any{"type": "plain_text_input", "action_id": "text", "max_length": 500, "placeholder": plain("What to do")}
	if text != "" {
		if runes := []rune(text); len(runes) > 500 {
			text = string(runes[:500])
		}
		task["initial_value"] = text
	}
	linkInput := map[string]any{"type": "url_text_input", "action_id": "link", "placeholder": plain("https://…slack.com/archives/…")}
	if link != "" {
		linkInput["initial_value"] = link
	}
	return map[string]any{
		"type": "modal", "callback_id": taskAddView,
		"title": plain("New task"), "submit": plain("Add task"), "close": plain("Cancel"),
		"blocks": []map[string]any{
			{"type": "input", "block_id": "people", "optional": true, "label": plain("People"), "hint": plain("Each person gets their own copy and closes it themselves."),
				"element": map[string]any{"type": "multi_users_select", "action_id": "people", "initial_users": []string{user}, "max_selected_items": taskPeopleLimit}},
			{"type": "input", "block_id": "channels", "optional": true, "label": plain("Channels"), "hint": plain("Everyone else in each channel gets a copy. You must be in it, and I must be in a private one."),
				"element": map[string]any{"type": "multi_conversations_select", "action_id": "channels", "max_selected_items": taskFormChannels, "filter": map[string]any{"include": []string{"public", "private"}, "exclude_bot_users": true}}},
			{"type": "input", "block_id": "everyone", "optional": true, "label": plain("Everyone"),
				"element": map[string]any{"type": "checkboxes", "action_id": "everyone", "options": []map[string]any{option("everyone", "Everyone in the workspace")}}},
			{"type": "input", "block_id": "text", "label": plain("Task"), "element": task},
			{"type": "input", "block_id": "link", "optional": true, "label": plain("Slack link"), "hint": plain("A Slack message or thread this task is about: Copy link in the message's menu."), "element": linkInput},
			{"type": "input", "block_id": "due", "optional": true, "label": plain("Due"), "element": map[string]any{"type": "datepicker", "action_id": "due"}},
			{"type": "input", "block_id": "priority", "optional": true, "label": plain("Priority"), "hint": plain("Only for a task you alone get."),
				"element": map[string]any{"type": "static_select", "action_id": "priority", "options": []map[string]any{option("high", "High"), option("normal", "Normal"), option("low", "Low")}}},
		},
	}
}

// taskFormChannels is the most channels the /task form takes.
const taskFormChannels = 5

// slackTaskAdd adds the task from a submitted /task form as the submitting
// member, for the people and channels chosen and, when asked, everyone.
// Problems are answered beside their fields; an added task closes the form and
// is confirmed in a direct message. The work outlives Slack's three seconds,
// so a slow submission is still added once, and submitting the same form again
// finds it. A form for more than taskConfirmAbove people asks first.
func (s *Server) slackTaskAdd(w http.ResponseWriter, r *http.Request, settings catalog.TaskQueues, user string, in taskInput, channels []string, everyone bool, operation string) {
	in.Text = strings.TrimSpace(in.Text)
	problems := map[string]string{}
	switch {
	case len(in.People) == 0 && len(channels) == 0 && !everyone:
		problems["people"] = "Choose people, a channel or everyone."
	case len(in.People) > taskPeopleLimit:
		problems["people"] = "Choose at most " + strconv.Itoa(taskPeopleLimit) + " people."
	case len(channels) > taskFormChannels:
		problems["channels"] = "Choose at most " + strconv.Itoa(taskFormChannels) + " channels."
	}
	for _, channel := range channels {
		if !messaging.ValidID(channel) || !strings.HasPrefix(channel, "C") && !strings.HasPrefix(channel, "G") {
			problems["channels"] = "Choose channels, not direct messages."
		}
		in.People = append(in.People, "<#"+channel+">")
	}
	if everyone {
		in.People = append(in.People, "everyone")
	}
	if err := tasks.CheckText(in.Text); err != nil {
		problems["text"] = taskSentence(err.Error())
	}
	if in.Due != "" && !validTaskDate(in.Due) {
		problems["due"] = "Choose a date."
	}
	if in.Source = strings.TrimSpace(in.Source); in.Source != "" && (!taskSlackLink.MatchString(in.Source) || tasks.CheckSource(in.Source) != nil) {
		problems["link"] = "Paste a Slack message link: Copy link in the message's menu."
	}
	switch in.Priority {
	case "", "normal":
	case "high", "low":
		if len(in.People) != 1 || in.People[0] != user {
			problems["priority"] = "Only a task you alone get has a priority."
		}
	default:
		problems["priority"] = "Choose high, normal or low."
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	var caller taskCaller
	var views []taskView
	if len(problems) == 0 {
		var input webcore.Error
		var confirm taskConfirmation
		var err error
		caller, err = s.slackTaskMember(ctx, settings, user, true)
		if err == nil {
			views, err = s.addTasks(ctx, caller, in, operation)
		}
		switch {
		case errors.Is(err, errNotTaskMember):
			problems["people"] = "Only members of this workspace can give tasks."
		case errors.As(err, &confirm):
			if view, ok := taskConfirmForm(confirm, user, in); ok {
				jsonResponse(w, map[string]any{"response_action": "update", "view": view})
				return
			}
			problems["text"] = "Shorten the task to give this many people a copy."
		case errors.As(err, &input) && input.Status < 500:
			problems["people"] = taskSentence(input.Message)
		case err != nil:
			s.logAutomationError("Slack task form", err)
			problems["text"] = "I couldn't add that task just now; try again in a moment."
		}
	}
	if len(problems) > 0 {
		jsonResponse(w, map[string]any{"response_action": "errors", "errors": problems})
		return
	}
	if _, transport, ok := s.messageClient(settings.Client); ok {
		effect := automation.Effect{Scope: automation.Hash([]string{"task-added", operation}), Action: automation.Hash([]string{"added", operation}), Actor: caller.Person.ID}
		s.sendTaskDM(ctx, settings, transport, "Task confirmation", effect, user, messaging.Reply{Text: taskAddedText(views)})
	}
	jsonResponse(w, map[string]string{"response_action": "clear"})
}

// taskConfirmRequest is what a confirmation form carries back: who asked,
// the request, and how many copies it gives.
type taskConfirmRequest struct {
	User  string    `json:"user"`
	Count int       `json:"count"`
	Input taskInput `json:"input"`
}

// taskConfirmForm asks to confirm a form giving many people a copy. ok is false
// when the request is too long for Slack to carry back.
func taskConfirmForm(c taskConfirmation, user string, in taskInput) (map[string]any, bool) {
	raw, err := json.Marshal(taskConfirmRequest{User: user, Count: c.Count, Input: in})
	if err != nil || len(raw) > 3000 {
		return nil, false
	}
	return map[string]any{
		"type": "modal", "callback_id": taskConfirmView, "private_metadata": string(raw),
		"title":  map[string]string{"type": "plain_text", "text": "Confirm task"},
		"submit": map[string]string{"type": "plain_text", "text": "Give " + strconv.Itoa(c.Count) + " copies"},
		"close":  map[string]string{"type": "plain_text", "text": "Cancel"},
		"blocks": []map[string]any{{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": c.question() + " Go ahead?\n" + taskQuote(in.Text, in.Due, in.Source)}}},
	}, true
}

// slackTaskConfirmForm adds a confirmed form's copies in the background, as
// the member who asked, and confirms them in a direct message when done.
func (s *Server) slackTaskConfirmForm(w http.ResponseWriter, r *http.Request, settings catalog.TaskQueues, user, raw, operation string) {
	var request taskConfirmRequest
	if json.Unmarshal([]byte(raw), &request) != nil {
		http.Error(w, "Invalid interaction", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2500*time.Millisecond)
	defer cancel()
	text := "Adding " + strconv.Itoa(request.Count) + " copies now. I'll message you when they're all added."
	switch {
	case request.User != user:
		text = "Only whoever asked can confirm this."
	case s.verifyTaskMember(ctx, settings, user) != nil:
		text = "Only members of this workspace can give tasks."
	default:
		d := messaging.Address{Client: settings.Client, Workspace: settings.Workspace, Channel: user, User: user, Direct: true}
		if err := s.queueConfirmedTasks(ctx, d, operation, request.Input); err != nil {
			s.logAutomationError("Slack task form", err)
			text = "I couldn't start that just now; open the form and try again."
		}
	}
	jsonResponse(w, map[string]any{"response_action": "update", "view": map[string]any{
		"type": "modal", "title": map[string]string{"type": "plain_text", "text": "New task"},
		"close":  map[string]string{"type": "plain_text", "text": "Close"},
		"blocks": []map[string]any{{"type": "section", "text": map[string]string{"type": "plain_text", "text": text}}},
	}})
}

// taskSentence makes a message a sentence beside a form field.
func taskSentence(message string) string {
	first, size := utf8.DecodeRuneInString(message)
	message = string(unicode.ToUpper(first)) + message[size:]
	if !strings.HasSuffix(message, ".") {
		message += "."
	}
	return message
}

// taskAddedText confirms added tasks to their requester.
func taskAddedText(views []taskView) string {
	view, ids := views[0], []string{}
	reply := ""
	switch {
	case len(views) > taskConfirmAbove:
		reply = "Added a copy for " + strconv.Itoa(len(views)) + " people; each closes their own, and they'll get a message now"
	case len(views) > 1:
		names, you := []string{}, false
		for _, v := range views {
			names, ids, you = append(names, mrkdwnEscape.Replace(taskName(v.Owner))), append(ids, v.ID), you || v.Owner.You
		}
		others := "they'll get a message now"
		if you {
			others = "the others get a message now"
		}
		reply = "Added a copy for " + taskNameList(names) + "; each closes their own, and " + others
	case view.Owner.You:
		reply = "Added to your queue"
	case view.Owner.SlackUser != "":
		reply = "Added to " + mrkdwnEscape.Replace(taskName(view.Owner)) + "'s queue; they'll get a message now"
	default:
		reply = "Added to " + mrkdwnEscape.Replace(taskName(view.Owner)) + "'s queue"
	}
	reply += ":\n" + taskQuote(view.Text, view.Due, view.Source)
	tasksLine := "Task " + view.ID + ". "
	switch {
	case len(views) > taskConfirmAbove:
		tasksLine = ""
	case len(ids) > 1:
		tasksLine = "Tasks " + strings.Join(ids, ", ") + ". "
	}
	return reply + "\n_" + tasksLine + "Send me `tasks` to see your queue and the tasks you gave._"
}
