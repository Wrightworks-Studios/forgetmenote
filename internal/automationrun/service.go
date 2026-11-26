// Package automationrun runs automations: the dispatcher that runs each queued
// run by its kind, the model turn of an answer and its tool loop, publication
// to the run's note and notification through the messaging hub, the task,
// recipe, digest, monitor, checks, native and Drive kinds, installed script
// packages with their host operations and updates, learning from observed
// data calls, recipe repair, and the runtime's start. Web keeps every
// authorization check: a run's identity and its recheck, the instance AI
// policy, client notebook access, data connection grants, definition
// validation, a learning event's actor and credential, document reads,
// personal context and consent, and the recheck of a run's evidence. The
// engine reaches them, and the action dispatcher, through Hooks.
package automationrun

import (
	"context"
	"encoding/json"
	"errors"

	"forgetmenote/internal/agentmemory"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/backups"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/chatrun"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/dataops"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/messaginghttp"
	"forgetmenote/internal/notebooks"
	"forgetmenote/internal/outgoing"
	"forgetmenote/internal/personal"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/taskqueue"
	"forgetmenote/internal/workspace"
)

// Hooks are what the engine needs from web. Each is required; one for
// something an instance has not configured returns its zero value.
type Hooks struct {
	// These return values web sets or replaces after startup, or that start
	// after the engine, so the engine reads them at each call. SlackConfigured
	// reports whether the Slack app is mounted.
	Chat            func() *chat.Runtime
	DataConnections func() *connectors.Manager
	Repositories    func() *reposource.Manager
	Backups         func() *backups.Manager
	Messaging       func() *messaginghttp.Service
	Personal        func() *personal.Service
	Tasks           func() *taskqueue.Service
	SlackConfigured func() bool
	// Python is the runtime installed packages run in, nil until web
	// configures one, and SlackPost posts a script's message to a channel
	// through the Slack app.
	Python    func() *automation.Python
	SlackPost func(ctx context.Context, channel string, post messaging.Post) (string, error)

	// RunContext is the context a run works in: its definition's identity,
	// bound after the definition's policy and its actor's account and read
	// access are checked. CheckRun rechecks that identity: the policy, the
	// definition fence, the session, the notebook epochs and authority.
	// Policy is a definition's instance AI policy and personal check, which
	// the runtime applies before it queues or runs a job. AIWriting refuses
	// a model call while the instance AI policy turns AI writing off.
	RunContext func(ctx context.Context, spec automation.Spec) (context.Context, error)
	CheckRun   func(ctx context.Context) error
	Policy     func(automation.Spec) error
	AIWriting  func() error
	// CheckEvidence rechecks what a run relies on before each provider call,
	// publication and delivery: its identity, its event evidence, its data
	// receipts and the sources it cites. upstream is false for the lighter
	// recheck on each streamed event.
	CheckEvidence func(ctx context.Context, r *automation.Run, upstream bool) error

	// ClientAccess opens a notebook store for the context's identity, after
	// its credential, scope, automation and notebook role checks. DataAllowed
	// reports whether the context may use a data connection's operation, or
	// the connection itself for an empty operation: its grants, the notebooks
	// the connection serves, and its code-only and external processing rules.
	// DataExecute runs a data call through the shared data operations, which
	// check its grant; DataList lists the connections the context may use.
	ClientAccess func(ctx context.Context, actor, project, role string) (*workspace.Store, error)
	DataAllowed  func(ctx context.Context, connection, operation string) bool
	DataExecute  func(ctx context.Context, in dataops.Call) (dataops.Result, error)
	DataList     func(ctx context.Context) []connectors.Info
	// ReadDocument and NotebookMap read a document and a notebook's map as
	// the context's identity, through the same checks as the read_document and
	// notebook_map tools.
	ReadDocument func(ctx context.Context, ref notebooks.DocumentRef, lines reposource.LineRange) (notebooks.DocumentOutput, error)
	NotebookMap  func(ctx context.Context, project, origin, source string, q knowledge.MapQuery) (any, error)

	// ValidateDefinition checks a definition as saving it would: its actor's
	// role on each notebook, its connection grants and its Slack audience. A
	// package update checks the updated definition with it.
	ValidateDefinition func(ctx context.Context, spec automation.Spec) error
	// CheckLearningEvent rechecks that an observed data call may still teach
	// a learning run: its actor's account, session and notebook access, the
	// connection grant, the credential the call was made with, an OAuth
	// credential's too, and the data connection's own rules.
	CheckLearningEvent func(ctx context.Context, r *automation.Run, e automation.LearningEvent) error
	// JudgmentsConfigured is the configured judge, nil without one, and
	// whether local-only mode prohibits it.
	JudgmentsConfigured func() (*decision.Service, bool)

	// A model's tool call reaches an action only through DispatchAction, the
	// action dispatcher, which checks the caller's access and applies the
	// chat guard. The engine never reaches the action registry.
	DispatchAction func(ctx context.Context, project, name string, args json.RawMessage) (json.RawMessage, error)
	// DocumentContentScope resolves the notes a selection may read, within the
	// context's run scope; ResolveSourceBindings checks and pins the
	// repository sources of a run's corpus; AcquireEngine lends a notebook's
	// knowledge engine; AcquireSourceWork admits source work.
	DocumentContentScope  func(ctx context.Context, store *workspace.Store, selection knowledge.ContainerSelection) (workspace.ContentScope, error)
	ResolveSourceBindings func(ctx context.Context, corpus *chat.Corpus) (*chat.Corpus, error)
	AcquireEngine         func(ctx context.Context, store *workspace.Store) (*knowledge.Engine, func(), error)
	AcquireSourceWork     func(ctx context.Context) (func(), error)

	// PersonalContext reads a person's personal assistant context for a run,
	// and ConsentStamp is their current consent to it. PersonalSpecs are the
	// personal assistant's jobs, and EmailPairing runs the native task that
	// pairs messaging members with accounts, which web owns.
	PersonalContext func(ctx context.Context) (string, error)
	ConsentStamp    func(user string) string
	PersonalSpecs   func() []automation.Spec
	EmailPairing    func(ctx context.Context, r *automation.Run) (map[string]any, automation.Incident, error)
}

