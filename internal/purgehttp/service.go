package purgehttp

import (
	"context"
	"errors"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/search"
	"forgetmenote/internal/workspace"
)

// Hooks bind purge to the instance's shared checks and to the owners of what
// it cleans outside the notebook, read at each call. All are required; an
// omitted check cannot open a second path around it.
type Hooks struct {
	// ClientProjectAllowed reports whether the caller's credential reaches a
	// notebook.
	ClientProjectAllowed func(ctx context.Context, project string) bool
	// RedactChats counts, or with write redacts, the private chat transcripts
	// of project and of other notebooks that drew on it with redact; none
	// without a chat store.
	RedactChats func(ctx context.Context, project string, redact func([]byte) ([]byte, bool, error), write bool) (int, error)
	// RedactJudgmentRecords does the same for the notebook's judgment records; none
	// without judgments.
	RedactJudgmentRecords func(ctx context.Context, notebook string, redact func([]byte) ([]byte, bool, error), write bool) (int, error)
	// OwnsJudgmentJob reports whether user owns the judgment job.
	OwnsJudgmentJob func(job, user string) bool
	// KnowledgeHolds counts the notebook's Knowledge proposals that quote
	// what redact changes.
	KnowledgeHolds func(ctx context.Context, store *workspace.Store, redact func([]byte) ([]byte, bool, error)) (int, error)
	// FenceExports ends every bundle export in progress and keeps a new one
	// from recording itself until release runs.
	FenceExports func(context.Context) (release func(), err error)
	// DropBundleExports ends every bundle export in progress.
	DropBundleExports func(context.Context) error
	// RemoveTemporaryBundles removes the bundles this gateway is preparing
	// or sending.
	RemoveTemporaryBundles func() error
	// Origin is the instance's address, for the link to an agent's plan.
	Origin func() string
}

// Service serves one instance's purges.
type Service struct {
	catalog *catalog.Catalog
	db      *pg.DB
	indexes *search.Indexes
	// data is the gateway's data directory, which keeps the receipts the
	// database refused (ReceiptSpool).
	data  string
	hooks Hooks
}

// New serves the purges of the instance whose gateway keeps data.
func New(c *catalog.Catalog, db *pg.DB, indexes *search.Indexes, data string, hooks Hooks) (*Service, error) {
	h := hooks
	if c == nil || db == nil || indexes == nil || h.ClientProjectAllowed == nil || h.RedactChats == nil || h.RedactJudgmentRecords == nil || h.OwnsJudgmentJob == nil || h.KnowledgeHolds == nil || h.FenceExports == nil || h.DropBundleExports == nil || h.RemoveTemporaryBundles == nil || h.Origin == nil {
		return nil, errors.New("purge requires the catalog, database, indexes and every shared check")
	}
	return &Service{catalog: c, db: db, indexes: indexes, data: data, hooks: hooks}, nil
}
