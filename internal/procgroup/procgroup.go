// Package procgroup runs local helper programs that may start their own
// children, and bounds the output kept from them.
package procgroup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Command is exec.CommandContext in a new process group. Cancellation kills the
// whole group, so a helper's children cannot keep running or writing; a group
// that already exited is not an error. WaitDelay bounds the wait for output
// pipes a killed descendant still holds open.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// ErrLimit is the default error for output beyond an Output's limit.
var ErrLimit = errors.New("command output exceeds its limit")

// Output keeps at most Limit bytes. A write that would pass the limit is
// refused whole with Err (ErrLimit when nil) and sets Exceeded.
type Output struct {
	bytes.Buffer
	Limit    int
	Err      error
	Exceeded bool
}

func (o *Output) Write(p []byte) (int, error) {
	if len(p) > o.Limit-o.Len() {
		o.Exceeded = true
		if o.Err == nil {
			return 0, ErrLimit
		}
		return 0, o.Err
	}
	return o.Buffer.Write(p)
}
