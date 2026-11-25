package web

import (
	"context"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
)

func (s *Server) presentDiscussion(ctx context.Context, notebook string, value workspace.Discussion, err error) (webcore.DiscussionOutput, error) {
	if err != nil {
		return webcore.DiscussionOutput{}, err
	}
	return webcore.PresentDiscussion(s.catalog, subject(ctx), notebook, value)
}
func (s *Server) presentHistory(ctx context.Context, notebook string, value []workspace.Commit, err error) ([]webcore.HistoryOutput, error) {
	if err != nil {
		return nil, err
	}
	return webcore.PresentHistory(s.catalog, subject(ctx), notebook, value)
}
