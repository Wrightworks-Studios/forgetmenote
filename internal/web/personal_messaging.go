package web

import (
	"maps"
	"slices"

	"forgetmenote/internal/messaging"
)

func (s *Server) personalMessagePolicy(c messaging.Client, d messaging.Address) messaging.Client {
	if d.Client != "slack" || !d.Direct || d.Shared || s.personalConnections == nil || c.Disabled || d.Workspace != c.Workspace || !s.modules.Enabled("personal") {
		return c
	}
	account := s.personalConnections.LinkedOwner("slack", c.Workspace, d.User)
	user, ok := s.catalog.AssistantSubject(account)
	if !ok {
		return c
	}
	settings, err := s.catalog.Assistant(user)
	if err != nil || !slices.Contains(settings.Features, "slack") {
		return c
	}
	spec, err := s.personalSpec(user, settings)
	if err != nil {
		return c
	}
	c.Users = maps.Clone(c.Users)
	if c.Users == nil {
		c.Users = map[string]string{}
	}
	c.DirectMessages = maps.Clone(c.DirectMessages)
	if c.DirectMessages == nil {
		c.DirectMessages = map[string]messaging.Binding{}
	}
	c.Users[d.User] = user
	c.DirectMessages[d.User] = messaging.Binding{Project: settings.Project, ReadProjects: spec.ReadProjects, Connections: spec.Connections, AIConnection: spec.AIConnection, Model: spec.Model, ExternalProcessing: true, ThreadHistory: true, Attachments: slices.Contains(settings.Features, "documents")}
	return c
}
