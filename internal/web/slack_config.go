package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"forgetmenote/internal/capacity"
	"forgetmenote/internal/durable"
	"forgetmenote/internal/integrations"
	"forgetmenote/internal/jsonfile"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/outgoing"
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

// slackApp serves every request to the Slack app's endpoints: events,
// interactions and slash commands. It checks each one with slackRequest before
// any handler reads it, and hands the handler for the path only the verified
// body. Replays inside the timestamp window are answered once, by the event,
// button or form ID each handler queues under.
func (s *Server) slackApp(w http.ResponseWriter, r *http.Request, path string) {
	body, ok := s.slackRequest(w, r)
	if !ok {
		return
	}
	switch path {
	case "/integrations/slack/events":
		s.messaging.SlackEvents(w, r, body)
	case "/integrations/slack/interactions":
		s.messaging.SlackInteractions(w, r, body)
	default:
		s.slackCommands(w, r, body)
	}
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
