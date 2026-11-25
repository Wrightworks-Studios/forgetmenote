//go:build fmn_omit_redshift

package connectors

import (
	"context"

	"forgetmenote/internal/integrations"
)

// New refuses a redshift connection in this build, so these never run.
type redshiftClient struct{}

func (r *runtime) configureRedshift() error { return integrations.Check("redshift") }
func (r *runtime) redshiftCall(context.Context, string, map[string]any) (any, bool, error) {
	return nil, false, integrations.Check("redshift")
}
