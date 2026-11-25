package web

import (
	"context"
	"encoding/json"
	"errors"
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
func chatCorpusSnapshot(ctx context.Context, store *workspace.Store, run chat.Run, revision string) (workspace.Snapshot, error) {
	scope, err := chat.ContentScope(run)
	if err != nil {
		return workspace.Snapshot{}, err
	}
	snapshot, err := store.SnapshotScoped(ctx, revision, scope)
	if err != nil {
		return workspace.Snapshot{}, err
	}
	files := snapshot.Files[:0]
	for _, file := range snapshot.Files {
		if file.Path != "AGENTS.md" && !workspace.IsTrash(file.Path) {
			files = append(files, file)
		}
	}
	snapshot.Files = files
	return snapshot, nil
}
func bindChatRetrieval(run chat.Run, query knowledge.RetrievalQuery) (knowledge.RetrievalQuery, error) {
	scope, err := chat.ContentScope(run)
	if err != nil {
		return query, err
	}
	if query.IncludeTrash || query.Archives != "" && query.Archives != scope.Policy.Archives || query.Drafts != "" && query.Drafts != scope.Policy.Drafts || query.Scope == "history" && !run.Options.Corpus.History {
		return query, errors.New("requested retrieval is outside this run's saved corpus")
	}
	query.NotesOnly = true
	query.Archives, query.Drafts = scope.Policy.Archives, scope.Policy.Drafts
	if query.Scope == "" {
		query.Scope = "current"
		if run.Options.Corpus.History {
			query.Scope = "history"
		}
	}
	return query, nil
}

// Bind shared document operations to an origin selected at run creation. An
// attachment in the current notebook is never authority to read another one.
func (s *Server) documentChatScope(ctx context.Context, project, origin, source, revision, path string) (context.Context, error) {
	scope := chat.RunScopeFrom(ctx)
	if scope == nil {
		return ctx, nil
	}
	if scope.Run.Options.Corpus.Validate() != nil {
		return ctx, chat.ErrOutsideCorpus
	}
	if origin == "notes" {
		if project != scope.Project && !slices.Contains(scope.Run.Options.Corpus.NoteProjects, project) {
			return ctx, chat.ErrOutsideCorpus
		}
		if project != scope.Project {
			copy := *scope
			copy.Origin.Attachments = nil
			copy.Run.Citations = nil
			for _, citation := range scope.Run.Citations {
				if citation.Project == project && citation.Kind == "notebook" {
					copy.Run.Citations = append(copy.Run.Citations, citation)
				}
			}
			ctx = chat.WithScope(ctx, &copy)
		}
		return ctx, nil
	}
	if err := s.checkSourceChatScope(ctx, project); err != nil {
		return ctx, err
	}
	if s.repositories == nil {
		return ctx, chat.ErrOutsideCorpus
	}
	state, err := s.repositories.Status(project)
	if err != nil {
		return ctx, err
	}
	if !state.Connected || !state.IndexConfig.ExternalProcessing || state.ID != scope.Run.Options.Corpus.SourceBindings[project] || source != "" && source != state.ID || revision != "" && revision != state.Downloaded {
		return ctx, chat.ErrOutsideCorpus
	}
	var paths []string
	if path != "" {
		paths = []string{path}
	}
	return ctx, s.repositories.AuthorizeProcessing(ctx, project, state.ID, state.Downloaded, paths)
}

func chatScopedSnapshot(ctx context.Context, store *workspace.Store, revision string) (workspace.Snapshot, error) {
	scope := chat.RunScopeFrom(ctx)
	if scope == nil {
		return workspace.Snapshot{}, chat.ErrCorpus
	}
	return chatCorpusSnapshot(ctx, store, scope.Run, revision)
}
func readChatNote(ctx context.Context, store *workspace.Store, path, revision string) (workspace.Document, error) {
	doc, _, err := readChatNoteSnapshot(ctx, store, path, revision)
	return doc, err
}

// Return the fresh scoped snapshot alongside the pinned read so one validation
// phase can compare current content without another full notebook traversal.
func readChatNoteSnapshot(ctx context.Context, store *workspace.Store, path, revision string) (workspace.Document, workspace.Snapshot, error) {
	return readChatNoteFrom(ctx, store, path, revision, func(revision string) (workspace.Snapshot, error) {
		return chatScopedSnapshot(ctx, store, revision)
	})
}

// readChatNoteFrom is readChatNoteSnapshot taking the scoped snapshots it
// needs, "" for the current one, from snapshot, so that the reads of one
// validation pass can share them.
func readChatNoteFrom(ctx context.Context, store *workspace.Store, path, revision string, snapshot func(revision string) (workspace.Snapshot, error)) (workspace.Document, workspace.Snapshot, error) {
	var current workspace.Snapshot
	scope := chat.RunScopeFrom(ctx)
	if scope == nil {
		doc, err := store.Read(ctx, path, revision)
		return doc, current, err
	}
	if path == "AGENTS.md" || workspace.IsTrash(path) {
		return workspace.Document{}, current, chat.ErrOutsideCorpus
	}
	var err error
	current, err = snapshot("")
	if err != nil {
		return workspace.Document{}, current, err
	}
	currentEligible := false
	for _, file := range current.Files {
		if file.Path == path {
			currentEligible = true
			break
		}
	}
	// An attachment exception is an immutable source, never authority for its path.
	for _, attachment := range scope.Origin.Attachments {
		if attachment.Kind == "committed" && attachment.Path == path && attachment.Revision == revision {
			doc, err := store.KnowledgeRead(ctx, path, revision)
			if err != nil {
				return workspace.Document{}, current, err
			}
			if workspace.ContentHash(doc.Content) != workspace.ContentHash([]byte(attachment.Content)) {
				return workspace.Document{}, current, chat.ErrOutsideCorpus
			}
			return doc, current, nil
		}
	}
	if revision == "" || revision == current.Revision {
		if !currentEligible {
			return workspace.Document{}, current, chat.ErrOutsideCorpus
		}
		doc, err := store.KnowledgeRead(ctx, path, current.Revision)
		return doc, current, err
	}
	if !scope.Run.Options.Corpus.History {
		if !currentEligible {
			return workspace.Document{}, current, chat.ErrOutsideCorpus
		}
		pinned := false
		for _, citation := range scope.Run.Citations {
			if citation.Path == path && citation.Revision == revision && citation.Hash != "" {
				pinned = true
				break
			}
		}
		if !pinned {
			return workspace.Document{}, current, chat.ErrOutsideCorpus
		}
	}
	historical, err := snapshot(revision)
	if err != nil {
		return workspace.Document{}, current, err
	}
	for _, file := range historical.Files {
		if file.Path == path {
			doc, err := store.KnowledgeRead(ctx, path, revision)
			return doc, current, err
		}
	}
	return workspace.Document{}, current, chat.ErrOutsideCorpus
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
