package web

import (
	"context"

	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/purgehttp"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// initPurge binds purge to this instance's access checks and to the owners of
// what it cleans outside the notebook: chat transcripts, judgment records,
// Knowledge proposals and bundle exports. The chat store, judgments and the origin
// are read at each call.
func (s *Server) initPurge() error {
	var err error
	s.purge, err = purgehttp.New(s.catalog, s.db, s.indexes, s.data, purgehttp.Hooks{
		ClientProjectAllowed: s.clientProjectAllowed,
		RedactChats: func(ctx context.Context, project string, redact func([]byte) ([]byte, bool, error), write bool) (int, error) {
			if s.chat == nil || s.chat.Store == nil {
				return 0, nil
			}
			return s.chat.Store.RedactProject(ctx, project, redact, write)
		},
		RedactJudgmentRecords: func(ctx context.Context, notebook string, redact func([]byte) ([]byte, bool, error), write bool) (int, error) {
			if s.judgments == nil {
				return 0, nil
			}
			return s.judgments.RedactRecords(ctx, notebook, redact, write)
		},
		OwnsJudgmentJob: func(job, user string) bool {
			return s.judgments != nil && s.judgments.OwnsJob(job, user)
		},
		KnowledgeHolds: func(ctx context.Context, store *workspace.Store, redact func([]byte) ([]byte, bool, error)) (int, error) {
			return withEngine(s, ctx, store, func(e *knowledge.Engine) (int, error) {
				return e.PurgeHolds(ctx, redact)
			})
		},
		FenceExports:           s.fenceExports,
		DropBundleExports:      s.dropBundleExports,
		RemoveTemporaryBundles: s.removeTemporaryBundles,
		Origin:                 func() string { return s.origin },
	})
	return err
}

// purgeTools lets an agent prepare a purge for a person to run, in a notebook
// it opens as its Owner.
func (s *Server) purgeTools(server *mcp.Server) {
	addAction(s, server, &mcp.Tool{Name: "prepare_purge", Description: "Prepare a plan to permanently erase content from every version of a notebook: a whole note, chosen versions of one, or an exact text or pattern (such as a leaked API key) from chosen notes or the whole notebook. Requires Owner or Admin. Changes nothing: it returns the plan (affected notes and versions, other places the text appears, references remapped, whether the GitHub copy is force-pushed) and a browser URL where a person reviews it, gives a reason and runs it. Agents cannot run a purge. Tell the user that a purge cannot undo exposure: rotate leaked secrets regardless."}, func(ctx context.Context, in purgehttp.AgentInput) (purgehttp.AgentPlan, error) {
		store, err := s.toolStore(ctx, in.Project, "owner")
		if err != nil {
			return purgehttp.AgentPlan{}, err
		}
		return s.purge.PrepareForAgent(ctx, store, in)
	}, "notes:manage")
}
