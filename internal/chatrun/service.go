// Package chatrun is the chat run engine: the rounds of a chat run, fitting
// each model request to its connection, the evidence and provenance of tool
// results, review workflows, research and the run's terminal state. Web keeps
// the routes, the runner that starts a run, its controls and every
// authorization check, and the engine reaches them through Hooks.
package chatrun

import (
	"context"
	"encoding/json"
	"errors"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/personal"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/search"
	"forgetmenote/internal/workspace"
)

// Hooks are what the engine needs from web. Each is required; one for
// something an instance has not configured returns its zero value.
type Hooks struct {
	// Chat, Origin, DataConnections and Repositories return values web sets
	// or replaces after startup, so the engine reads them at each call.
	Chat            func() *chat.Runtime
	Origin          func() string
	DataConnections func() *connectors.Manager
	Repositories    func() *reposource.Manager

	// CheckRun rechecks that a run's session, account epoch and notebook
	// read access still hold.
	CheckRun func(chat.RunAuth) error
	// ClientAccess and ToolStore open a notebook store for the context's
	// identity, after its credential, scope and notebook role checks.
	ClientAccess func(ctx context.Context, actor, project, role string) (*workspace.Store, error)
	ToolStore    func(ctx context.Context, project, role string) (*workspace.Store, error)
	// CheckDataCitations rechecks the data grants a run's citations used.
	CheckDataCitations func(ctx context.Context, citations []chat.Citation) error
	// CheckSourceScope checks a repository source notebook the run cites.
	CheckSourceScope func(ctx context.Context, project string) error
	// JevConfigured and JevAuthorized decide whether a run may ask Jev.
	JevConfigured func() (*decision.Service, bool)
	JevAuthorized func(ctx context.Context, user, project, feature string, expected *decision.Service) error
	// PersonalConsentStamp is the person's current consent to personal
	// assistant context; PersonalContext reads that context for a run.
	PersonalConsentStamp func(user string) string
	PersonalContext      func(ctx context.Context) (string, error)
	// RecordPersonalInteraction keeps a direct exchange for profile memory.
	RecordPersonalInteraction func(ctx context.Context, user string, in personal.Interaction) error

	// ActionTool and ChatTools are the registered tools a run may offer.
	ActionTool func(name string) (chat.Tool, bool)
	ChatTools  func(mode string, enabled []string) []chat.Tool

	// AcquireEngine lends a notebook's knowledge engine; InstructionsAt reads
	// its instructions at a revision; RetrieveScoped searches within the
	// run's scope; DecisionPreferences are a person's Jev preferences;
	// AcquireSourceWork admits research source work.
	AcquireEngine       func(ctx context.Context, store *workspace.Store) (*knowledge.Engine, func(), error)
	InstructionsAt      func(ctx context.Context, store *workspace.Store, revision string) (map[string]any, error)
	RetrieveScoped      func(ctx context.Context, in search.Request) (search.Result, error)
	DecisionPreferences func(user string) (decision.Preferences, error)
	AcquireSourceWork   func(ctx context.Context) (func(), error)

	// ExecuteTool runs one of the model's tool calls through web's executor,
	// which rechecks the run, applies the chat guard and dispatches the
	// action.
	//
	// Temporary: "Move the chat runner, controls and routes into
	// internal/chatrun", the PR after this one, moves the executor here and
	// deletes this hook.
	ExecuteTool func(ctx context.Context, a chat.RunAuth, id, runID string, call chat.ToolCall, connection chat.Connection) error
	// PrepareOperation records an operation that waits for the person's
	// confirmation.
	//
	// Temporary: "Move the chat runner, controls and routes into
	// internal/chatrun", the PR after this one, moves it here and deletes
	// this hook.
	PrepareOperation func(ctx context.Context, a chat.RunAuth, id, runID string, call chat.ToolCall, canonical json.RawMessage) error
	// StartCitationCheck queues the Jev citation check of a completed run.
	//
	// Temporary: "Move the chat runner, controls and routes into
	// internal/chatrun", the PR after this one, moves the citation check
	// here and deletes this hook.
	StartCitationCheck func(a chat.RunAuth, id, runID string, explicit bool) error
}

// Service runs chat runs for one instance.
type Service struct {
	catalog *catalog.Catalog
	hooks   Hooks
}

// New returns the engine. It refuses a nil catalog or hook.
func New(c *catalog.Catalog, hooks Hooks) (*Service, error) {
	h := hooks
	if c == nil || h.Chat == nil || h.Origin == nil || h.DataConnections == nil || h.Repositories == nil ||
		h.CheckRun == nil || h.ClientAccess == nil || h.ToolStore == nil || h.CheckDataCitations == nil || h.CheckSourceScope == nil ||
		h.JevConfigured == nil || h.JevAuthorized == nil || h.PersonalConsentStamp == nil || h.PersonalContext == nil || h.RecordPersonalInteraction == nil ||
		h.ActionTool == nil || h.ChatTools == nil ||
		h.AcquireEngine == nil || h.InstructionsAt == nil || h.RetrieveScoped == nil || h.DecisionPreferences == nil || h.AcquireSourceWork == nil ||
		h.ExecuteTool == nil || h.PrepareOperation == nil || h.StartCitationCheck == nil {
		return nil, errors.New("the chat engine requires the catalog and every hook")
	}
	return &Service{catalog: c, hooks: hooks}, nil
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
