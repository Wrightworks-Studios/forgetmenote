package web

import (
	"context"
	"errors"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/workspace"
	"log/slog"
)

// Tool names are registered server-side. Arguments, results and raw errors can
// contain company or personal data and never belong in operational logs.
func chatToolDiagnostic(run, name string, err error) {
	outcome := "succeeded"
	category := "completed"
	if err != nil {
		outcome = "failed"
		switch {
		case errors.Is(err, connectors.ErrSchemaChanged):
			category = "schema_changed"
		case errors.Is(err, workspace.ErrForbidden), errors.Is(err, chat.ErrOutsideCorpus):
			category = "access_denied"
		case errors.Is(err, connectors.ErrQuery):
			category = "query_rejected"
		case errors.Is(err, context.DeadlineExceeded):
			category = "deadline"
		case errors.Is(err, context.Canceled):
			category = "cancelled"
		default:
			category = "operation_failed"
		}
	}
	slog.Info("chat tool outcome", "run", run, "tool", name, "outcome", outcome, "category", category)
}
