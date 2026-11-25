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
func chatContentScope(run chat.Run) (workspace.ContentScope, error) {
	if err := run.Options.Corpus.Validate(); err != nil {
		return workspace.ContentScope{}, err
	}
	return workspace.ContentScope{NotesOnly: true, Policy: workspace.ContainerPolicy{Version: 1, Archives: run.Options.Corpus.Archives, Drafts: run.Options.Corpus.Drafts}, Collection: "all"}, nil
}
func chatCorpusSnapshot(ctx context.Context, store *workspace.Store, run chat.Run, revision string) (workspace.Snapshot, error) {
	scope, err := chatContentScope(run)
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
	scope, err := chatContentScope(run)
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

type chatScopeKey struct{}
type chatRunScope struct {
	Owner, Project, Chat string
	Run                  chat.Run
	Origin               chat.Turn
	Content              workspace.ContentScope
	// Personal reports that this conversation has used personal assistant
	// context; outside the person's own assistant notebook its changes need
	// the person's approval. Approved marks the operation they approved.
	Personal, Approved bool
}

func withChatRunScope(ctx context.Context, c chat.Conversation, run chat.Run) (context.Context, error) {
	if run.Options.Corpus != nil && len(run.Options.Corpus.SourceProjects) > 0 && len(run.Options.Corpus.SourceBindings) != len(run.Options.Corpus.SourceProjects) {
		return nil, chat.ErrCorpus
	}
	scope, err := chatContentScope(run)
	if err != nil {
		return nil, err
	}
	for _, turn := range c.Turns {
		if turn.Role == "user" && turn.RunID == run.ID {
			return context.WithValue(ctx, chatScopeKey{}, &chatRunScope{Owner: c.Owner, Project: c.Project, Chat: c.ID, Run: run, Origin: turn, Content: scope, Personal: chatPersonalContext(c)}), nil
		}
	}
	return nil, errors.New("run is missing its original user turn")
}

// withChatApproval binds the person's approval of the operation about to run.
func withChatApproval(ctx context.Context) context.Context {
	scope := chatScopeFrom(ctx)
	if scope == nil {
		return ctx
	}
	approved := *scope
	approved.Approved = true
	return context.WithValue(ctx, chatScopeKey{}, &approved)
}
func chatScopeFrom(ctx context.Context) *chatRunScope {
	scope, _ := ctx.Value(chatScopeKey{}).(*chatRunScope)
	return scope
}

var errOutsideChatCorpus = errors.New("the requested source or action is outside this run's saved corpus")

// Bind shared document operations to an origin selected at run creation. An
// attachment in the current notebook is never authority to read another one.
func (s *Server) documentChatScope(ctx context.Context, project, origin, source, revision, path string) (context.Context, error) {
	scope := chatScopeFrom(ctx)
	if scope == nil {
		return ctx, nil
	}
	if scope.Run.Options.Corpus.Validate() != nil {
		return ctx, errOutsideChatCorpus
	}
	if origin == "notes" {
		if project != scope.Project && !slices.Contains(scope.Run.Options.Corpus.NoteProjects, project) {
			return ctx, errOutsideChatCorpus
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
			ctx = context.WithValue(ctx, chatScopeKey{}, &copy)
		}
		return ctx, nil
	}
	if err := s.checkSourceChatScope(ctx, project); err != nil {
		return ctx, err
	}
	if s.repositories == nil {
		return ctx, errOutsideChatCorpus
	}
	state, err := s.repositories.Status(project)
	if err != nil {
		return ctx, err
	}
	if !state.Connected || !state.IndexConfig.ExternalProcessing || state.ID != scope.Run.Options.Corpus.SourceBindings[project] || source != "" && source != state.ID || revision != "" && revision != state.Downloaded {
		return ctx, errOutsideChatCorpus
	}
	var paths []string
	if path != "" {
		paths = []string{path}
	}
	return ctx, s.repositories.AuthorizeProcessing(ctx, project, state.ID, state.Downloaded, paths)
}

func chatScopedSnapshot(ctx context.Context, store *workspace.Store, revision string) (workspace.Snapshot, error) {
	scope := chatScopeFrom(ctx)
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
	scope := chatScopeFrom(ctx)
	if scope == nil {
		doc, err := store.Read(ctx, path, revision)
		return doc, current, err
	}
	if path == "AGENTS.md" || workspace.IsTrash(path) {
		return workspace.Document{}, current, errOutsideChatCorpus
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
				return workspace.Document{}, current, errOutsideChatCorpus
			}
			return doc, current, nil
		}
	}
	if revision == "" || revision == current.Revision {
		if !currentEligible {
			return workspace.Document{}, current, errOutsideChatCorpus
		}
		doc, err := store.KnowledgeRead(ctx, path, current.Revision)
		return doc, current, err
	}
	if !scope.Run.Options.Corpus.History {
		if !currentEligible {
			return workspace.Document{}, current, errOutsideChatCorpus
		}
		pinned := false
		for _, citation := range scope.Run.Citations {
			if citation.Path == path && citation.Revision == revision && citation.Hash != "" {
				pinned = true
				break
			}
		}
		if !pinned {
			return workspace.Document{}, current, errOutsideChatCorpus
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
	return workspace.Document{}, current, errOutsideChatCorpus
}

func scopeChatActionArguments(ctx context.Context, name string, raw json.RawMessage) (json.RawMessage, error) {
	scope := chatScopeFrom(ctx)
	if scope == nil {
		return raw, nil
	}
	if err := scope.Run.Options.Corpus.Validate(); err != nil {
		return nil, err
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(raw, &args) != nil {
		return nil, errOutsideChatCorpus
	}
	switch policy(name).Scope {
	case selectedRepositories:
		var projects []string
		if json.Unmarshal(args["projects"], &projects) != nil || len(projects) == 0 {
			return nil, errOutsideChatCorpus
		}
		for _, p := range projects {
			if !slices.Contains(scope.Run.Options.Corpus.SourceProjects, p) {
				return nil, errOutsideChatCorpus
			}
		}
	case selectedRepository:
		var project string
		if json.Unmarshal(args["project"], &project) != nil || !slices.Contains(scope.Run.Options.Corpus.SourceProjects, project) {
			return nil, errOutsideChatCorpus
		}
	}
	switch name {
	case "browse_folder", "search_notes", "hybrid_search", "semantic_search":
		query := args
		if name == "hybrid_search" || name == "semantic_search" {
			if json.Unmarshal(args["query"], &query) != nil {
				return nil, errOutsideChatCorpus
			}
		}
		for field, choice := range map[string]string{"archives": scope.Content.Policy.Archives, "drafts": scope.Content.Policy.Drafts} {
			var supplied string
			if value, ok := query[field]; ok && (json.Unmarshal(value, &supplied) != nil || supplied != "" && supplied != choice) {
				return nil, errOutsideChatCorpus
			}
			query[field], _ = json.Marshal(choice)
		}
		var history string
		var trash bool
		if value, ok := query["scope"]; ok {
			if json.Unmarshal(value, &history) != nil || history == "history" && !scope.Run.Options.Corpus.History {
				return nil, errOutsideChatCorpus
			}
		}
		if value, ok := query["includeTrash"]; ok {
			if json.Unmarshal(value, &trash) != nil || trash {
				return nil, errOutsideChatCorpus
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
		return nil, errOutsideChatCorpus
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
			return nil, errOutsideChatCorpus
		}
		if err = s.repositories.Authorize(ctx, p, state.ID); err != nil {
			return nil, err
		}
		corpus.SourceBindings[p] = state.ID
	}
	return corpus, nil
}
