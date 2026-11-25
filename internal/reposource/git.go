package reposource

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/procgroup"
)

type cache struct {
	dir       string
	limits    Limits
	remoteURL func(Repository) string
	allowFile bool // Only synthetic tests supply a local upstream.
	// blobs holds the source's durable files under key, the source directory's
	// path in the data directory. It is nil when dir's own files are those.
	blobs blob.Store
	key   string
	// evidence lists the source's stored evidence keys.
	evidence func(context.Context) (map[string]bool, error)
	// fetch makes the mirror hold the commit a read's source state names.
	fetch func(context.Context) error
}

// mirror returns the Git mirror's directory, holding the source's commit.
func (c *cache) mirror(ctx context.Context) (string, error) {
	if c.fetch != nil {
		if err := c.fetch(ctx); err != nil {
			return "", err
		}
		c.fetch = nil
	}
	return filepath.Join(c.dir, "source.git"), nil
}

func (c *cache) refresh(ctx context.Context, repo Repository, sha, token string) error {
	if !validRepo(repo) || !revision.MatchString(sha) {
		return ErrDenied
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.limits.Seconds)*time.Second)
	defer cancel()
	if err := os.MkdirAll(c.dir, 0700); err != nil {
		return err
	}
	current, old := filepath.Join(c.dir, "source.git"), filepath.Join(c.dir, "source.old")
	// A crash during promotion leaves either the complete new cache or the old
	// cache. Neither a partial fetch nor a mixture of revisions becomes visible.
	if _, err := os.Stat(current); errors.Is(err, os.ErrNotExist) {
		if _, err = os.Stat(old); err == nil {
			if err = os.Rename(old, current); err != nil {
				return err
			}
		}
	}
	if err := os.RemoveAll(old); err != nil {
		return err
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "fetch-") {
			if err = os.RemoveAll(filepath.Join(c.dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	stage, err := os.MkdirTemp(c.dir, "fetch-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err = os.WriteFile(filepath.Join(c.dir, "askpass"), []byte("#!/bin/sh\ncase \"$1\" in\n *Username*) printf '%s\\n' x-access-token ;;\n *Password*) printf '%s\\n' \"$FMN_SOURCE_TOKEN\" ;;\n *) exit 1 ;;\nesac\n"), 0700); err != nil {
		return err
	}
	if _, err = c.git(ctx, stage, "", 65536, "init", "--bare"); err != nil {
		return err
	}
	remote := "https://github.com/" + repo.FullName + ".git"
	if c.allowFile && c.remoteURL != nil {
		remote = c.remoteURL(repo)
	}
	priorBytes, err := diskBytes(current)
	if err != nil {
		return err
	}
	var exceeded atomic.Bool
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				n, e := diskBytes(stage)
				if e != nil || n+priorBytes > c.limits.CacheBytes {
					exceeded.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	_, err = c.git(ctx, stage, token, 65536, "fetch", "--depth="+strconv.Itoa(c.limits.History), "--no-tags", "--no-recurse-submodules", remote, sha)
	close(done)
	<-stopped
	if exceeded.Load() {
		return ErrLimit
	}
	if err != nil {
		return err
	}
	n, err := diskBytes(stage)
	if err != nil {
		return err
	}
	if n+priorBytes > c.limits.CacheBytes {
		return ErrLimit
	}
	got, err := c.git(ctx, stage, "", 128, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(got)) != sha {
		return ErrConflict
	}
	if _, err = c.git(ctx, stage, "", 128, "update-ref", "refs/heads/source", sha); err != nil {
		return err
	}
	if _, err = c.git(ctx, stage, "", 128, "symbolic-ref", "HEAD", "refs/heads/source"); err != nil {
		return err
	}
	if _, err = os.Stat(current); err == nil {
		if err = os.Rename(current, old); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = os.Rename(stage, current); err != nil {
		_ = os.Rename(old, current)
		return err
	}
	return os.RemoveAll(old)
}

func (c *cache) git(ctx context.Context, dir, token string, limit int, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.limits.Seconds)*time.Second)
	defer cancel()
	cmd := c.command(ctx, dir, token, args...)
	stdout := &procgroup.Output{Limit: limit, Err: ErrLimit}
	stderr := &procgroup.Output{Limit: 65536, Err: ErrLimit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if stdout.Exceeded || stderr.Exceeded {
			return nil, ErrLimit
		}
		return nil, errors.New("source Git operation failed")
	}
	return stdout.Bytes(), nil
}

func diskBytes(dir string) (int64, error) {
	var size int64
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return ErrDenied
		}
		if !d.IsDir() {
			st, e := d.Info()
			if e != nil {
				return e
			}
			if !st.Mode().IsRegular() {
				return ErrDenied
			}
			size += st.Size()
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	return size, err
}

func safePath(p string) bool {
	return p != "" && len(p) <= 1024 && !strings.HasPrefix(p, "/") && !strings.ContainsAny(p, "\x00\r\n\\") && p != "." && p != ".." && !strings.HasPrefix(p, "../") && path.Clean(p) == p
}

func regularRead(file string, limit int64) ([]byte, error) {
	st, err := os.Lstat(file)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, ErrLimit
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(b)) > limit {
		return nil, ErrLimit
	}
	return b, err
}

func (c *cache) command(ctx context.Context, dir, token string, args ...string) *exec.Cmd {
	base := []string{"-c", "core.hooksPath=/dev/null", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "credential.helper=", "-c", "http.followRedirects=false", "-c", "http.proxy=", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "--git-dir=" + dir}
	if c.allowFile {
		base = append([]string{"-c", "protocol.file.allow=always"}, base...)
	}
	// Fetch spawns transport and pack helpers. Cancel the whole process group so
	// a timed-out fetch cannot keep writing after its staging directory is removed.
	cmd := procgroup.Command(ctx, "git", append(base, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=" + filepath.Join(c.dir, "askpass"), "FMN_SOURCE_TOKEN=" + token, "LC_ALL=C"}
	return cmd
}
