// Package messaginghttp serves conversations with ForgetMeNote in external
// chat clients such as Slack: the Slack app's events and interactions, the
// assistant runs a message starts and their replies, file artifacts and
// confirmed data writes.
//
// Web keeps the instance's checks and configuration: automation policy and
// identity, the personal assistant, data access, chat answers, the Slack app's
// configuration, its transport and its request signatures, and what binds a
// messaging user to an account (account links and email pairing). Web checks
// each Slack request's signature before it calls SlackEvents or
// SlackInteractions. This package reaches the rest through Hooks, each read at
// each call.
package messaginghttp

import (
	"context"
	"encoding/json"
	"errors"

	"forgetmenote/internal/agentmemory"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/dataops"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/outgoing"
	"forgetmenote/internal/taskqueue"
)

// Hooks are the instance's checks and state that messaging uses. Each is read
// at each call, so a value web replaces later, such as the Slack app or a
// data connection, is seen at once.
type Hooks struct {
	// Automation policy and identity, and the answer a run gives.
	AutomationPolicy        func(automation.Spec) error
	AutomationProvider      func(automation.Spec) (*chat.Config, chat.Connection, *chat.Provider, error)
	CheckAutomationEvidence func(ctx context.Context, r *automation.Run, upstream bool) error
	AnswerOnce              func(ctx context.Context, r *automation.Run, save func() error, unknown error, pin func() error, prompt string, tools bool, history ...chat.Message) (string, error)

	// Chat conversations.
	ChatStore             func() *chat.Store
	ValidateChatSources   func(ctx context.Context, run chat.Run, upstream bool) error
	AssistantInstructions func(ctx context.Context, spec automation.Spec) (map[string]any, string, error)

	// The personal assistant: the policy a linked direct message gets, the
	// account a messaging user linked, and its memory capture.
	PersonalMessagePolicy     func(messaging.Client, messaging.Address) messaging.Client
	LinkedOwner               func(client, workspace, user string) string
	RecordPersonalInteraction func(ctx context.Context, user string, in Interaction) error

	// Account binding, which web owns. LinkReply answers a link or unlink
	// command where web allows one; ok is false for any other message.
	// SlackMemberEvent reads a Slack member's join or change and queues the
	// email-pairing jobs it affects.
	LinkReply        func(r *automation.Run, c messaging.Client, d messaging.Address) (reply string, ok bool, err error)
	SlackMemberEvent func(eventID string, raw json.RawMessage) error

	// Data connections.
	DataConnections func() *connectors.Manager
	DataAllowed     func(ctx context.Context, connection, operation string) bool
	DataExecute     func(ctx context.Context, in dataops.Call) (dataops.Result, error)
	ActionArguments func(project, name string, raw json.RawMessage) (json.RawMessage, error)

	// The instance's origin, the configured transports by client ID, and the
	// mounted Slack app: its policy (false when none is mounted) and its
	// replies to interactions.
	Origin       func() string
	Transport    func(id string) messaging.Transport
	SlackPolicy  func() (messaging.Client, bool)
	SlackRespond func(ctx context.Context, responseURL, text string) error
}

// Interaction is a direct message's question and answer, for the personal
// assistant's memory.
type Interaction struct {
	ID       string
	UserText string
	Response string
	Source   string
}

// Service is messaging for one instance.
type Service struct {
	catalog     *catalog.Catalog
	automations *automation.Runtime
	tasks       *taskqueue.Service
	memory      *agentmemory.Service
	outgoing    *outgoing.Mode
	hooks       Hooks
}

// New returns the messaging service. Every dependency and hook is required.
func New(c *catalog.Catalog, automations *automation.Runtime, tasks *taskqueue.Service, memory *agentmemory.Service, mode *outgoing.Mode, hooks Hooks) (*Service, error) {
	h := hooks
	if c == nil || automations == nil || tasks == nil || memory == nil || mode == nil ||
		h.AutomationPolicy == nil || h.AutomationProvider == nil || h.CheckAutomationEvidence == nil || h.AnswerOnce == nil ||
		h.ChatStore == nil || h.ValidateChatSources == nil || h.AssistantInstructions == nil ||
		h.PersonalMessagePolicy == nil || h.LinkedOwner == nil || h.RecordPersonalInteraction == nil ||
		h.LinkReply == nil || h.SlackMemberEvent == nil ||
		h.DataConnections == nil || h.DataAllowed == nil || h.DataExecute == nil || h.ActionArguments == nil ||
		h.Origin == nil || h.Transport == nil || h.SlackPolicy == nil || h.SlackRespond == nil {
		return nil, errors.New("messaging requires the catalog, automations, task queues, memory, outgoing mode and every shared check")
	}
	return &Service{catalog: c, automations: automations, tasks: tasks, memory: memory, outgoing: mode, hooks: hooks}, nil
}
