package gitexport

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/procgroup"
)

// gitConfig is the configuration of every Git command this package runs: no
// hooks, no automatic maintenance, no network transport, durable writes, and
// packing bounded for a shared server.
var gitConfig = []string{
	"-c", "core.hooksPath=/dev/null", "-c", "gc.auto=0", "-c", "maintenance.auto=false",
	"-c", "core.fsync=committed,reference", "-c", "core.fsyncMethod=fsync",
	"-c", "core.bigFileThreshold=1m", "-c", "core.deltaBaseCacheLimit=32m", "-c", "pack.threads=1",
	"-c", "fetch.fsckObjects=true", "-c", "protocol.allow=never", "-c", "protocol.file.allow=always",
}

// gitCommand runs Git in dir, a repository's Git directory, without user or
// system configuration, prompts, replacement objects or repository discovery.
func gitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := procgroup.Command(ctx, "git", append(append([]string{}, gitConfig...), args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_DIR=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "LC_ALL=C"}
	return cmd
}

// A gitError is a failed Git command with the end of what it printed.
type gitError struct {
	args   []string
	err    error
	stderr string
}

func (e *gitError) Error() string {
	msg := "git " + strings.Join(e.args, " ") + ": " + e.err.Error()
	if e.stderr != "" {
		msg += ": " + e.stderr
	}
	return msg
}

func (e *gitError) Unwrap() error { return e.err }

// runGit runs Git in dir within timeout and returns its output, at most limit
// bytes of it, without the final newline.
func runGit(ctx context.Context, dir string, timeout time.Duration, limit int, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := gitCommand(ctx, dir, args...)
	out := &procgroup.Output{Limit: limit}
	errOut := &tail{limit: 4 << 10}
	cmd.Stdout, cmd.Stderr = out, errOut
	if err := cmd.Run(); err != nil {
		if out.Exceeded {
			err = procgroup.ErrLimit
		} else if ctx.Err() != nil {
			err = fmt.Errorf("%w: %w", err, ctx.Err())
		}
		return "", &gitError{args: args, err: err, stderr: strings.TrimSpace(errOut.String())}
	}
	return strings.TrimSuffix(out.String(), "\n"), nil
}

// streamGit runs Git in dir within timeout and calls fn with each line it
// prints, so output of any length is never held whole.
func streamGit(ctx context.Context, dir string, timeout time.Duration, fn func(line string) error, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := gitCommand(ctx, dir, args...)
	errOut := &tail{limit: 4 << 10}
	cmd.Stderr = errOut
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	lines := bufio.NewScanner(pipe)
	lines.Buffer(make([]byte, 64<<10), 1<<20)
	var fnErr error
	for fnErr == nil && lines.Scan() {
		fnErr = fn(lines.Text())
	}
	if fnErr == nil {
		fnErr = lines.Err()
	}
	if fnErr != nil {
		cancel()
	}
	if err := cmd.Wait(); err != nil && fnErr == nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w: %w", err, ctx.Err())
		}
		return &gitError{args: args, err: err, stderr: strings.TrimSpace(errOut.String())}
	}
	return fnErr
}

// streamCommits runs git log in dir within timeout and calls fn with each
// commit rev reaches, parents first in rev-list --reverse --topo-order order,
// and the renames Git detects in it against its first parent, as the Git
// backend's history found them: added path to deleted path. Paths are read
// NUL-separated, so any name passes unchanged.
func streamCommits(ctx context.Context, dir string, timeout time.Duration, rev gitobj.ID, fn func(gitobj.ID, map[string]string) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{"log", "--reverse", "--topo-order", "--no-abbrev", "-z", "--format=%H", "--name-status", "--find-renames",
		"--diff-merges=first-parent", "--no-color", "--no-ext-diff", "--no-show-signature", rev.String(), "--"}
	cmd := gitCommand(ctx, dir, args...)
	errOut := &tail{limit: 4 << 10}
	cmd.Stderr = errOut
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	fields := bufio.NewScanner(pipe)
	fields.Buffer(make([]byte, 64<<10), 1<<20)
	fields.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, 0); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return 0, nil, errors.New("git log output ends without a NUL")
		}
		return 0, nil, nil
	})
	var fnErr error
	var commit gitobj.ID
	var renames map[string]string
	flush := func() error {
		if renames == nil {
			return nil
		}
		return fn(commit, renames)
	}
	for fnErr == nil && fields.Scan() {
		// A commit's ID, or the status of one of its changes, which a
		// newline separates from the line before; then that change's paths.
		item := strings.TrimLeft(fields.Text(), "\n")
		if len(item) == 2*len(gitobj.ID{}) {
			if fnErr = flush(); fnErr == nil {
				commit, fnErr = gitobj.ParseID(item)
				renames = map[string]string{}
			}
			continue
		}
		if renames == nil || item == "" || !strings.ContainsRune("ACDMRTUXB", rune(item[0])) {
			fnErr = fmt.Errorf("unexpected git log output %q", item)
			break
		}
		var paths []string
		for range 1 + strings.Count("RC", item[:1]) {
			if !fields.Scan() {
				break
			}
			paths = append(paths, fields.Text())
		}
		if item[0] == 'R' && len(paths) == 2 {
			renames[paths[1]] = paths[0]
		}
	}
	if fnErr == nil {
		if fnErr = fields.Err(); fnErr == nil {
			fnErr = flush()
		}
	}
	if fnErr != nil {
		cancel()
	}
	if err := cmd.Wait(); err != nil && fnErr == nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w: %w", err, ctx.Err())
		}
		return &gitError{args: args, err: err, stderr: strings.TrimSpace(errOut.String())}
	}
	return fnErr
}

