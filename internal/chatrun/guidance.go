package chatrun

import (
	"context"
	"encoding/json"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
)

// chatInstructions preserves the complete authorized guidance at the evidence
// snapshot. Callers include it in their normal context budget without truncation.
func (s *Service) chatInstructions(ctx context.Context, store *workspace.Store, revision string) (map[string]any, string, error) {
	guidance, err := s.hooks.InstructionsAt(ctx, store, revision)
	if err != nil {
		return nil, "", err
	}
	// Presence and exact content identify guidance across unrelated notebook
	// commits; the payload separately records its pinned snapshot revision.
	identity, err := json.Marshal(struct {
		Path    any
		Exists  any
		Content any
	}{guidance["path"], guidance["exists"], guidance["content"]})
	if err != nil {
		return nil, "", err
	}
	return guidance, chat.Hash(identity), nil
}

func (s *Service) AssistantInstructions(ctx context.Context, spec automation.Spec) (map[string]any, string, error) {
	store, err := s.hooks.ClientAccess(ctx, spec.Actor, spec.Project, "read")
	if err != nil {
		return nil, "", err
	}
	return s.chatInstructions(ctx, store, "")
}
