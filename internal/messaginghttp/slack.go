package messaginghttp

import (
	"context"
	"encoding/json"
	"errors"
	"forgetmenote/internal/webcore"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/taskqueue"
	"forgetmenote/internal/workspace"
)

// slackEventUser is an event's user: an ID in message events, and a whole user
// object in member events.
type slackEventUser struct {
	ID  string
	Raw json.RawMessage
}

func (u *slackEventUser) UnmarshalJSON(raw []byte) error {
	u.Raw = slices.Clone(raw)
	if json.Unmarshal(raw, &u.ID) == nil {
		return nil
	}
	var user struct {
		ID string `json:"id"`
	}
	err := json.Unmarshal(raw, &user)
	u.ID = user.ID
	return err
}

// SlackWorkspace reports whether a signed request comes from the workspace
// the enabled Slack client serves, or writes the refusal. Every request it
// admits leads to a reply or an action in Slack, so a paused instance admits
// none.
func (s *Service) SlackWorkspace(w http.ResponseWriter, team string) bool {
	if err := s.outgoing.Check(); err != nil {
		webcore.Fail(w, err)
		return false
	}
	policy, _, enabled := s.MessageClient("slack")
	mounted, ok := s.hooks.SlackPolicy()
	if !enabled || !ok || team != policy.Workspace || team != mounted.Workspace {
		http.Error(w, "Unknown workspace", http.StatusForbidden)
		return false
	}
	return true
}

// SlackEvents serves the Slack app's events: messages and mentions the
// assistant answers, knowledge channel messages and member changes. Web
// verifies the request's signature and timestamp before it calls this with the
// verified body.
func (s *Service) SlackEvents(w http.ResponseWriter, r *http.Request, body []byte) {
	var event struct {
		Type      string `json:"type"`
		Team      string `json:"team_id"`
		ID        string `json:"event_id"`
		Challenge string `json:"challenge"`
		Event     struct {
			Type        string           `json:"type"`
			Subtype     string           `json:"subtype"`
			Channel     string           `json:"channel"`
			ChannelType string           `json:"channel_type"`
			User        slackEventUser   `json:"user"`
			Text        string           `json:"text"`
			TS          string           `json:"ts"`
			Thread      string           `json:"thread_ts"`
			BotID       string           `json:"bot_id"`
			Files       []messaging.File `json:"files"`
		} `json:"event"`
	}
	if json.Unmarshal(body, &event) != nil {
		http.Error(w, "Invalid event", 400)
		return
	}
	if event.Type == "url_verification" {
		webcore.JSON(w, map[string]string{"challenge": event.Challenge})
		return
	}
	if !s.SlackWorkspace(w, event.Team) {
		return
	}
	policy, _, _ := s.MessageClient("slack")
	if event.Type == "event_callback" && (event.Event.Type == "team_join" || event.Event.Type == "user_change") {
		if err := s.hooks.SlackMemberEvent(event.ID, event.Event.User.Raw); err != nil {
			http.Error(w, "Invalid member event", http.StatusBadRequest)
			return
		}
		webcore.JSON(w, map[string]bool{"ok": true})
		return
	}
	direct := event.Event.Type == "message" && event.Event.ChannelType == "im" && strings.HasPrefix(event.Event.Channel, "D")
	if project := policy.KnowledgeChannels[event.Event.Channel]; project != "" && event.Type == "event_callback" && event.Event.Type == "message" && event.Event.Subtype == "" && event.Event.BotID == "" {
		actor := policy.Users[event.Event.User.ID]
		if actor == "" || !s.catalog.Interactive(actor) {
			http.Error(w, "Knowledge capture access unavailable", 403)
			return
		}
		if _, err := s.catalog.Access(actor, project, "read"); err != nil {
			http.Error(w, "Knowledge capture access unavailable", 403)
			return
		}
		if event.ID == "" || len(event.ID) > 128 || len(event.Event.Text) > 16000 {
			http.Error(w, "Invalid knowledge event", 400)
			return
		}
		for _, spec := range s.automations.Definitions.Specs() {
			if spec.Disabled || s.hooks.AutomationPolicy(spec) != nil || spec.Trigger == nil || spec.Trigger.Kind != "slack.knowledge" || spec.Trigger.Channel != event.Event.Channel || spec.Project != project {
				continue
			}
			if _, err := s.automations.Queue.ObserveEvent(spec, automation.Event{ID: event.ID, Actor: actor, Epoch: s.catalog.Epoch(actor), Text: event.Event.Text, At: time.Now()}, time.Now()); err != nil {
				http.Error(w, "Knowledge queue unavailable", 503)
				return
			}
		}
	}
	ignoredSubtype := event.Event.Subtype != "" && !(event.Event.Subtype == "file_share" && len(event.Event.Files) > 0)
	if event.Type != "event_callback" || event.Event.Type != "app_mention" && !direct || event.Event.BotID != "" || ignoredSubtype {
		webcore.JSON(w, map[string]bool{"ok": true})
		return
	}
	if event.ID == "" || len(event.ID) > 128 || event.Event.TS == "" {
		http.Error(w, "Invalid event", http.StatusBadRequest)
		return
	}
	// Preserve deduplication for events queued by the previous Slack-only handler.
	legacyID := automation.Hash([]string{"slack-" + automation.Hash(event.ID)[:32], event.ID})
	if _, err := s.automations.Queue.Get(legacyID); err == nil {
		webcore.JSON(w, map[string]bool{"ok": true})
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		http.Error(w, "Event queue unavailable", http.StatusServiceUnavailable)
		return
	}
	thread := event.Event.Thread
	if thread == "" {
		thread = event.Event.TS
	}
	if direct && event.Event.Thread == "" {
		thread = ""
	}
	address := messaging.Address{Client: "slack", Workspace: event.Team, Channel: event.Event.Channel, Thread: thread, User: event.Event.User.ID, Message: event.Event.TS, Direct: direct, Files: event.Event.Files}
	err := s.EnqueueMessage(r.Context(), messaging.Event{ID: event.ID, Text: event.Event.Text, Address: address})
	// Otherwise a person or conversation without access gets no answer at all.
	// The notice reveals only that the bot is present, which Slack already shows.
	if errors.Is(err, workspace.ErrForbidden) && s.unavailableNotice(r.Context(), address) == nil {
		err = nil
	}
	webcore.Respond(w, map[string]bool{"ok": true}, err)
}

