package benchfixture

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// GitResult describes a materialized repository.
type GitResult struct {
	Head      string
	Revisions int
}

// gitCommand runs Git without user or system configuration, hooks or signing.
func gitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	base := []string{"-C", dir, "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "core.autocrlf=false", "-c", "core.quotePath=false"}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	return cmd
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitCommand(ctx, dir, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// WriteGit replays revs into a new repository at dir with git fast-import, on
// branch main, then checks out the working tree unless bare. It fails if Git
// assigns any commit an ID other than the revision's predicted Commit, so a
// successful result is also proof that the predictions are exact.
func WriteGit(ctx context.Context, dir string, revs iter.Seq[Revision], bare bool) (GitResult, error) {
	dir, err := filepath.Abs(dir) // git -C would resolve relative paths again
	if err != nil {
		return GitResult{}, err
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return GitResult{}, fmt.Errorf("benchfixture: %s is not empty", dir)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return GitResult{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return GitResult{}, err
	}
	initArgs := []string{"init", "--quiet", "--initial-branch=main"}
	if bare {
		initArgs = append(initArgs, "--bare")
	}
	if _, err := runGit(ctx, dir, initArgs...); err != nil {
		return GitResult{}, err
	}
	marks := filepath.Join(dir, ".git", "fixture-marks")
	if bare {
		marks = filepath.Join(dir, "fixture-marks")
	}
	cmd := gitCommand(ctx, dir, "fast-import", "--quiet", "--done", "--date-format=raw", "--export-marks="+marks)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return GitResult{}, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		return GitResult{}, err
	}
	expected, streamErr := writeFastImport(stdin, revs)
	closeErr := stdin.Close()
	if waitErr := cmd.Wait(); waitErr != nil {
		return GitResult{}, fmt.Errorf("git fast-import: %w: %s", errors.Join(waitErr, streamErr), stderr.String())
	}
	if err = errors.Join(streamErr, closeErr); err != nil {
		return GitResult{}, err
	}
	raw, err := os.ReadFile(marks)
	if err != nil {
		return GitResult{}, err
	}
	os.Remove(marks)
	got := map[int]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		mark, id, ok := strings.Cut(line, " ")
		n, convErr := strconv.Atoi(strings.TrimPrefix(mark, ":"))
		if !ok || convErr != nil {
			return GitResult{}, fmt.Errorf("benchfixture: unexpected fast-import mark %q", line)
		}
		got[n] = id
	}
	for i, want := range expected {
		if got[i+1] != want {
			return GitResult{}, fmt.Errorf("benchfixture: revision %d: git assigned commit %s, generator predicted %s", i+1, got[i+1], want)
		}
	}
	result := GitResult{Revisions: len(expected)}
	if len(expected) > 0 {
		result.Head = expected[len(expected)-1]
	}
	if !bare && result.Head != "" {
		if _, err = runGit(ctx, dir, "read-tree", "--reset", "-u", "HEAD"); err != nil {
			return result, err
		}
	}
	return result, nil
}

// writeFastImport streams revs as one linear branch and returns the predicted
// commit of each revision in order.
func writeFastImport(w io.Writer, revs iter.Seq[Revision]) ([]string, error) {
	out := bufio.NewWriterSize(w, 1<<20)
	var expected []string
	data := func(b []byte) {
		fmt.Fprintf(out, "data %d\n", len(b))
		out.Write(b)
		out.WriteByte('\n')
	}
	for r := range revs {
		if r.Seq != len(expected)+1 {
			return expected, fmt.Errorf("benchfixture: revision %d out of order", r.Seq)
		}
		date := gitDate(r.Time)
		fmt.Fprintf(out, "commit refs/heads/main\nmark :%d\nauthor %s <%s> %s\ncommitter %s <%s> %s\n", r.Seq, r.AuthorName, r.AuthorEmail, date, CommitterName, CommitterEmail, date)
		data([]byte(r.Message()))
		for _, c := range r.Changes {
			for _, p := range []string{c.Path, c.OldPath} {
				if strings.ContainsAny(p, "\n\"") {
					return expected, fmt.Errorf("benchfixture: unsupported path %q", p)
				}
			}
			if c.OldPath != "" {
				fmt.Fprintf(out, "D %s\n", c.OldPath)
			}
			if c.Content == nil {
				fmt.Fprintf(out, "D %s\n", c.Path)
				continue
			}
			fmt.Fprintf(out, "M 100644 inline %s\n", c.Path)
			data(c.Content)
		}
		out.WriteByte('\n')
		expected = append(expected, r.Commit)
	}
	if _, err := out.WriteString("done\n"); err != nil {
		return expected, err
	}
	return expected, out.Flush()
}

// WriteFiles replays revs and writes the final files into dir, without
// history. dir must be empty or absent.
func WriteFiles(dir string, revs iter.Seq[Revision]) (int, error) {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return 0, fmt.Errorf("benchfixture: %s is not empty", dir)
	}
	// Recent content stays in memory so a note rewritten thousands of times is
	// written once; large binary batches spill to disk as they accumulate.
	pending, pendingBytes := map[string][]byte{}, 0
	flush := func() error {
		paths := slices.Sorted(func(yield func(string) bool) {
			for p := range pending {
				if !yield(p) {
					return
				}
			}
		})
		for _, p := range paths {
			target := filepath.Join(dir, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(target, pending[p], 0o644); err != nil {
				return err
			}
		}
		clear(pending)
		pendingBytes = 0
		return nil
	}
	remove := func(p string) error {
		if content, ok := pending[p]; ok {
			pendingBytes -= len(content)
			delete(pending, p)
		}
		err := os.Remove(filepath.Join(dir, filepath.FromSlash(p)))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	n := 0
	for r := range revs {
		n++
		for _, c := range r.Changes {
			if c.OldPath != "" {
				if err := remove(c.OldPath); err != nil {
					return n, err
				}
			}
			if err := remove(c.Path); err != nil {
				return n, err
			}
			if c.Content != nil {
				pending[c.Path] = c.Content
				pendingBytes += len(c.Content)
			}
		}
		if pendingBytes > 64<<20 {
			if err := flush(); err != nil {
				return n, err
			}
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return n, err
	}
	if err := flush(); err != nil {
		return n, err
	}
	return n, removeEmptyDirs(dir)
}

// removeEmptyDirs drops directories that moves and deletes left empty; Git
// does not track them either.
func removeEmptyDirs(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != root {
			dirs = append(dirs, p)
		}
		return err
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if entries, err := os.ReadDir(dirs[i]); err == nil && len(entries) == 0 {
			if err = os.Remove(dirs[i]); err != nil {
				return err
			}
		}
	}
	return nil
}
