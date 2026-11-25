package web

import (
	"context"
	"errors"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"sort"
)

func (s *Server) chatClientStatus(actor string) (map[string]any, error) {
	if !s.catalog.IsAdmin(actor) {
		return nil, workspace.ErrForbidden
	}
	config := s.catalog.ChatClients()
	if config.Revision == 0 && s.slack != nil {
		config.Clients["slack"] = s.slack.policy()
	}
	available := []string{}
	for id := range s.messageClients {
		available = append(available, id)
	}
	sort.Strings(available)
	return map[string]any{"config": config, "available": available, "eventURL": s.origin + "/integrations/slack/events", "interactionURL": s.origin + "/integrations/slack/interactions", "commandURL": s.origin + "/integrations/slack/commands"}, nil
}
func (s *Server) messagingTools(server *mcp.Server) {
	addManagementTool(s, server, "chat_clients", "Inspect external chat client policy and configured transports. Admin only; credentials are never returned.", true, func(ctx context.Context, _ struct{}) (map[string]any, error) { return s.chatClientStatus(subject(ctx)) })
	addManagementTool(s, server, "set_chat_clients", "Configure linked accounts, conversation scopes, files and shared audiences for external chat clients. Admin only; requires the current revision.", false, func(ctx context.Context, in struct {
		Config catalog.ChatClients `json:"config"`
	}) (map[string]any, error) {
		if !s.catalog.IsAdmin(subject(ctx)) {
			return nil, workspace.ErrForbidden
		}
		for id, c := range in.Config.Clients {
			if s.messageClients[id] == nil {
				return nil, errors.New("mount the transport credentials before configuring this client")
			}
			if id == "slack" && c.Workspace != s.slack.config.Team {
				return nil, errors.New("workspace must match the mounted Slack app")
			}
			if id == "slack" && (len(c.Channels)+len(c.DirectMessages)+len(c.KnowledgeChannels) > 0 || c.Members != nil) && s.slack.config.SigningSecretFile == "" {
				return nil, errors.New("mount the Slack signing secret before enabling incoming messages")
			}
			if c.Disabled {
				continue
			}
			for _, actor := range c.Users {
				if !s.catalog.Interactive(actor) {
					return nil, errors.New("link an active ForgetMeNote account")
				}
			}
			bindings := []map[string]messaging.Binding{c.Channels, c.DirectMessages}
			if c.Members != nil {
				if _, ok := s.messageClients[id].(messaging.MemberVerifier); !ok || !s.catalog.IsService(c.Members.Actor) {
					return nil, errors.New("workspace access requires a service account and a transport that verifies membership")
				}
				for _, project := range append([]string{c.Members.Binding.Project}, c.Members.Binding.ReadProjects...) {
					if _, err := s.catalog.Access(c.Members.Actor, project, "read"); err != nil {
						return nil, err
					}
				}
				bindings = append(bindings, map[string]messaging.Binding{"members": c.Members.Binding})
			}
			for _, bindings := range bindings {
				for _, b := range bindings {
					if _, _, _, err := s.automationProvider(automation.Spec{AIConnection: b.AIConnection, Model: b.Model, ExternalProcessing: b.ExternalProcessing}); err != nil {
						return nil, err
					}
					for _, project := range append([]string{b.Project}, b.ReadProjects...) {
						if _, err := s.catalog.Access(subject(ctx), project, "read"); err != nil {
							return nil, err
						}
					}
				}
			}
		}
		if _, err := s.catalog.SetChatClients(subject(ctx), in.Config); err != nil {
			return nil, err
		}
		return s.chatClientStatus(subject(ctx))
	})
}
