package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/tasks"
)

// Task buttons in direct messages. A press is checked again against the task
// as it is then: only the queue's owner can close it.
const (
	taskDoneAction = "task_done"
)

type taskButtonValue struct {
	Task     string `json:"task"`
	Revision string `json:"revision"`
	// Until is the date a Snooze button snoozes the task until.
	Until string `json:"until,omitempty"`
}

// taskNotice is one change that people other than its actor hear about.
type taskNotice struct {
	Event      string // added, done, not_applicable, withdrawn, reassigned or moved earlier
	Task       taskView
	Actor      string // person ID
	Previous   string // the owner before a reassignment
	Reason     string
	Occurrence *tasks.Closing
}

// noticeTask sends bot direct messages about a task change: a new task to its
// owner, a closed task to its requester. People without a workspace member
// hear nothing. Each message is sent at most once, and a failure never undoes
// the change, which Git already records.
func (s *Server) noticeTask(ctx context.Context, n taskNotice) {
	settings, _, err := s.taskQueues()
	if err != nil || settings.Client == "" {
		return
	}
	_, transport, ok := s.messageClient(settings.Client)
	if !ok {
		return
	}
	owner, requester := n.Task.Owner, n.Task.Requester
	who := func(p taskPersonView) string {
		switch {
		case p.Name != "":
			return mrkdwnEscape.Replace(p.Name)
		case p.SlackUser != "":
			return "a colleague (Slack member " + p.SlackUser + ")"
		}
		return "a colleague"
	}
	task := mrkdwnEscape.Replace(n.Task.Text)
	line := "> " + task
	if n.Task.Due != "" {
		line += "\n> Due " + n.Task.Due
	}
	if n.Task.Every != nil {
		line += ", repeating every " + strings.TrimSuffix(n.Task.Every.String(), " from "+n.Task.Every.Start)
	}
	if link := taskSourceLink(n.Task.Source); link != "" {
		line += "\n> " + link
	}
	reason := ""
	if n.Reason != "" {
		reason = "\nReason: " + mrkdwnEscape.Replace(n.Reason)
	}
	type message struct {
		to      taskPersonView
		text    string
		buttons []messaging.Button
	}
	done := func() []messaging.Button {
		value, _ := json.Marshal(taskButtonValue{Task: n.Task.ID, Revision: n.Task.Revision})
		return []messaging.Button{{Action: taskDoneAction, Label: "Done", Value: string(value), Style: "primary"}}
	}
	var out []message
	switch n.Event {
	case "added":
		out = append(out, message{owner, "New task from " + who(requester) + ":\n" + line, done()})
	case "done", "not_applicable":
		outcome := "done"
		if n.Event == "not_applicable" {
			outcome = "not applicable"
		}
		text := who(owner) + " marked your task " + outcome + ":\n" + line + reason
		if n.Occurrence != nil {
			text = who(owner) + " marked your recurring task " + outcome + " for " + n.Occurrence.Closed + "; it is next due " + n.Occurrence.Next + ":\n> " + task + reason
		}
		out = append(out, message{requester, text, nil})
	case "withdrawn":
		out = append(out, message{owner, who(requester) + " withdrew a task they gave you:\n" + line + reason, nil})
	case "reassigned":
		out = append(out, message{owner, "New task from " + who(requester) + ", passed to you by " + who(s.taskPersonView(n.Actor, taskCaller{})) + ":\n" + line + reason, done()})
		out = append(out, message{requester, who(s.taskPersonView(n.Actor, taskCaller{})) + " passed your task to " + who(owner) + ":\n" + line + reason, nil})
	case "moved earlier":
		out = append(out, message{owner, who(requester) + " moved a task's due date earlier:\n" + line, nil})
	}
	for _, m := range out {
		if m.to.ID == n.Actor || m.to.SlackUser == "" {
			continue
		}
		text := m.text + "\n\n_Task " + n.Task.ID + ". Reply `tasks` to see your queue._"
		action := automation.Hash([]string{n.Event, n.Task.ID, n.Task.Revision, m.to.ID, fmt.Sprint(n.Occurrence)})
		// Each message is its own scope, so one whose delivery is unknown never
		// holds back later messages about the task.
		effect := automation.Effect{Scope: automation.Hash([]string{"task-notice", n.Task.ID, action}), Action: action, Actor: m.to.ID}
		s.sendTaskDM(ctx, settings, transport, "Task notice", effect, m.to.SlackUser, messaging.Reply{Text: text, Buttons: m.buttons})
	}
}

// sendTaskDM sends a member a direct message at most once: effect names it. A
// failure is logged and never undoes the change the message reports.
func (s *Server) sendTaskDM(ctx context.Context, settings catalog.TaskQueues, transport messaging.Transport, label string, effect automation.Effect, member string, reply messaging.Reply) {
	address := messaging.Address{Client: settings.Client, Workspace: settings.Workspace, User: member, Channel: member, Direct: true}
	if _, err := s.sendOnce(ctx, effect, transport, address, reply, dmSendTimeout, nil); err != nil {
		s.logAutomationError(label, err)
	}
}
