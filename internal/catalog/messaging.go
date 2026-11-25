package catalog

import (
	"errors"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/workspace"
	"maps"
	"slices"
)

type ChatClients struct {
	Revision uint64                      `json:"revision"`
	Clients  map[string]messaging.Client `json:"clients"`
}

func cloneChatClients(in ChatClients) ChatClients {
	in.Clients = maps.Clone(in.Clients)
	for id, client := range in.Clients {
		client.Users = maps.Clone(client.Users)
		client.KnowledgeChannels = maps.Clone(client.KnowledgeChannels)
		client.Channels = cloneBindings(client.Channels)
		client.DirectMessages = cloneBindings(client.DirectMessages)
		client.Broadcasts = maps.Clone(client.Broadcasts)
		for key, broadcast := range client.Broadcasts {
			broadcast.Actors = slices.Clone(broadcast.Actors)
			broadcast.Connections = slices.Clone(broadcast.Connections)
			client.Broadcasts[key] = broadcast
		}
		if client.Members != nil {
			members := *client.Members
			members.Binding = cloneBinding(members.Binding)
			members.PairByEmail = slices.Clone(members.PairByEmail)
			client.Members = &members
		}
		in.Clients[id] = client
	}
	return in
}
func (in ChatClients) validate() error {
	if len(in.Clients) > 16 {
		return errors.New("too many chat clients")
	}
	for id, c := range in.Clients {
		if !messaging.ValidID(id) {
			return errors.New("invalid chat client ID")
		}
		if err := c.Validate(); err != nil {
			return err
		}
	}
	return nil
}
func (c *Catalog) ChatClients() ChatClients {
	c.lock()
	defer c.mu.Unlock()
	if c.state.ChatClients == nil {
		return ChatClients{Clients: map[string]messaging.Client{}}
	}
	return cloneChatClients(*c.state.ChatClients)
}
func (c *Catalog) SetChatClients(actor string, in ChatClients) (ChatClients, error) {
	c.lock()
	defer c.mu.Unlock()
	if !c.admin(actor) {
		return ChatClients{}, workspace.ErrForbidden
	}
	var revision uint64
	if c.state.ChatClients != nil {
		revision = c.state.ChatClients.Revision
	}
	if in.Revision != revision {
		return ChatClients{}, workspace.ErrConflict
	}
	if err := in.validate(); err != nil {
		return ChatClients{}, err
	}
	in = cloneChatClients(in)
	in.Revision++
	err := c.change(actor, "chat.clients", "instance", nil, func() error {
		c.state.ChatClients = &in
		return nil
	})
	return cloneChatClients(in), err
}

func cloneBinding(in messaging.Binding) messaging.Binding {
	in.ReadProjects = slices.Clone(in.ReadProjects)
	in.Connections = slices.Clone(in.Connections)
	in.Actors = slices.Clone(in.Actors)
	return in
}
func cloneBindings(in map[string]messaging.Binding) map[string]messaging.Binding {
	out := maps.Clone(in)
	for key, binding := range out {
		out[key] = cloneBinding(binding)
	}
	return out
}