// Service runs an instance's automations.
type Service struct {
	catalog  *catalog.Catalog
	runtime  *automation.Runtime
	chatrun  *chatrun.Service
	memory   *agentmemory.Service
	outgoing *outgoing.Mode
	// data is the instance's data directory, whose storage the
	// storage-health task inspects.
	data  string
	hooks Hooks
	// Same credential-free source boundary as document capture; replaceable
	// by local HTTP fixtures in tests without weakening production DNS and
	// redirect checks.
	scriptImageFetch     func(context.Context, string) ([]byte, string, string, error)
	scriptWebsiteCapture func(context.Context, string) (chat.WebsiteEvidence, error)
}

// New returns the engine for runtime's queue and definitions. It refuses a nil
// dependency or hook.
func New(c *catalog.Catalog, runtime *automation.Runtime, chats *chatrun.Service, memory *agentmemory.Service, mode *outgoing.Mode, data string, hooks Hooks) (*Service, error) {
	h := hooks
	if c == nil || runtime == nil || chats == nil || memory == nil || mode == nil ||
		h.Chat == nil || h.DataConnections == nil || h.Repositories == nil || h.Backups == nil || h.Messaging == nil || h.Personal == nil || h.Tasks == nil || h.SlackConfigured == nil || h.Python == nil || h.SlackPost == nil ||
		h.RunContext == nil || h.CheckRun == nil || h.Policy == nil || h.AIWriting == nil || h.CheckEvidence == nil ||
		h.ClientAccess == nil || h.DataAllowed == nil || h.DataExecute == nil || h.DataList == nil || h.ReadDocument == nil || h.NotebookMap == nil ||
		h.ValidateDefinition == nil || h.CheckLearningEvent == nil || h.JudgmentsConfigured == nil ||
		h.DispatchAction == nil || h.DocumentContentScope == nil || h.ResolveSourceBindings == nil || h.AcquireEngine == nil || h.AcquireSourceWork == nil ||
		h.PersonalContext == nil || h.ConsentStamp == nil || h.PersonalSpecs == nil || h.EmailPairing == nil {
		return nil, errors.New("the automation engine requires the catalog, the runtime, chat, memory, the outgoing mode and every hook")
	}
	return &Service{catalog: c, runtime: runtime, chatrun: chats, memory: memory, outgoing: mode, data: data, hooks: hooks}, nil
}

// repos is the repository source manager.
func (s *Service) repos() (*reposource.Manager, error) {
	if m := s.hooks.Repositories(); m != nil {
		return m, nil
	}
	return nil, reposource.ErrUnavailable
}

// withEngine runs fn with the notebook's knowledge engine.
func withEngine[O any](s *Service, ctx context.Context, store *workspace.Store, fn func(*knowledge.Engine) (O, error)) (O, error) {
	e, release, err := s.hooks.AcquireEngine(ctx, store)
	if err != nil {
		var zero O
		return zero, err
	}
	defer release()
	return fn(e)
}
