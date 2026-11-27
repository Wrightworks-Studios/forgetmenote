package web

import (
	"context"
	"encoding/json"

	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/messaginghttp"
	"forgetmenote/internal/personal"
)

// initMessaging binds messaging to this instance's automation, chat, personal
// assistant and data checks, its account links and email pairing, its
// transports and its Slack app. It runs after
// automations, memory and task queues, which messaging uses directly. The
// Slack app, the transports, the chat store, data and personal connections and
// the origin are read at each call, since web replaces them after startup.
func (s *Server) initMessaging() error {
	var err error
	s.messaging, err = messaginghttp.New(s.catalog, s.automations, s.tasks, s.memory, s.outgoing, messaginghttp.Hooks{
		AutomationPolicy:        s.automationPolicy,
		AutomationProvider:      s.automationrun.Provider,
		CheckAutomationEvidence: s.checkAutomationEvidence,
		AnswerOnce:              s.automationrun.AnswerOnce,
		ChatStore:               func() *chat.Store { return s.chat.Store },
		ValidateChatSources:     s.chatrun.ValidateSources,
		AssistantInstructions:   s.chatrun.AssistantInstructions,
		PersonalMessagePolicy:   s.personalMessagePolicy,
		LinkedOwner: func(client, workspace, user string) string {
			if s.personalConnections == nil {
				return ""
			}
			return s.personalConnections.LinkedOwner(client, workspace, user)
		},
		RecordPersonalInteraction: func(ctx context.Context, user string, in messaginghttp.Interaction) error {
			return s.recordPersonalInteraction(ctx, user, personal.Interaction{ID: in.ID, UserText: in.UserText, Response: in.Response, Source: in.Source})
		},
		LinkReply: s.linkCommandReply,
		SlackMemberEvent: func(eventID string, raw json.RawMessage) error {
			member, active, err := s.slack.EventMember(raw)
			if err != nil {
				return err
			}
			s.observeMemberChange("slack", eventID, member, active)
			return nil
		},
		DataConnections: func() *connectors.Manager { return s.dataConnections },
		DataAllowed:     s.dataAllowed,
		DataExecute:     s.dataExecute,
		ActionArguments: func(project, name string, raw json.RawMessage) (json.RawMessage, error) {
			_, decoded, err := s.actionArguments(project, name, raw)
			return decoded, err
		},
		Origin:    func() string { return s.origin },
		Transport: func(id string) messaging.Transport { return s.messageClients[id] },
		SlackPolicy: func() (messaging.Client, bool) {
			if s.slack == nil {
				return messaging.Client{}, false
			}
			return s.slack.policy(), true
		},
		SlackRespond: func(ctx context.Context, responseURL, text string) error {
			return s.slack.Respond(ctx, responseURL, text)
		},
	})
	return err
}