// tail keeps the last bytes written to it, for error messages.
type tail struct {
	bytes.Buffer
	limit int
}

func (t *tail) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > t.limit {
		p = p[len(p)-t.limit:]
	}
	if over := t.Len() + len(p) - t.limit; over > 0 {
		t.Next(over)
	}
	t.Buffer.Write(p)
	return n, nil
}

// revParse resolves name to a commit ID in dir.
func revParse(ctx context.Context, dir, name string) (gitobj.ID, error) {
	out, err := runGit(ctx, dir, time.Minute, 4<<10, "rev-parse", "--verify", "--end-of-options", name+"^{commit}")
	if err != nil {
		return gitobj.ID{}, err
	}
	return gitobj.ParseID(out)
}

// catFile reads objects from one long-running git cat-file --batch.
type catFile struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	limit  int64
	cancel context.CancelFunc
	failed error
}

func openCatFile(ctx context.Context, dir string, limit int64) (*catFile, error) {
	ctx, cancel := context.WithCancel(ctx)
	cmd := gitCommand(ctx, dir, "cat-file", "--batch")
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	return &catFile{cmd: cmd, in: in, out: bufio.NewReaderSize(out, 64<<10), limit: limit, cancel: cancel}, nil
}

// read returns the body of object id, which must be of kind.
func (c *catFile) read(id gitobj.ID, kind string) ([]byte, error) {
	if c.failed != nil {
		return nil, c.failed
	}
	body, err := c.request(id, kind)
	var missing *objectError
	if err != nil && !errors.As(err, &missing) {
		// The stream is out of step; nothing after this can be trusted.
		c.failed = err
		c.cancel()
	}
	return body, err
}

// An objectError is an object the repository lacks or holds as another kind.
type objectError struct {
	id   gitobj.ID
	want string
	got  string
}

func (e *objectError) Error() string {
	if e.got == "missing" {
		return fmt.Sprintf("object %s is missing", e.id)
	}
	return fmt.Sprintf("object %s is a %s, not a %s", e.id, e.got, e.want)
}

func (c *catFile) request(id gitobj.ID, kind string) ([]byte, error) {
	if _, err := io.WriteString(c.in, id.String()+"\n"); err != nil {
		return nil, err
	}
	header, err := c.out.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("git cat-file: %w", err)
	}
	fields := strings.Fields(header)
	if len(fields) == 2 && fields[0] == id.String() && fields[1] == "missing" {
		return nil, &objectError{id: id, want: kind, got: "missing"}
	}
	if len(fields) != 3 || fields[0] != id.String() {
		return nil, fmt.Errorf("git cat-file: unexpected reply %q", strings.TrimSpace(header))
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 || size > c.limit {
		return nil, fmt.Errorf("object %s is larger than this import admits", id)
	}
	body := make([]byte, size+1)
	if _, err := io.ReadFull(c.out, body); err != nil {
		return nil, fmt.Errorf("git cat-file: %w", err)
	}
	if body[size] != '\n' {
		return nil, errors.New("git cat-file: reply is not terminated")
	}
	if fields[1] != kind {
		return nil, &objectError{id: id, want: kind, got: fields[1]}
	}
	return body[:size], nil
}

func (c *catFile) Close() error {
	c.in.Close()
	if c.failed != nil {
		c.cmd.Wait()
		return nil
	}
	err := c.cmd.Wait()
	c.cancel()
	return err
}
