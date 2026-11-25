//go:build fmn_omit_iwork

package sources

import (
	"context"

	"forgetmenote/internal/integrations"
)

// Pages and Keynote sources are refused before extraction in this build.
func extractIWork(context.Context, string, []byte) (string, string, string, error) {
	return "", "", "", refused(integrations.Check("iwork"))
}
