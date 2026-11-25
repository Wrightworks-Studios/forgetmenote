//go:build !fmn_omit_slack

package web

import (
	"context"
	"encoding/json"

	"forgetmenote/internal/messaging"
	"forgetmenote/internal/sources"
)

// The Slack Web API transport of a configured Slack app. A build with the tag
// fmn_omit_slack leaves it out (slack_transport_omitted.go), and
// ConfigureSlackConfig refuses Slack configuration there.

// transport reads the endpoint for every call, so tests can redirect a
// configured service to a local server. Every call uses the bot token as the
// instance, so a paused instance makes none: the transport asks the outgoing
// mode before each request.
func (s *slackService) transport() *messaging.Slack {
	return &messaging.Slack{Team: s.config.Team, Endpoint: s.endpoint, Client: s.client, Allow: s.outgoing.Check, Token: func() (string, error) { return runtimeSecret(s.config.BotTokenFile) }, Extract: slackAttachmentText}
}
func slackAttachmentText(ctx context.Context, name, url string, data []byte) (string, string, error) {
	capture, err := sources.Prepare(ctx, name, url, data)
	return capture.Text, capture.Record.Extraction + " " + capture.Record.Warning, err
}

func (s *slackService) VerifyMember(ctx context.Context, d messaging.Address) error {
	return s.transport().VerifyMember(ctx, d)
}
func (s *slackService) EventMember(raw json.RawMessage) (messaging.Member, bool, error) {
	return s.transport().EventMember(raw)
}
func (s *slackService) Members(ctx context.Context) ([]messaging.Member, error) {
	return s.transport().Members(ctx)
}
func (s *slackService) ChannelMembers(ctx context.Context, channel string) ([]string, error) {
	return s.transport().ChannelMembers(ctx, channel)
}
func (s *slackService) Permalink(ctx context.Context, channel, ts string) (string, error) {
	return s.transport().Permalink(ctx, channel, ts)
}
func (s *slackService) GroupMembers(ctx context.Context, group string) ([]string, error) {
	return s.transport().GroupMembers(ctx, group)
}
func (s *slackService) Status(ctx context.Context, d messaging.Address, status string) error {
	return s.transport().Status(ctx, d, status)
}
func (s *slackService) Context(ctx context.Context, d messaging.Address, b messaging.Binding) (messaging.Context, error) {
	return s.transport().Context(ctx, d, b)
}
func (s *slackService) Send(ctx context.Context, d messaging.Address, reply messaging.Reply) error {
	return s.transport().Send(ctx, messageAddress(d), reply)
}
func (s *slackService) Post(ctx context.Context, channel string, p messaging.Post) (string, error) {
	return s.transport().Post(ctx, channel, p)
}
func (s *slackService) OpenView(ctx context.Context, trigger string, view any) error {
	return s.transport().OpenView(ctx, trigger, view)
}
func (s *slackService) Respond(ctx context.Context, responseURL, text string) error {
	return s.transport().Respond(ctx, responseURL, text)
}
func (s *slackService) RespondAside(ctx context.Context, responseURL, text string) error {
	return s.transport().RespondAside(ctx, responseURL, text)
}
