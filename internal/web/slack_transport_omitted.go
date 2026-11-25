//go:build fmn_omit_slack

package web

import (
	"context"
	"encoding/json"

	"forgetmenote/internal/integrations"
	"forgetmenote/internal/messaging"
)

// ConfigureSlackConfig refuses Slack in this build, so no slackService exists
// and none of these runs.

func (s *slackService) VerifyMember(context.Context, messaging.Address) error {
	return integrations.Check("slack")
}
func (s *slackService) EventMember(json.RawMessage) (messaging.Member, bool, error) {
	return messaging.Member{}, false, integrations.Check("slack")
}
func (s *slackService) Members(context.Context) ([]messaging.Member, error) {
	return nil, integrations.Check("slack")
}
func (s *slackService) ChannelMembers(context.Context, string) ([]string, error) {
	return nil, integrations.Check("slack")
}
func (s *slackService) Permalink(context.Context, string, string) (string, error) {
	return "", integrations.Check("slack")
}
func (s *slackService) GroupMembers(context.Context, string) ([]string, error) {
	return nil, integrations.Check("slack")
}
func (s *slackService) Status(context.Context, messaging.Address, string) error {
	return integrations.Check("slack")
}
func (s *slackService) Context(context.Context, messaging.Address, messaging.Binding) (messaging.Context, error) {
	return messaging.Context{}, integrations.Check("slack")
}
func (s *slackService) Send(context.Context, messaging.Address, messaging.Reply) error {
	return messaging.NotPosted(integrations.Check("slack"))
}
func (s *slackService) Post(context.Context, string, messaging.Post) (string, error) {
	return "", messaging.NotPosted(integrations.Check("slack"))
}
func (s *slackService) OpenView(context.Context, string, any) error {
	return integrations.Check("slack")
}
func (s *slackService) Respond(context.Context, string, string) error {
	return integrations.Check("slack")
}
func (s *slackService) RespondAside(context.Context, string, string) error {
	return integrations.Check("slack")
}
