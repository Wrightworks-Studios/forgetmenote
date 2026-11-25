package personal

import (
	"context"

	"forgetmenote/internal/automation"
)

// Web's personal assistant tests use these exports; production code never
// does. TestAssistantTestExportsStayInTests in web fails on any reference to
// them from a non-test file. None of them reads or carries a credential.

// Collect runs one collection pass. Tests only.
func (s *Service) Collect(ctx context.Context, r *automation.Run, state *State, persist func() error) error {
	return s.collectPersonal(ctx, r, state, persist)
}

// MaintainMemory updates the profile memory from recorded interactions. Tests
// only.
func (s *Service) MaintainMemory(ctx context.Context, r *automation.Run) error {
	return s.maintainPersonalMemory(ctx, r)
}

// Notify sends the Slack notification for ready drafts. Tests only.
func (s *Service) Notify(ctx context.Context, r *automation.Run, state *State, persist func() error) error {
	return s.notifyPersonal(ctx, r, state, persist)
}

// PublishDrafts writes ready drafts to the notebook and notifies. Tests only.
func (s *Service) PublishDrafts(ctx context.Context, r *automation.Run, state *State, persist func() error, save func() error) error {
	return s.publishPersonalDrafts(ctx, r, state, persist, save)
}

// Fingerprint returns an observation's key and revision hash. Tests only.
func Fingerprint(o Observation) string { return personalFingerprint(o) }
