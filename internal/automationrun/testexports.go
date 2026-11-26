package automationrun

import (
	"context"

	"forgetmenote/internal/automation"
)

// Test-only exports: web's server-level tests reach these engine internals,
// and nothing else may. TestAutomationRunTestExportsStayInTests in web fails
// on a reference from production code.

// RunAutomation runs one run as the runtime does, so web's tests can drive a
// run, or the runtime's queue, directly.
func (s *Service) RunAutomation(ctx context.Context, r *automation.Run, save func() error) error {
	return s.runAutomation(ctx, r, save)
}
