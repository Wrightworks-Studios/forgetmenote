package taskqueue

import (
	"context"
	"errors"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/dataops"
	"forgetmenote/internal/messaging"
)

// Hooks bind task queues to the instance's shared checks, and to the services
// an instance configures after it starts, read at each call. All are required;
// an omitted check cannot open a second path around it.
type Hooks struct {
	// Personal reports whether the caller is a signed-in account whose
	// credential reaches all of its notebooks. Task queues are personal, so a
	// credential limited to some notebooks, and an automation, cannot use them.
	Personal func(context.Context) bool
	// CheckAutomation rechecks an automation run's current authority; other
	// callers pass.
	CheckAutomation func(context.Context) error
	// Client returns a configured, enabled messaging client and its transport.
	Client func(id string) (messaging.Client, messaging.Transport, bool)
	// SendDM sends a direct message at most once, as effect names it.
	SendDM func(context.Context, automation.Effect, messaging.Transport, messaging.Address, messaging.Reply) (bool, error)
	// Enqueue queues a conversation event for the assistant's runner.
	Enqueue func(context.Context, messaging.Event) error
	// The configured Slack app's interactive calls.
	Permalink    func(ctx context.Context, channel, ts string) (string, error)
	OpenView     func(ctx context.Context, trigger string, view any) error
	Respond      func(ctx context.Context, responseURL, text string) error
	RespondAside func(ctx context.Context, responseURL, text string) error
	// DataCall runs a data operation as the caller, under its grants.
	DataCall func(context.Context, dataops.Call) (dataops.Result, error)
	// DataConnections and PersonalConnections are the configured managers.
	DataConnections     func() *connectors.Manager
	PersonalConnections func() *connectors.PersonalConnections
	// ReplyAuthority is the account whose reply reminders may read its Slack,
	// when the instance and the account allow them.
	ReplyAuthority func(user string) (catalog.Account, error)
	// Origin is the instance's address, for links in task messages.
	Origin func() string
}

// Service serves one instance's task queues.
type Service struct {
	catalog     *catalog.Catalog
	automations *automation.Runtime
	hooks       Hooks
	// Clock replaces the time task queues read, in tests.
	Clock func() time.Time
	// memberNames are the task workspace's member names, for task messages.
	memberNames memberNameCache
}

// New serves an instance's task queues.
func New(c *catalog.Catalog, automations *automation.Runtime, hooks Hooks) (*Service, error) {
	h := hooks
	if c == nil || automations == nil || h.Personal == nil || h.CheckAutomation == nil || h.Client == nil || h.SendDM == nil || h.Enqueue == nil || h.Permalink == nil || h.OpenView == nil || h.Respond == nil || h.RespondAside == nil || h.DataCall == nil || h.DataConnections == nil || h.PersonalConnections == nil || h.ReplyAuthority == nil || h.Origin == nil {
		return nil, errors.New("task queues require the catalog, automations and every shared check")
	}
	return &Service{catalog: c, automations: automations, hooks: hooks}, nil
}

func (s *Service) logAutomationError(stage string, err error) { s.automations.Report(stage, err) }

// ResetMemberNames replaces the cached member names with known, as if the list
// had never been read, so the next unknown member reads it. Tests use it.
func (s *Service) ResetMemberNames(known []messaging.Member) {
	c := &s.memberNames
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keep(known)
	c.read = time.Time{}
}
