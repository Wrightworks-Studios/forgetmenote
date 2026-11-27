// Package judgments owns judgment reviews and the organizer: durable review and
// organization jobs, their scheduler and runner, results, notifications, and
// each person's and notebook's judgment settings. Web keeps instance policy and
// authorization: every check of who may use judgments reaches this package through
// Hooks.
package judgments

import (
	"context"
	"errors"
	"net/http"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/citationchecks"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/outgoing"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/quicknotes"
	"forgetmenote/internal/search"
	"forgetmenote/internal/workspace"
)

// Hooks bind judgments to the instance's identity, policy and authorization. Each is
// read at each call, so a value the instance sets after New, such as the chat
// runtime or Quick Notes, is current.
type Hooks struct {
	// Identity is the signed-in person a browser request acts for.
	Identity func(*http.Request) (string, error)
	// RejectNotebookAI refuses a notebook or personal AI setting while the
	// instance policy manages AI, and reports whether it did.
	RejectNotebookAI func(http.ResponseWriter) bool
	// Judge returns the configured decision service, if any, and whether chat is
	// limited to local models.
	Judge func() (*decision.Service, bool)
	// Authorize is the shared judgment admission for one feature in one notebook.
	Authorize func(ctx context.Context, user, project, feature string, expected *decision.Service) error
	// InboxAIAllowed admits model processing of a protected Inbox only for
	// its consenting owner. ProtectedInboxAIAllowed does the same by notebook
	// ID.
	InboxAIAllowed          func(user string, p catalog.Project) error
	ProtectedInboxAIAllowed func(user, project string) error
	// Preferences are a person's judgment preferences under the instance policy.
	Preferences func(user string) (decision.Preferences, error)
	// SyncInstanceAI applies the instance AI policy to every notebook.
	SyncInstanceAI func(context.Context)
	// Quicknotes is the Quick Notes service, or nil before it opens.
	Quicknotes func() *quicknotes.Service
}

// Service runs judgment reviews and organization for one instance.
type Service struct {
	catalog       *catalog.Catalog
	db            *pg.DB
	outgoing      *outgoing.Mode
	indexes       *search.Indexes
	hooks         Hooks
	judgmentState *judgmentState
	citations     *citationchecks.Runtime
}

// New opens the judgment records. It starts no work: the instance starts the
// scheduler once it serves.
func New(c *catalog.Catalog, db *pg.DB, mode *outgoing.Mode, indexes *search.Indexes, hooks Hooks) (*Service, error) {
	h := hooks
	if c == nil || db == nil || mode == nil || indexes == nil || h.Identity == nil || h.RejectNotebookAI == nil || h.Judge == nil || h.Authorize == nil || h.InboxAIAllowed == nil || h.ProtectedInboxAIAllowed == nil || h.Preferences == nil || h.SyncInstanceAI == nil || h.Quicknotes == nil {
		return nil, errors.New("judgments requires the catalog, database, outgoing mode, indexes and every shared check")
	}
	s := &Service{catalog: c, db: db, outgoing: mode, indexes: indexes, hooks: hooks}
	if err := s.initState(); err != nil {
		return nil, err
	}
	return s, nil
}

// Close stops every review and organization run on this gateway and waits for
// them.
func (s *Service) Close() {
	if s != nil {
		s.judgmentState.close()
	}
}

// Citations is the citation-check runtime, which keeps its records beside
// the judgment records.
func (s *Service) Citations() *citationchecks.Runtime { return s.citations }

// StoredPreferences are the judgment preferences a person saved, before the
// instance policy applies.
func (s *Service) StoredPreferences(user string) (decision.Preferences, error) {
	return s.judgmentState.preferences(user)
}

// OrganizationGuard holds the organization lock until the returned function
// runs, so no organization job starts or settles meanwhile.
func (s *Service) OrganizationGuard() func() {
	s.judgmentState.organizationMu.Lock()
	return s.judgmentState.organizationMu.Unlock
}

// InvalidateOrganizationJobs withdraws every live organization job's grant
// and stops its run. The caller holds OrganizationGuard.
func (s *Service) InvalidateOrganizationJobs() error {
	return s.invalidateOrganizationJobs(func(judgmentJob) bool { return true })
}

// RedactRecords redacts a notebook's judgment records, or counts what it would
// change when write is false.
func (s *Service) RedactRecords(ctx context.Context, notebook string, redact func([]byte) ([]byte, bool, error), write bool) (int, error) {
	return s.judgmentState.records.RedactNotebook(ctx, notebook, redact, write)
}

// OwnsJob reports whether user owns the stored job.
func (s *Service) OwnsJob(job, user string) bool {
	found, err := s.judgmentState.records.ListMatching(jobName(job), map[string]any{"owner": user}, 1)
	return err == nil && len(found) > 0
}

func withEngine[O any](s *Service, ctx context.Context, store *workspace.Store, fn func(*knowledge.Engine) (O, error)) (O, error) {
	e, release, err := s.indexes.Acquire(ctx, store)
	if err != nil {
		var zero O
		return zero, err
	}
	defer release()
	return fn(e)
}
