package sources

import (
	"context"
	"os"
)

type jobScratchKey struct{}

// Job conversions inherit a private scratch lifetime from their durable owner.
// Standalone extraction callers still own their temporary directory via defer.
func scratchDirectory(ctx context.Context, prefix string) (string, error) {
	root, _ := ctx.Value(jobScratchKey{}).(string)
	return os.MkdirTemp(root, prefix)
}
