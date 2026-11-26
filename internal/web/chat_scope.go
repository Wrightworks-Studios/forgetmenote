package web

import (
	"context"
	"encoding/json"
	"slices"

	"forgetmenote/internal/chat"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
)

func (s *Server) resolveChatCorpus(ctx context.Context, store *workspace.Store, requested, inherited *chat.Corpus, legacy bool) (*chat.Corpus, error) {
	selected := requested
	if selected == nil {
		selected = inherited
	}
	// A protected Inbox reaches a model only for its consenting owner.
	if err := s.protectedInboxAIAllowed(subject(ctx), projectID(ctx)); err != nil {
		return nil, err
	}
	if selected != nil {
		if err := selected.Validate(); err == nil {
			copy := *selected
			copy.NoteProjects = append([]string(nil), selected.NoteProjects...)
			copy.SourceProjects = append([]string(nil), selected.SourceProjects...)
			// Authorize the whole selection before resolving any external source.
			for _, p := range append(append([]string(nil), copy.SourceProjects...), copy.NoteProjects...) {
				if _, err := s.catalog.Access(subject(ctx), p, "read"); err != nil {
					return nil, err
				}
				if err := s.protectedInboxAIAllowed(subject(ctx), p); err != nil {
					return nil, err
				}
			}
			return s.resolveChatSourceBindings(ctx, &copy)
		} else if requested != nil || !legacy {
			return nil, err
		}
	}
	scope, err := withEngine(s, ctx, store, func(e *knowledge.Engine) (workspace.ContentScope, error) {
		return e.ResolveContentScope(knowledge.ContainerSelection{}, true)
	})
	if err != nil {
		return nil, err
	}
	return &chat.Corpus{Version: 1, Archives: scope.Policy.Archives, Drafts: scope.Policy.Drafts, History: false}, nil
}

func scopeChatActionArguments(ctx context.Context, name string, raw json.RawMessage) (json.RawMessage, error) {
	scope := chat.RunScopeFrom(ctx)
	if scope == nil {
		return raw, nil
	}
	if err := scope.Run.Options.Corpus.Validate(); err != nil {
		return nil, err
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(raw, &args) != nil {
		return nil, chat.ErrOutsideCorpus
	}
	switch policy(name).Scope {
	case selectedRepositories:
		var projects []string
		if json.Unmarshal(args["projects"], &projects) != nil || len(projects) == 0 {
			return nil, chat.ErrOutsideCorpus
		}
		for _, p := range projects {
			if !slices.Contains(scope.Run.Options.Corpus.SourceProjects, p) {
				return nil, chat.ErrOutsideCorpus
			}
		}
	case selectedRepository:
		var project string
		if json.Unmarshal(args["project"], &project) != nil || !slices.Contains(scope.Run.Options.Corpus.SourceProjects, project) {
			return nil, chat.ErrOutsideCorpus
		}
	}
	switch name {
	case "browse_folder", "search_notes", "hybrid_search", "semantic_search":
		query := args
		if name == "hybrid_search" || name == "semantic_search" {
			if json.Unmarshal(args["query"], &query) != nil {
				return nil, chat.ErrOutsideCorpus
			}
		}
		for field, choice := range map[string]string{"archives": scope.Content.Policy.Archives, "drafts": scope.Content.Policy.Drafts} {
			var supplied string
			if value, ok := query[field]; ok && (json.Unmarshal(value, &supplied) != nil || supplied != "" && supplied != choice) {
				return nil, chat.ErrOutsideCorpus
			}
			query[field], _ = json.Marshal(choice)
		}
		var history string
		var trash bool
		if value, ok := query["scope"]; ok {
			if json.Unmarshal(value, &history) != nil || history == "history" && !scope.Run.Options.Corpus.History {
				return nil, chat.ErrOutsideCorpus
			}
		}
		if value, ok := query["includeTrash"]; ok {
			if json.Unmarshal(value, &trash) != nil || trash {
				return nil, chat.ErrOutsideCorpus
			}
		}
		if name == "hybrid_search" || name == "semantic_search" {
			if history == "" {
				history = "current"
				if scope.Run.Options.Corpus.History {
					history = "history"
				}
				query["scope"], _ = json.Marshal(history)
			}
			args["query"], _ = json.Marshal(query)
		}
	}
	return json.Marshal(args)
}

func (s *Server) resolveChatSourceBindings(ctx context.Context, corpus *chat.Corpus) (*chat.Corpus, error) {
	corpus.SourceBindings = nil
	if len(corpus.SourceProjects) == 0 {
		return corpus, nil
	}
	if s.repositories == nil {
		return nil, chat.ErrOutsideCorpus
	}
	corpus.SourceBindings = map[string]string{}
	for _, p := range corpus.SourceProjects {
		if _, err := s.catalog.Access(subject(ctx), p, "read"); err != nil {
			return nil, err
		}
		state, err := s.repositories.Status(p)
		if err != nil {
			return nil, err
		}
		if !state.Connected || !state.IndexConfig.ExternalProcessing {
			return nil, chat.ErrOutsideCorpus
		}
		if err = s.repositories.Authorize(ctx, p, state.ID); err != nil {
			return nil, err
		}
		corpus.SourceBindings[p] = state.ID
	}
	return corpus, nil
}
