package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"forgetmenote/internal/durable"
	"forgetmenote/internal/jsonfile"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/capacity"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/integrations"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/outgoing"
	"forgetmenote/internal/workspace"
)

type SlackChannel = messaging.Binding
type SlackConfig = messaging.SlackConfig

// An operator explicitly approves the audience for each shared destination.
// Jobs can select these destinations, but cannot broaden their data scope.
type SlackBroadcast = messaging.Broadcast
type slackService struct {
	config   SlackConfig
	client   *http.Client
	endpoint string
	outgoing *outgoing.Mode
}

func (s *Server) ConfigureSlack(file string) error {
	if err := s.privateRuntimeFile(file); err != nil {
		return err
	}
	var cfg SlackConfig
	if err := jsonfile.Load(file, 256<<10, &cfg); err != nil {
		return errors.New("invalid Slack configuration")
	}
	return s.ConfigureSlackConfig(cfg)
}
func (s *Server) ConfigureSlackConfig(cfg messaging.SlackConfig) error {
	if err := integrations.Check("slack"); err != nil {
		return err
	}
	unlock, err := s.startupConfiguration()
	if err != nil {
		return err
	}
	defer unlock()

	var e error
	if cfg.Team == "" || len(cfg.Users) > 1000 || len(cfg.Channels)+len(cfg.Broadcasts)+len(cfg.KnowledgeChannels) > 128 || len(cfg.DirectMessages) > 1000 {
		return errors.New("invalid Slack configuration")
	}
	files := []string{cfg.BotTokenFile}
	if cfg.SigningSecretFile != "" || cfg.Members != nil || len(cfg.Channels)+len(cfg.DirectMessages)+len(cfg.KnowledgeChannels) > 0 {
		files = append(files, cfg.SigningSecretFile)
	}
	for _, p := range files {
		if e = s.privateRuntimeFile(p); e != nil {
			return e
		}
	}
	for id, b := range cfg.Broadcasts {
		if !strings.HasPrefix(id, "C") || strings.ContainsAny(id, " \r\n/") || !b.AudienceApproved || b.Project == "" || len(b.Actors) == 0 || len(b.Actors) > 128 {
			return errors.New("shared Slack destinations require an approved audience, notebook and allowed actors")
		}
	}
	for id, project := range cfg.KnowledgeChannels {
		if !strings.HasPrefix(id, "C") || project == "" || len(cfg.Users) == 0 {
			return errors.New("knowledge channels require a notebook and mapped authors")
		}
	}
	if e = (&slackService{config: cfg}).policy().Validate(); e != nil {
		return e
	}
	if e = s.initAutomations(); e != nil {
		return e
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	s.slack = &slackService{config: cfg, outgoing: s.outgoing, endpoint: "https://slack.com/api/chat.postEphemeral", client: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if s.messageClients == nil {
		s.messageClients = map[string]messaging.Transport{}
	}
	s.messageClients["slack"] = s.slack
	return nil
}
func (s *slackService) policy() messaging.Client {
	return messaging.Client{Members: s.config.Members, Workspace: s.config.Team, Users: s.config.Users, Channels: s.config.Channels, DirectMessages: s.config.DirectMessages, KnowledgeChannels: s.config.KnowledgeChannels, Broadcasts: s.config.Broadcasts}
}
func runtimeSecret(file string) (string, error) {
	value, err := durable.ReadSecret(file, 64<<10)
	if errors.Is(err, durable.ErrOpen) {
		return "", errors.New("runtime credential unavailable")
	}
	if err != nil {
		return "", errors.New("invalid runtime credential")
	}
	return value, nil
}

// slackRequest returns the body of a request Slack signed within the last five
// minutes, or writes the refusal.
func (s *Server) slackRequest(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if s.slack == nil || s.slack.config.SigningSecretFile == "" {
		http.NotFound(w, r)
		return nil, false
	}
	if r.Method != "POST" {
		http.Error(w, "POST required", 405)
		return nil, false
	}
	body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(capacity.SourceBytes.Limit())))
	if e != nil {
		http.Error(w, "Event too large", 413)
		return nil, false
	}
	stamp := r.Header.Get("X-Slack-Request-Timestamp")
	seconds, e := strconv.ParseInt(stamp, 10, 64)
	if e != nil || time.Since(time.Unix(seconds, 0)) > 5*time.Minute || time.Until(time.Unix(seconds, 0)) > 5*time.Minute {
		http.Error(w, "Invalid signature", 401)
		return nil, false
	}
	secret, e := runtimeSecret(s.slack.config.SigningSecretFile)
	if e != nil {
		http.Error(w, "Slack unavailable", 503)
		return nil, false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + stamp + ":"))
	mac.Write(body)
	if !hmac.Equal([]byte("v0="+hex.EncodeToString(mac.Sum(nil))), []byte(r.Header.Get("X-Slack-Signature"))) {
		http.Error(w, "Invalid signature", 401)
		return nil, false
	}
	return body, true
}

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

