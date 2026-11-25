package web

import (
	"context"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
)

func scopedChatMetadata(ctx context.Context, store *workspace.Store, revision string) (workspace.Snapshot, error) {
	current, err := chatScopedSnapshot(ctx, store, "")
	if err != nil {
		return workspace.Snapshot{}, err
	}
	if revision != "" && revision != current.Revision {
		scope := chatScopeFrom(ctx)
		if !scope.Run.Options.Corpus.History {
			return workspace.Snapshot{}, errOutsideChatCorpus
		}
		current, err = chatScopedSnapshot(ctx, store, revision)
		if err != nil {
			return workspace.Snapshot{}, err
		}
	}
	for i := range current.Files {
		current.Files[i].Content = ""
	}
	return current, nil
}
func scopedChatDiscussion(ctx context.Context, store *workspace.Store, path string) (workspace.Discussion, error) {
	doc, err := readChatNote(ctx, store, path, "")
	if err != nil {
		return workspace.Discussion{}, err
	}
	result, err := store.Comments(ctx, path)
	if err != nil {
		return result, err
	}
	// Comments may be read while another writer reclassifies the committed note.
	// Bind this discussion to the validated committed snapshot before projection.
	if result.Revision != doc.Revision {
		return workspace.Discussion{}, workspace.ErrConflict
	}
	for _, comment := range result.Comments {
		if comment.Finding == nil {
			continue
		}
		for _, ref := range comment.Finding.Evidence {
			if ref.Path != "" {
				if _, err = readChatNote(ctx, store, ref.Path, ref.Revision); err != nil {
					return workspace.Discussion{}, errOutsideChatCorpus
				}
			}
		}
	}
	return result, nil
}
func (s *Server) scopedChatInbox(ctx context.Context, store *workspace.Store, e *knowledge.Engine) (map[string]any, error) {
	snapshot, err := chatScopedSnapshot(ctx, store, "")
	if err != nil {
		return nil, err
	}
	findings := []workspace.ReviewFinding{}
	for _, file := range snapshot.Files {
		discussion, err := scopedChatDiscussion(ctx, store, file.Path)
		if err != nil {
			return nil, err
		}
		for _, comment := range discussion.Comments {
			if comment.Finding != nil {
				findings = append(findings, workspace.ReviewFinding{Path: file.Path, Comment: comment, Revision: discussion.Revision})
			}
		}
	}
	all, err := e.Proposals(ctx)
	if err != nil {
		return nil, err
	}
	proposals := []knowledge.Proposal{}
	for _, proposal := range all {
		if validateChatProposal(ctx, store, proposal, chatScopeFrom(ctx).Content) == nil {
			proposals = append(proposals, proposal)
		}
	}
	return map[string]any{"findings": findings, "proposals": proposals, "completionMeaning": "Eligible current notes only; external checkpoints do not establish scoped review coverage."}, nil
}