// SlackInteractions accepts a pressed confirmation button as the same request
// as the clicking user replying "confirm CODE" or "cancel CODE" in the thread
// that proposed it. The queued request rechecks everything a typed reply would,
// including that the clicking user asked for the change. Web verifies the
// request's signature and timestamp before it calls this with the verified
// body.
func (s *Service) SlackInteractions(w http.ResponseWriter, r *http.Request, body []byte) {
	form, err := url.ParseQuery(string(body))
	var click struct {
		Type      string              `json:"type"`
		Team      struct{ ID string } `json:"team"`
		User      struct{ ID string } `json:"user"`
		Channel   struct{ ID string } `json:"channel"`
		Container struct {
			MessageTS string `json:"message_ts"`
		} `json:"container"`
		ResponseURL string `json:"response_url"`
		TriggerID   string `json:"trigger_id"`
		CallbackID  string `json:"callback_id"`
		Message     struct {
			TS   string `json:"ts"`
			Text string `json:"text"`
		} `json:"message"`
		Actions []struct {
			ActionID string `json:"action_id"`
			Value    string `json:"value"`
		} `json:"actions"`
		View struct {
			ID              string `json:"id"`
			CallbackID      string `json:"callback_id"`
			PrivateMetadata string `json:"private_metadata"`
			State           struct {
				Values map[string]map[string]struct {
					Value                 string   `json:"value"`
					SelectedUsers         []string `json:"selected_users"`
					SelectedConversations []string `json:"selected_conversations"`
					SelectedDate          string   `json:"selected_date"`
					SelectedOption        struct {
						Value string `json:"value"`
					} `json:"selected_option"`
					SelectedOptions []struct {
						Value string `json:"value"`
					} `json:"selected_options"`
				} `json:"values"`
			} `json:"state"`
		} `json:"view"`
	}
	if err != nil || json.Unmarshal([]byte(form.Get("payload")), &click) != nil {
		http.Error(w, "Invalid interaction", 400)
		return
	}
	if !s.SlackWorkspace(w, click.Team.ID) {
		return
	}
	team, user := click.Team.ID, click.User.ID
	// A task interaction comes from a member of the workspace task queues
	// serve; taskSettings returns their settings, or writes the refusal.
	taskSettings := func() (catalog.TaskQueues, bool) {
		settings, on := s.tasks.SlackQueues(team)
		switch {
		case !messaging.ValidID(user):
			http.Error(w, "Invalid interaction", http.StatusBadRequest)
		case !on:
			http.Error(w, "Unknown workspace", http.StatusForbidden)
		default:
			return settings, true
		}
		return settings, false
	}
	switch {
	case click.Type == "view_submission" && click.View.CallbackID == taskqueue.NotApplicableView:
		if settings, ok := taskSettings(); ok {
			// Submitting the same modal again is the same request.
			press := "slack-view-" + automation.Hash([]string{click.View.ID, click.View.PrivateMetadata, user})[:32]
			s.tasks.SlackReason(w, r, settings, user, click.View.PrivateMetadata, click.View.State.Values["reason"]["reason"].Value, press)
		}
		return
	case click.Type == "view_submission" && click.View.CallbackID == taskqueue.AddView:
		if settings, ok := taskSettings(); ok {
			values := click.View.State.Values
			form := taskqueue.Input{People: values["people"]["people"].SelectedUsers, Text: values["text"]["text"].Value, Due: values["due"]["due"].SelectedDate, Priority: values["priority"]["priority"].SelectedOption.Value, Source: values["link"]["link"].Value}
			everyone := len(values["everyone"]["everyone"].SelectedOptions) == 1 && values["everyone"]["everyone"].SelectedOptions[0].Value == "everyone"
			// Submitting the same form again is the same request.
			s.tasks.SlackAdd(w, r, settings, user, form, values["channels"]["channels"].SelectedConversations, everyone, "slack-view-"+automation.Hash([]string{click.View.ID, user})[:32])
		}
		return
	case click.Type == "view_submission" && click.View.CallbackID == taskqueue.ConfirmView:
		if settings, ok := taskSettings(); ok {
			// The form keeps its view ID when it asks to confirm, so the request
			// keeps the operation it had.
			s.tasks.SlackConfirmForm(w, r, settings, user, click.View.PrivateMetadata, "slack-view-"+automation.Hash([]string{click.View.ID, user})[:32])
		}
		return
	case click.Type == "message_action" && click.CallbackID == taskqueue.Shortcut:
		s.tasks.SlackShortcut(w, r, team, user, click.Channel.ID, click.Message.TS, click.Message.Text, click.TriggerID, click.ResponseURL)
		return
	case click.Type != "block_actions":
		// Slack reads nothing from the acknowledgement of a button press.
		w.WriteHeader(http.StatusOK)
		return
	}
	if len(click.Actions) == 1 {
		action, raw := click.Actions[0].ActionID, click.Actions[0].Value
		switch action {
		case taskqueue.DoneAction, taskqueue.SnoozeAction:
			if settings, ok := taskSettings(); ok {
				// Pressing the same button again is the same request.
				press := "slack-button-" + automation.Hash([]string{click.Channel.ID, click.Container.MessageTS, action, raw, user})[:32]
				s.tasks.SlackButton(w, r, settings, user, action, raw, click.ResponseURL, press)
			}
			return
		case taskqueue.ConfirmAction, taskqueue.CancelAction:
			if settings, ok := taskSettings(); ok {
				s.tasks.SlackConfirm(w, r, settings, user, action, raw, click.ResponseURL)
			}
			return
		case taskqueue.NotApplicableAction:
			s.tasks.SlackReasonModal(w, r, raw, click.TriggerID)
			return
		case taskqueue.ReplyDismissAction:
			if settings, ok := taskSettings(); ok {
				s.tasks.SlackReplyDismiss(w, r, settings, user, raw, click.ResponseURL)
			}
			return
		}
	}
	var value messaging.ButtonValue
	if len(click.Actions) != 1 || json.Unmarshal([]byte(click.Actions[0].Value), &value) != nil || !writeCode.MatchString(value.Code) || len(value.Thread) > 128 || !messaging.ValidID(click.Channel.ID) || !messaging.ValidID(click.User.ID) || len(click.Container.MessageTS) > 128 {
		http.Error(w, "Invalid interaction", 400)
		return
	}
	command, done := "confirm", "Confirmation received; the result follows here."
	switch click.Actions[0].ActionID {
	case messaging.ConfirmAction:
	case messaging.CancelAction:
		command, done = "cancel", "Cancellation received; the result follows here."
	default:
		http.Error(w, "Invalid interaction", 400)
		return
	}
	address := messaging.Address{Client: "slack", Workspace: click.Team.ID, Channel: click.Channel.ID, Thread: value.Thread, User: click.User.ID, Direct: strings.HasPrefix(click.Channel.ID, "D")}
	// Pressing the same button again is the same request.
	id := "slack-button-" + automation.Hash([]string{click.Channel.ID, click.Container.MessageTS, click.Actions[0].ActionID, click.User.ID})[:32]
	if err := s.EnqueueMessage(r.Context(), messaging.Event{ID: id, Text: command + " " + value.Code, Address: address}); err != nil {
		webcore.Fail(w, err)
		return
	}
	// The buttons are gone once pressed; a failure here only leaves them shown.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.hooks.SlackRespond(ctx, click.ResponseURL, done); err != nil {
		s.automations.Report("Slack button response", err)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Service) unavailableNotice(ctx context.Context, d messaging.Address) error {
	_, transport, ok := s.MessageClient("slack")
	if !ok || !messaging.ValidID(d.User) || !messaging.ValidID(d.Channel) {
		return workspace.ErrForbidden
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	d.Files = nil
	return transport.Send(ctx, d, messaging.Reply{Text: "I'm not set up to answer you here yet. Ask an administrator to enable this conversation or link your account."})
}

// DeliverSlack sends an automation's result to a Slack destination, checking a
// shared channel's approved audience first.
func (s *Service) DeliverSlack(ctx context.Context, r *automation.Run, save func() error, d automation.Delivery) error {
	policy, transport, ok := s.MessageClient("slack")
	if !ok {
		return workspace.ErrForbidden
	}
	if d.Shared {
		if err := SharedAudience(policy, d.Channel, r); err != nil {
			return err
		}
	}
	return s.sendMessage(ctx, transport, MessageAddress(d), messaging.Reply{Text: r.Answer, Artifacts: r.Artifacts}, r, save)
}

// SharedAudience checks that a channel's approved broadcast may receive
// everything the run can read or has read: its notebooks, connections,
// receipts and citations. A job selects the channel; only the broadcast
// approves who sees the result.
func SharedAudience(policy messaging.Client, channel string, r *automation.Run) error {
	b, ok := policy.Broadcasts[channel]
	if !ok || !b.AudienceApproved || b.Project != r.Spec.Project || !slices.Contains(b.Actors, r.Spec.Actor) {
		return workspace.ErrForbidden
	}
	for _, connection := range r.Spec.Connections {
		if !slices.Contains(b.Connections, connection) {
			return workspace.ErrForbidden
		}
	}
	for _, project := range r.Spec.ReadProjects {
		if project != b.Project {
			return workspace.ErrForbidden
		}
	}
	for _, receipt := range r.Receipts {
		if !slices.Contains(b.Connections, receipt.Connection) {
			return workspace.ErrForbidden
		}
	}
	for _, citation := range r.Citations {
		if citation.Kind == "data" && slices.Contains(b.Connections, citation.Source) {
			continue
		}
		if citation.Project != b.Project {
			return workspace.ErrForbidden
		}
	}
	return nil
}
