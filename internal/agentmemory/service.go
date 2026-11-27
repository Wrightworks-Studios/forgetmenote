package agentmemory

import (
	"context"
	"errors"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/search"
	"forgetmenote/internal/workspace"
)

// Hooks bind agent memory to the instance's shared checks and to the Jev and
// chat configuration, read at each call. All are required; an omitted check
// cannot open a second path around it.
type Hooks struct {
	// Personal reports whether the caller is a signed-in account whose
	// credential reaches all of its notebooks. Only such a caller creates
	// memory notebooks; a notebook-limited credential or an automation uses
	// only those that exist and it may open.
	Personal func(context.Context) bool
	// ClientAccess opens a notebook for the caller under its credential and
	// notebook role.
	ClientAccess func(ctx context.Context, actor, project, role string) (*workspace.Store, error)
	// ClientProjectAllowed reports whether the caller's credential reaches a
	// notebook.
	ClientProjectAllowed func(ctx context.Context, project string) bool
	// Judge is the configured Jev service, or nil.
	Judge func() *decision.Service
	// JudgeAuthorized checks that Jev may review a person's notebook for a
	// feature.
	JudgeAuthorized func(ctx context.Context, user, project, feature string, expected *decision.Service) error
	// EmbeddingPolicy withholds an embedding endpoint the chat configuration
	// keeps local.
	EmbeddingPolicy func(*knowledge.Endpoint) *knowledge.Endpoint
	// Origin is the instance's address, for links in memory instructions.
	Origin func() string
}

// Service serves one instance's agent memory.
type Service struct {
	catalog *catalog.Catalog
	indexes *search.Indexes
	hooks   Hooks
	vectors memoryVectors
}

// New serves an instance's agent memory. indexes provides the embedding
// endpoint; without it, memory search ranks by text alone.
func New(c *catalog.Catalog, indexes *search.Indexes, hooks Hooks) (*Service, error) {
	h := hooks
	if c == nil || h.Personal == nil || h.ClientAccess == nil || h.ClientProjectAllowed == nil || h.Judge == nil || h.JudgeAuthorized == nil || h.EmbeddingPolicy == nil || h.Origin == nil {
		return nil, errors.New("agent memory requires the catalog and every shared check")
	}
	return &Service{catalog: c, indexes: indexes, hooks: hooks}, nil
}
