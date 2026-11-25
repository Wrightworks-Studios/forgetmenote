//go:build unix && !linux

package sandbox

import (
	"errors"
	"io"
)

// macOS does not apply RLIMIT_DATA to mmap allocations, so the memory limit
// is Linux-only.
const memoryLimit = false

var errNoLandlock = errors.New("Landlock is Linux-only")

// Available reports why a Strict policy cannot run here.
func Available() error { return errNoLandlock }

// Landlock is Linux-only; the portable resource limits still apply here, and a
// strict policy refuses to run.
func restrict(_ []rule, strict bool, _ io.Writer) error {
	if strict {
		return errors.New("Landlock unavailable (Linux-only); refusing to run without the sandbox")
	}
	return nil
}
