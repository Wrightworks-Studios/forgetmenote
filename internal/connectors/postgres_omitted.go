//go:build fmn_omit_postgres

package connectors

import (
	"context"

	"forgetmenote/internal/integrations"
)

// New refuses a postgres connection in this build, so this never runs.
func (r *runtime) postgresCall(context.Context, string, map[string]any, string) (any, bool, error) {
	return nil, false, integrations.Check("postgres")
}
