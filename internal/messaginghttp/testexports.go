package messaginghttp

import (
	"context"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/messaging"
)

// Web's messaging tests use these exports; production code never does.
// TestMessagingTestExportsStayInTests in web fails on any reference to them
// from a non-test file.

// MessageConversation returns the owner and binding key of the conversation a
// message belongs to. Tests only.
func MessageConversation(c messaging.Client, b messaging.Binding, d messaging.Address, actor string) (string, string) {
	return messageConversation(c, b, d, actor)
}

// SendMessage sends a run's reply once, as a run's delivery does. Tests only.
func (s *Service) SendMessage(ctx context.Context, t messaging.Transport, d messaging.Address, reply messaging.Reply, r *automation.Run, save func() error) error {
	return s.sendMessage(ctx, t, d, reply, r, save)
}

// SendAttempts is how many times a message Slack refuses is tried. Tests only.
const SendAttempts = sendAttempts
