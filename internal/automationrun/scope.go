package automationrun

import (
	"context"
	"errors"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/chat"
)

// SourceContext binds the notebooks and repository sources an AI run may
// read to ctx as its run scope, after checking the run's actor may read each.
// A context that already has a run scope keeps it.
func (s *Service) SourceContext(ctx context.Context, r *automation.Run) (context.Context, error) {
	if chat.RunScopeFrom(ctx) != nil || !r.Spec.UsesAI() {
		return ctx, nil
	}
	corpus := &chat.Corpus{Version: 1, Archives: "exclude", Drafts: "exclude"}
	for _, project := range append([]string{r.Spec.Project}, r.Spec.ReadProjects...) {
		if _, err := s.hooks.ClientAccess(ctx, r.Spec.Actor, project, "read"); err != nil {
			return ctx, err
		}
		connected := false
		if s.hooks.Repositories() != nil {
			state, err := s.hooks.Repositories().Status(project)
			if err != nil {
				return ctx, err
			}
			connected = state.Connected
		}
		if connected {
			corpus.SourceProjects = append(corpus.SourceProjects, project)
		} else if s.catalog.Kind(project) == "repository" {
			return ctx, errors.New("repository sources unavailable")
		} else if project != r.Spec.Project {
			corpus.NoteProjects = append(corpus.NoteProjects, project)
		}
	}
	corpus, err := s.hooks.ResolveSourceBindings(ctx, corpus)
	if err != nil {
		return ctx, err
	}
	run := chat.Run{ID: r.ID, Options: chat.Options{Corpus: corpus}}
	content, err := chat.ContentScope(run)
	if err != nil {
		return ctx, err
	}
	return chat.WithScope(ctx, &chat.RunScope{Owner: r.Spec.Actor, Project: r.Spec.Project, Run: run, Content: content}), nil
}
