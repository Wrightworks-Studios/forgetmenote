package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"forgetmenote/internal/workspace"
)

// ErrRetrievalSettings identifies notebook-local settings failures. Callers may
// keep ordinary notebook access available, but must not fall back for retrieval.
var ErrRetrievalSettings = errors.New("retrieval settings unavailable")

type RetrievalSettings struct {
	Version int                       `json:"version"`
	Search  workspace.ContainerPolicy `json:"search"`
	AI      workspace.ContainerPolicy `json:"ai"`
}
type RetrievalSettingsState struct {
	Settings RetrievalSettings `json:"settings"`
	ID       string            `json:"id"`
}

func DefaultRetrievalSettings() RetrievalSettings {
	return RetrievalSettings{Version: 1, Search: workspace.ContainerPolicy{Version: 1, Archives: "exclude", Drafts: "include"}, AI: workspace.ContainerPolicy{Version: 1, Archives: "exclude", Drafts: "exclude"}}
}
func (s RetrievalSettings) Validate() error {
	if s.Version != 1 {
		return errors.New("retrieval settings require version 1")
	}
	return errors.Join(s.Search.Validate(), s.AI.Validate())
}
func (s RetrievalSettings) ID() string { raw, _ := json.Marshal(s); return workspace.ContentHash(raw) }

// RetrievalSettings reports the settings this engine last read, rereading them
// first when the database answers.
func (e *Engine) RetrievalSettings() RetrievalSettingsState {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.refreshRetrieval(context.Background())
	return RetrievalSettingsState{Settings: e.retrievalSettings, ID: e.retrievalSettings.ID()}
}
func (e *Engine) SetRetrievalSettings(settings RetrievalSettings, expectedID string) (RetrievalSettingsState, error) {
	if err := settings.Validate(); err != nil {
		return RetrievalSettingsState{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.refreshRetrieval(context.Background()); err != nil {
		return RetrievalSettingsState{}, err
	}
	if expectedID == "" || expectedID != e.retrievalSettings.ID() {
		return RetrievalSettingsState{}, workspace.ErrConflict
	}
	if err := e.saveRetrieval(settings); err != nil {
		return RetrievalSettingsState{}, err
	}
	return RetrievalSettingsState{Settings: settings, ID: settings.ID()}, nil
}

// Selection fields are optional request preferences. Required owner scopes are
// always complete, validated ContainerPolicies with a content-derived identity.
type ContainerSelection struct {
	NotesOnly  bool   `json:"-"`
	Archives   string `json:"archives,omitempty"`
	Drafts     string `json:"drafts,omitempty"`
	Collection string `json:"collection,omitempty"`
}

// ResolveContentScope rereads retrieval settings first: they decide what search
// and AI may disclose, so a scope is never resolved from settings another
// gateway has replaced.
func (e *Engine) ResolveContentScope(selection ContainerSelection, ai bool) (workspace.ContentScope, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.refreshRetrieval(context.Background()); err != nil {
		return workspace.ContentScope{}, err
	}
	return e.resolveContentScope(selection, ai)
}
func (e *Engine) resolveContentScope(selection ContainerSelection, ai bool) (workspace.ContentScope, error) {
	policy := e.retrievalSettings.Search
	if ai {
		policy = e.retrievalSettings.AI
	}
	if selection.Archives != "" {
		policy.Archives = selection.Archives
	}
	if selection.Drafts != "" {
		policy.Drafts = selection.Drafts
	}
	scope := workspace.ContentScope{NotesOnly: selection.NotesOnly, Policy: policy, Collection: selection.Collection}
	if scope.Collection == "" {
		scope.Collection = "all"
	}
	return scope, scope.Validate()
}
func (e *Engine) Find(ctx context.Context, query, tag, status, path string, includeTrash bool, limit int, selection ContainerSelection) (workspace.ScopedEntries, error) {
	scope, err := e.ResolveContentScope(selection, false)
	if err != nil {
		return workspace.ScopedEntries{}, err
	}
	return e.Store.FindScoped(ctx, query, tag, status, path, includeTrash, limit, scope)
}

func (e *Engine) resolveSearch(query SearchQuery) (SearchQuery, error) {
	scope, err := e.ResolveContentScope(query.ContainerSelection, false)
	query.contentScope = scope
	return query, err
}
func (e *Engine) validateSearchSnapshot(ctx context.Context, snapshot workspace.Snapshot, scope workspace.ContentScope) error {
	if snapshot.ScopeID != scope.ID() || snapshot.Visibility == "" {
		return errors.Join(workspace.ErrOwnerUnavailable, errors.New("owner cannot validate search scope"))
	}
	state, err := e.Store.Status(ctx)
	if err != nil {
		return err
	}
	visibility, err := e.Store.HistoryVisibilityID(ctx)
	if err != nil {
		return err
	}
	if state.Revision != snapshot.Revision || visibility != snapshot.Visibility {
		return errors.Join(workspace.ErrConflict, errors.New("notes or visibility changed during search; retry"))
	}
	return nil
}