// slackWorkspace reports whether a signed request comes from the workspace
// the enabled Slack client serves, or writes the refusal. Every request it
// admits leads to a reply or an action in Slack, so a paused instance admits
// none.
func (s *Server) slackWorkspace(w http.ResponseWriter, team string) bool {
	if err := s.outgoing.Check(); err != nil {
		fail(w, err)
		return false
	}
	policy, _, enabled := s.messageClient("slack")
	if !enabled || team != policy.Workspace || team != s.slack.config.Team {
		http.Error(w, "Unknown workspace", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) slackEvents(w http.ResponseWriter, r *http.Request) {
	body, ok := s.slackRequest(w, r)
	if !ok {
		return
	}
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
		jsonResponse(w, map[string]string{"challenge": event.Challenge})
		return
	}
	if !s.slackWorkspace(w, event.Team) {
		return
	}
	policy, _, _ := s.messageClient("slack")
	if event.Type == "event_callback" && (event.Event.Type == "team_join" || event.Event.Type == "user_change") {
		member, active, err := s.slack.EventMember(event.Event.User.Raw)
		if err != nil {
			http.Error(w, "Invalid member event", http.StatusBadRequest)
			return
		}
		s.observeMemberChange("slack", event.ID, member, active)
		jsonResponse(w, map[string]bool{"ok": true})
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
			if spec.Disabled || s.automationPolicy(spec) != nil || spec.Trigger == nil || spec.Trigger.Kind != "slack.knowledge" || spec.Trigger.Channel != event.Event.Channel || spec.Project != project {
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
		jsonResponse(w, map[string]bool{"ok": true})
		return
	}
	if event.ID == "" || len(event.ID) > 128 || event.Event.TS == "" {
		http.Error(w, "Invalid event", http.StatusBadRequest)
		return
	}
	// Preserve deduplication for events queued by the previous Slack-only handler.
	legacyID := automation.Hash([]string{"slack-" + automation.Hash(event.ID)[:32], event.ID})
	if _, err := s.automations.Queue.Get(legacyID); err == nil {
		jsonResponse(w, map[string]bool{"ok": true})
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
	err := s.enqueueMessage(r.Context(), messaging.Event{ID: event.ID, Text: event.Event.Text, Address: address})
	// Otherwise a person or conversation without access gets no answer at all.
	// The notice reveals only that the bot is present, which Slack already shows.
	if errors.Is(err, workspace.ErrForbidden) && s.unavailableNotice(r.Context(), address) == nil {
		err = nil
	}
	respond(w, map[string]bool{"ok": true}, err)
}

// slackInteractions accepts a pressed confirmation button as the same request
// as the clicking user replying "confirm CODE" or "cancel CODE" in the thread
// that proposed it. The queued request rechecks everything a typed reply would,
// including that the clicking user asked for the change.
func (s *Server) slackInteractions(w http.ResponseWriter, r *http.Request) {
	body, ok := s.slackRequest(w, r)
	if !ok {
		return
	}
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
	if !s.slackWorkspace(w, click.Team.ID) {
		return
	}
	team, user := click.Team.ID, click.User.ID
	// A task interaction comes from a member of the workspace task queues
	// serve; taskSettings returns their settings, or writes the refusal.
	taskSettings := func() (catalog.TaskQueues, bool) {
		settings, on := s.slackTaskQueues(team)
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
	case click.Type == "view_submission" && click.View.CallbackID == taskNotApplicableView:
		if settings, ok := taskSettings(); ok {
			// Submitting the same modal again is the same request.
			press := "slack-view-" + automation.Hash([]string{click.View.ID, click.View.PrivateMetadata, user})[:32]
			s.slackTaskReason(w, r, settings, user, click.View.PrivateMetadata, click.View.State.Values["reason"]["reason"].Value, press)
		}
		return
	case click.Type == "view_submission" && click.View.CallbackID == taskAddView:
		if settings, ok := taskSettings(); ok {
			values := click.View.State.Values
			form := taskInput{People: values["people"]["people"].SelectedUsers, Text: values["text"]["text"].Value, Due: values["due"]["due"].SelectedDate, Priority: values["priority"]["priority"].SelectedOption.Value, Source: values["link"]["link"].Value}
			everyone := len(values["everyone"]["everyone"].SelectedOptions) == 1 && values["everyone"]["everyone"].SelectedOptions[0].Value == "everyone"
			// Submitting the same form again is the same request.
			s.slackTaskAdd(w, r, settings, user, form, values["channels"]["channels"].SelectedConversations, everyone, "slack-view-"+automation.Hash([]string{click.View.ID, user})[:32])
		}
		return
	case click.Type == "view_submission" && click.View.CallbackID == taskConfirmView:
		if settings, ok := taskSettings(); ok {
			// The form keeps its view ID when it asks to confirm, so the request
			// keeps the operation it had.
			s.slackTaskConfirmForm(w, r, settings, user, click.View.PrivateMetadata, "slack-view-"+automation.Hash([]string{click.View.ID, user})[:32])
		}
		return
	case click.Type == "message_action" && click.CallbackID == taskShortcut:
		s.slackTaskShortcut(w, r, team, user, click.Channel.ID, click.Message.TS, click.Message.Text, click.TriggerID, click.ResponseURL)
		return
	case click.Type != "block_actions":
		// Slack reads nothing from the acknowledgement of a button press.
		w.WriteHeader(http.StatusOK)
		return
	}
	if len(click.Actions) == 1 {
		action, raw := click.Actions[0].ActionID, click.Actions[0].Value
		switch action {
		case taskDoneAction, taskSnoozeAction:
			if settings, ok := taskSettings(); ok {
				// Pressing the same button again is the same request.
				press := "slack-button-" + automation.Hash([]string{click.Channel.ID, click.Container.MessageTS, action, raw, user})[:32]
				s.slackTaskButton(w, r, settings, user, action, raw, click.ResponseURL, press)
			}
			return
		case taskConfirmAction, taskCancelAction:
			if settings, ok := taskSettings(); ok {
				s.slackTaskConfirm(w, r, settings, user, action, raw, click.ResponseURL)
			}
			return
		case taskNotApplicableAction:
			s.slackTaskReasonModal(w, r, raw, click.TriggerID)
			return
		case taskReplyDismissAction:
			if settings, ok := taskSettings(); ok {
				s.slackReplyDismiss(w, r, settings, user, raw, click.ResponseURL)
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
	if err := s.enqueueMessage(r.Context(), messaging.Event{ID: id, Text: command + " " + value.Code, Address: address}); err != nil {
		fail(w, err)
		return
	}
	// The buttons are gone once pressed; a failure here only leaves them shown.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.slack.Respond(ctx, click.ResponseURL, done); err != nil {
		s.logAutomationError("Slack button response", err)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) unavailableNotice(ctx context.Context, d messaging.Address) error {
	_, transport, ok := s.messageClient("slack")
	if !ok || !messaging.ValidID(d.User) || !messaging.ValidID(d.Channel) {
		return workspace.ErrForbidden
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	d.Files = nil
	return transport.Send(ctx, d, messaging.Reply{Text: "I'm not set up to answer you here yet. Ask an administrator to enable this conversation or link your account."})
}
func (s *Server) deliverSlack(ctx context.Context, r *automation.Run, save func() error, d automation.Delivery) error {
	policy, transport, ok := s.messageClient("slack")
	if !ok {
		return workspace.ErrForbidden
	}
	if d.Shared {
		if err := sharedAudience(policy, d.Channel, r); err != nil {
			return err
		}
	}
	return s.sendMessage(ctx, transport, messageAddress(d), messaging.Reply{Text: r.Answer, Artifacts: r.Artifacts}, r, save)
}

// sharedAudience checks that a channel's approved broadcast may receive
// everything the run can read or has read: its notebooks, connections,
// receipts and citations. A job selects the channel; only the broadcast
// approves who sees the result.
func sharedAudience(policy messaging.Client, channel string, r *automation.Run) error {
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
