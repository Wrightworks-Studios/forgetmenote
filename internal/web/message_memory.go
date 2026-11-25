package web

import (
	"context"
	"encoding/json"
	"regexp"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
)

// A direct, unshared messaging conversation, such as a Slack DM, uses the core
// memory of the person it is with: the account it runs as, or, for a workspace
// member served by the members' service account, the account that member
// linked (web/message_link.go). The run reads it as context and may remember
// items, labelled with the client; internal/agentmemory reads and writes them.
// Channels and unlinked members never see personal memory. The person's Memory
// setting governs it, as it does for their connected agents. Memory is the
// person's own notebook, so it is read and written as them, outside the run's
// notebook scope, and the conversation is marked as carrying personal context.

var messageMemoryClient = regexp.MustCompile(`^[a-z]{1,32}$`)

// messageMemoryPerson returns whose memory the conversation uses. linkable
// reports a workspace member who has not linked an account.
func (s *Server) messageMemoryPerson(r *automation.Run) (person string, linkable bool) {
	d := r.Spec.Delivery
	if r.Spec.Kind != "assistant" || d == nil || !d.Direct || d.Shared || !messageMemoryClient.MatchString(d.Client) {
		return "", false
	}
	if s.catalog.Interactive(r.Spec.Actor) {
		return r.Spec.Actor, false
	}
	c, _, ok := s.messageClient(d.Client)
	if !ok || c.Members == nil || c.Members.Actor != r.Spec.Actor {
		return "", false
	}
	person = s.catalog.MessageLink(d.Client, c.Workspace, messageAddress(*d).User)
	if !s.catalog.Interactive(person) {
		return "", true
	}
	return person, false
}

func (s *Server) messageMemoryOwner(r *automation.Run) (string, catalog.AssistantSettings, string, bool) {
	person, _ := s.messageMemoryPerson(r)
	if person == "" {
		return "", catalog.AssistantSettings{}, "", false
	}
	settings, err := s.catalog.Assistant(person)
	return person, settings, r.Spec.Delivery.Client, err == nil && settings.MaintainMemory
}

// messageRemember adds items to the person's core memory, labelled with the
// messaging client. The person's setting is checked again for each call.
func (s *Server) messageRemember(ctx context.Context, r *automation.Run, raw json.RawMessage) (json.RawMessage, error) {
	person, settings, via, ok := s.messageMemoryOwner(r)
	if !ok {
		return nil, workspace.ErrForbidden
	}
	return s.memory.MessageRemember(ctx, r, person, settings, via, raw)
}
