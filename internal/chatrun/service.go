// Package chatrun runs embedded chat: its routes, the runner that starts a run
// and executes the model's tool calls, approvals and other controls, deletion,
// the rounds of a run, fitting each model request to its connection, the
// evidence and provenance of tool results, review workflows, research, the
// run's terminal state and the Jev citation check of an answer. Web keeps
// every authorization check, the chat guard and the action dispatcher, and the
// engine reaches them through Hooks.
package chatrun

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/citationchecks"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/personal"
	"forgetmenote/internal/pg"
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

	// Interactive is the request's signed-in person; Authenticated
	// authenticates the request again while a stream or deletion runs.
	Interactive   func(*http.Request) (string, error)
	Authenticated func(*http.Request) bool
	// RunAuth captures the request's session for a conversation's run, and
	// RunContext is the context a run works in, as its owner, after CheckRun.
	RunAuth    func(*http.Request, chat.Conversation) chat.RunAuth
	RunContext func(chat.RunAuth) (context.Context, error)
	// CheckDataConversation rechecks the data grants a whole conversation
	// cited; ResolveCorpus resolves and checks the notebooks a run may read.
	CheckDataConversation func(ctx context.Context, c chat.Conversation) error
	ResolveCorpus         func(ctx context.Context, store *workspace.Store, requested, inherited *chat.Corpus, legacy bool) (*chat.Corpus, error)
	// ActionCatalog lists the registered actions for the chat page.
	ActionCatalog func() []map[string]any
	// Citations returns the citation check sidecars, which Jev opens after
	// the engine starts.
	Citations func() *citationchecks.Runtime

	// A model's tool call reaches an action only through these: the
	// dispatcher's canonical arguments, the chat guard's preflight of a
	// change, whether a change carrying personal context needs approval, the
	// read-only refusal an approval must not outrun, and the dispatcher
	// itself, which applies the chat guard again. The engine never reaches
	// the action registry.
	ActionArguments  func(project, name string, args json.RawMessage) (json.RawMessage, error)
	GuardAction      func(ctx context.Context, project, name string, args json.RawMessage) (context.Context, error)
	PersonalApproval func(user, project string, personal bool) bool
	ReadOnly         func(project string, run chat.Run) error
	DispatchAction   func(ctx context.Context, project, name string, args json.RawMessage) (json.RawMessage, error)
}

// Service runs chat runs for one instance.
type Service struct {
	catalog *catalog.Catalog
	db      *pg.DB
	hooks   Hooks
}

// New returns the engine. db holds the locks a conversation's deletion and
// citation checks share across gateways. It refuses a nil catalog, database
// or hook.
func New(c *catalog.Catalog, db *pg.DB, hooks Hooks) (*Service, error) {
	h := hooks
	if c == nil || db == nil || h.Chat == nil || h.Origin == nil || h.DataConnections == nil || h.Repositories == nil ||
		h.CheckRun == nil || h.ClientAccess == nil || h.ToolStore == nil || h.CheckDataCitations == nil || h.CheckSourceScope == nil ||
		h.JevConfigured == nil || h.JevAuthorized == nil || h.PersonalConsentStamp == nil || h.PersonalContext == nil || h.RecordPersonalInteraction == nil ||
		h.ActionTool == nil || h.ChatTools == nil ||
		h.AcquireEngine == nil || h.InstructionsAt == nil || h.RetrieveScoped == nil || h.DecisionPreferences == nil || h.AcquireSourceWork == nil ||
		h.Interactive == nil || h.Authenticated == nil || h.RunAuth == nil || h.RunContext == nil ||
		h.CheckDataConversation == nil || h.ResolveCorpus == nil || h.ActionCatalog == nil || h.Citations == nil ||
		h.ActionArguments == nil || h.GuardAction == nil || h.PersonalApproval == nil || h.ReadOnly == nil || h.DispatchAction == nil {
		return nil, errors.New("the chat engine requires the catalog, the database and every hook")
	}
	return &Service{catalog: c, db: db, hooks: hooks}, nil
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
