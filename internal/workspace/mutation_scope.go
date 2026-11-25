package workspace

import (
	"context"
)

type explicitCheckpointKey struct{}

// Whole-file editor clients opt out of the legacy automatic checkpoint on
// writes. The write checks it under the notebook's lock.
func WithExplicitCheckpoint(ctx context.Context) context.Context {
	return context.WithValue(ctx, explicitCheckpointKey{}, true)
}
func explicitCheckpoint(ctx context.Context) bool {
	required, _ := ctx.Value(explicitCheckpointKey{}).(bool)
	return required
}

// MutationScope is required owner-side authority attached only by integrated chat.
// Ordinary browser and MCP requests carry no scope.
type MutationSource struct {
	Path       string `json:"path"`
	Revision   string `json:"revision"`
	Hash       string `json:"hash"`
	Historical bool   `json:"historical"`
}
type MutationScope struct {
	Sources []MutationSource `json:"sources,omitempty"`
	Version int              `json:"version"`
	Content ContentScope     `json:"content"`
	Control string           `json:"control,omitempty"`
	Target  string           `json:"target,omitempty"`
}
type mutationScopeKey struct{}

func WithMutationScope(ctx context.Context, scope MutationScope) context.Context {
	return context.WithValue(ctx, mutationScopeKey{}, scope)
}
func mutationScopeFrom(ctx context.Context) *MutationScope {
	scope, ok := ctx.Value(mutationScopeKey{}).(MutationScope)
	if !ok {
		return nil
	}
	return &scope
}

func (m MutationScope) Validate() error {
	if m.Version != 1 {
		return ErrForbidden
	}
	if err := m.Content.Validate(); err != nil {
		return err
	}
	if len(m.Sources) > 12 {
		return ErrForbidden
	}
	for _, source := range m.Sources {
		if source.Path == "" || source.Revision == "" || source.Hash == "" {
			return ErrForbidden
		}
	}
	switch m.Control {
	case "", "set_note_container", "set_project_instructions", "trash_notes", "restore_trash", "empty_trash", "move_notes", "duplicate_notes", "archive_source", "upload_attachment", "set_note_lock":
	default:
		return ErrForbidden
	}
	return nil
}

func (m MutationScope) checkContent(content []byte) error {
	metadata, err := ParseMetadata(content)
	if err != nil || m.Content.Tier(metadata) < 0 {
		return ErrForbidden
	}
	return nil
}
