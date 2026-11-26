package chatrun

import (
	"context"
	"errors"
	"slices"

	"forgetmenote/internal/chat"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
)

func CorpusSnapshot(ctx context.Context, store *workspace.Store, run chat.Run, revision string) (workspace.Snapshot, error) {
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

func BindRetrieval(run chat.Run, query knowledge.RetrievalQuery) (knowledge.RetrievalQuery, error) {
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
func (s *Service) DocumentScope(ctx context.Context, project, origin, source, revision, path string) (context.Context, error) {
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
	if err := s.hooks.CheckSourceScope(ctx, project); err != nil {
		return ctx, err
	}
	if s.hooks.Repositories() == nil {
		return ctx, chat.ErrOutsideCorpus
	}
	state, err := s.hooks.Repositories().Status(project)
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
	return ctx, s.hooks.Repositories().AuthorizeProcessing(ctx, project, state.ID, state.Downloaded, paths)
}

func ScopedSnapshot(ctx context.Context, store *workspace.Store, revision string) (workspace.Snapshot, error) {
	scope := chat.RunScopeFrom(ctx)
	if scope == nil {
		return workspace.Snapshot{}, chat.ErrCorpus
	}
	return CorpusSnapshot(ctx, store, scope.Run, revision)
}

func ReadNote(ctx context.Context, store *workspace.Store, path, revision string) (workspace.Document, error) {
	doc, _, err := ReadNoteSnapshot(ctx, store, path, revision)
	return doc, err
}

// Return the fresh scoped snapshot alongside the pinned read so one validation
// phase can compare current content without another full notebook traversal.
func ReadNoteSnapshot(ctx context.Context, store *workspace.Store, path, revision string) (workspace.Document, workspace.Snapshot, error) {
	return readChatNoteFrom(ctx, store, path, revision, func(revision string) (workspace.Snapshot, error) {
		return ScopedSnapshot(ctx, store, revision)
	})
}

// readChatNoteFrom is ReadNoteSnapshot taking the scoped snapshots it
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
