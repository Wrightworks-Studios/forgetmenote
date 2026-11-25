//go:build fmn_omit_outlook

package sources

import (
	"context"

	"forgetmenote/internal/integrations"
)

// Outlook sources are refused before extraction in this build.
func outlookText(context.Context, []byte) (string, error) {
	return "", refused(integrations.Check("outlook"))
}
