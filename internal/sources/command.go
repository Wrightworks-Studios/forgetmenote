package sources

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"forgetmenote/internal/procgroup"
	"forgetmenote/internal/sandbox"
	"forgetmenote/internal/workspace"
)

// extractorTimeout bounds one local tool invocation. The kernel CPU limit sits
// below it so a parser spinning in a loop is stopped even if it ignores signals.
const extractorTimeout = 60 * time.Second

// Poppler's pdftotext fits comfortably in this envelope for 64 MiB originals.
// The process limit counts every task of the server's user ID on the host, so
// it is a fork-bomb guard rather than a tight budget.
var extractorLimits = sandbox.Policy{Memory: 1 << 30, CPU: 50, Processes: 4096, FileSize: 64 << 20}

// Local extractors receive no application credentials. Each runs through the
// sandbox helper: resource limits, and on Linux a Landlock domain in which its
// scratch directory (holding the input it converts and any output files) is the
// only readable or writable data. A canceled extraction must drain its complete
// process group, including children holding output pipes.
func localCommand(ctx context.Context, dir, name string, args ...string) *exec.Cmd {
	home, scratch := dir, dir
	if dir == "" {
		home, scratch = os.DevNull, os.TempDir()
	}
	policy := extractorLimits
	scratch, absErr := filepath.Abs(scratch)
	policy.Write = []string{scratch}
	self, err := os.Executable()
	cmd := procgroup.Command(ctx, self, policy.Args(name, args...)...)
	if err = errors.Join(err, absErr); err != nil {
		cmd.Err = err
	}
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + scratch,
		"FMN_LIMIT_SOURCE_BYTES=" + strconv.Itoa(workspace.MaxSourceBytes()),
		"PYTHONDONTWRITEBYTECODE=1"}
	cmd.Stderr = &sandboxNotice{}
	return cmd
}

var sandboxNoticeOnce sync.Once

// sandboxNotice discards extractor stderr, except the helper's own first line
// (for example, Landlock unavailable under a seccomp profile), which is logged
// once per process so operators can see that only resource limits apply.
type sandboxNotice struct {
	line []byte
	done bool
}

func (n *sandboxNotice) Write(p []byte) (int, error) {
	if !n.done {
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			end = len(p)
		}
		n.line = append(n.line, p[:min(end, 512-len(n.line))]...)
		if end < len(p) || len(n.line) >= 512 {
			n.done = true
			if text, ok := strings.CutPrefix(string(n.line), sandbox.NoticePrefix); ok {
				sandboxNoticeOnce.Do(func() { slog.Warn("local extractor sandbox", "notice", text) })
			}
		}
	}
	return len(p), nil
}
