// Package personal runs each employee's personal assistant in the background:
// it collects new mail, Slack messages, calendar events and data watches from
// their connections, asks Jev which need attention, drafts suggestions in
// their assistant notebook, keeps their profile memory current, and tells them
// in Slack.
//
// Web keeps every consent, connection and identity check: the connect and
// callback routes, the account page's approval, which notebooks and
// connections a run may read, whether a grant and its source are still
// current, and the mailbox write. The worker calls each of them through Hooks
// at the step that needs it, and reads the values web replaces after startup
// through Hooks too.
package personal

import (
	"context"
	"errors"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/messaginghttp"
	"forgetmenote/internal/workspace"
)

// Hooks are the instance's checks and state the worker uses. Each is read at
// each call.
type Hooks struct {
	// Consent and access checks, which web owns. Context returns the
	// caller's assistant profile files; ClientAccess opens a notebook as the
	// actor within the invocation's credential; Evidence reads a personal
	// source under the employee's current consent and grant; Current checks
	// that an observation's source is still allowed and unchanged;
	// CheckSources checks a run's grants before it uses their content;
	// DiscardUnavailable drops queued work whose grant or feature is gone; and
	// MailboxDraft writes an approved draft to the employee's mailbox.
	Context            func(ctx context.Context) ([]Memory, error)
	ClientAccess       func(ctx context.Context, actor, project, role string) (*workspace.Store, error)
	Evidence           func(ctx context.Context, call connectors.PersonalOperation) (map[string]any, error)
	Current            func(ctx context.Context, o Observation) error
	CheckSources       func(ctx context.Context, r *automation.Run, items []Observation) error
	DiscardUnavailable func(ctx context.Context, r *automation.Run, state *State) error
	MailboxDraft       func(ctx context.Context, d Draft, automatic bool) (connectors.Result, error)

	// Automation: the run's authority, its answers and evidence, and its data
	// calls.
	CheckAutomationAuth     func(context.Context) error
	AutomationAnswer        func(ctx context.Context, r *automation.Run, prompt string, tools bool) (string, error)
	CheckAutomationEvidence func(ctx context.Context, r *automation.Run, upstream bool) error
	AutomationCall          func(ctx context.Context, r *automation.Run, call automation.Call) (connectors.Result, error)

	// JevConfigured returns Jev and whether it runs locally.
	JevConfigured func() (*decision.Service, bool)

	// Values web replaces after startup: personal and data connections, the
	// origin, and the mounted Slack app's workspace, transport and member
	// check (ok is false when none is mounted).
	PersonalConnections func() *connectors.PersonalConnections
	DataConnections     func() *connectors.Manager
	Origin              func() string
	Slack               func() (team string, t messaging.Transport, members messaging.MemberVerifier, ok bool)
}

// Service is the personal assistant worker for one instance.
type Service struct {
	catalog     *catalog.Catalog
	automations *automation.Runtime
	messaging   *messaginghttp.Service
	hooks       Hooks
}

// New returns the worker. Every dependency and hook is required.
func New(c *catalog.Catalog, automations *automation.Runtime, chats *messaginghttp.Service, hooks Hooks) (*Service, error) {
	h := hooks
	if c == nil || automations == nil || chats == nil ||
		h.Context == nil || h.ClientAccess == nil || h.Evidence == nil || h.Current == nil || h.CheckSources == nil || h.DiscardUnavailable == nil || h.MailboxDraft == nil ||
		h.CheckAutomationAuth == nil || h.AutomationAnswer == nil || h.CheckAutomationEvidence == nil || h.AutomationCall == nil ||
		h.JevConfigured == nil || h.PersonalConnections == nil || h.DataConnections == nil || h.Origin == nil || h.Slack == nil {
		return nil, errors.New("the personal assistant requires the catalog, automations, messaging and every shared check")
	}
	return &Service{catalog: c, automations: automations, messaging: chats, hooks: hooks}, nil
}
