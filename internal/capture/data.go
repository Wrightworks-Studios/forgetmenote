package capture

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/procgroup"
)

// The data directory's files that belong in a capture besides the exports:
// the recovery key, the trusted proxy's secret and the instance's style.
var secretFiles = []string{"admin.token", "proxy.secret", "custom.css"}

// receiptSpool is where a gateway keeps the receipts of finished purges the
// database refused until it can keep them there (internal/purgehttp).
const receiptSpool = "purge-receipts"

var exportName = regexp.MustCompile(`^([a-z0-9][a-z0-9-]{0,63})\.git$`)

// copyData copies the data directory's files into data/: the secret files,
// spooled purge receipts and every notebook's Git export.
func (c *capture) copyData(ctx context.Context) error {
	for _, name := range secretFiles {
		copied, err := copyFile(filepath.Join(c.o.Data, name), filepath.Join(c.out, "data", name))
		if err != nil {
			return err
		}
		if copied {
			c.included = append(c.included, "data/"+name)
		}
	}
	entries, err := os.ReadDir(filepath.Join(c.o.Data, receiptSpool))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if len(entries) > 0 {
		if err := os.Mkdir(filepath.Join(c.out, "data", receiptSpool), 0o700); err != nil {
			return err
		}
		c.included = append(c.included, "data/"+receiptSpool)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") && !strings.HasPrefix(entry.Name(), ".") {
			if _, err := copyFile(filepath.Join(c.o.Data, receiptSpool, entry.Name()), filepath.Join(c.out, "data", receiptSpool, entry.Name())); err != nil {
				return err
			}
		}
	}
	entries, err = os.ReadDir(filepath.Join(c.o.Data, "exports"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(c.out, "data", "exports"), 0o700); err != nil {
		return err
	}
	for _, entry := range entries {
		match := exportName.FindStringSubmatch(entry.Name())
		if match == nil || !entry.IsDir() {
			continue // bundle objects of a blob store at the data directory, the exporter's lock
		}
		copied, err := c.copyExport(ctx, entry.Name())
		if err != nil {
			return err
		}
		if copied {
			c.exports = append(c.exports, match[1])
			c.included = append(c.included, "data/exports/"+entry.Name())
		}
	}
	return nil
}

// exportAttempts bounds how often an export that changed while it was copied
// is copied again.
const exportAttempts = 5

// errGone is an export removed while it was being copied.
var errGone = errors.New("export removed")

// copyExport copies the export repository name and checks the copy, again
// while maintenance or a purge's rebuild changes it meanwhile. It reports
// false for an export the exporter removed, with its notebook.
//
// The exporter writes an object before the branch names it, and a repack
// writes its new pack before it deletes the packs and loose objects it
// replaces. So the branch is copied first, then the objects: what the branch
// reaches was stored when it was read, and is copied unless a repack moved
// it meanwhile. Each file is read through one open descriptor, so a file
// replaced while it is copied is copied whole as it was; one removed before
// it is opened is skipped. Then Git, in the copy only, packs what the branch
// reaches and drops everything else, and fsck checks that nothing the branch
// reaches is missing: a copy that raced a repack fails that check and is
// made again. Dropping what the branch does not reach also drops what a
// purge's rebuild was removing during the copy. The exports themselves are
// only read, and no Git runs in them.
func (c *capture) copyExport(ctx context.Context, name string) (bool, error) {
	src, dst := filepath.Join(c.o.Data, "exports", name), filepath.Join(c.out, "data", "exports", name)
	var last error
	for attempt := 1; attempt <= exportAttempts; attempt++ {
		if err := os.RemoveAll(dst); err != nil {
			return false, err
		}
		err := copyRepository(src, dst)
		if errors.Is(err, errGone) {
			return false, os.RemoveAll(dst)
		}
		if err == nil {
			err = packCopy(ctx, dst)
		}
		if err == nil {
			return true, private(dst)
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		last = err
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
		}
	}
	return false, fmt.Errorf("the Git export %s changed during every copy: %w", name, last)
}

// copyRepository copies the repository src to the new dst: its branch first,
// then its other files, then its objects.
func copyRepository(src, dst string) error {
	info, err := os.Lstat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return errGone
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", src)
	}
	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}
	if copied, err := copyFile(filepath.Join(src, "HEAD"), filepath.Join(dst, "HEAD")); err != nil || !copied {
		if err == nil {
			err = errGone // the exporter renamed it away to remove it
		}
		return err
	}
	for _, rel := range []string{"refs", "packed-refs"} {
		if err := copyTree(src, dst, rel); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "HEAD", "refs", "packed-refs", "objects":
			continue
		}
		if err := copyTree(src, dst, entry.Name()); err != nil {
			return err
		}
	}
	return copyTree(src, dst, "objects")
}

// skipped reports whether the repository file rel is left out of a copy:
// temporaries and locks of a write in progress, hooks and logs, and what
// would let Git in the copy read objects it lacks (alternates) or trust
// accelerators naming packs it lacks (commit graphs, multi-pack indexes),
// which Git does without.
func skipped(rel string) bool {
	base := path.Base(rel)
	if strings.HasPrefix(base, ".") || strings.HasPrefix(base, "tmp_") || strings.HasSuffix(base, ".lock") || strings.HasSuffix(base, ".tmp") || strings.HasPrefix(base, "multi-pack-index") {
		return true
	}
	switch rel {
	case "hooks", "logs", "FETCH_HEAD", "ORIG_HEAD", "gc.pid", "gc.log",
		"objects/info/alternates", "objects/info/http-alternates", "objects/info/commit-graph", "objects/info/commit-graphs":
		return true
	}
	return false
}

// copyTree copies the regular files and directories under src/rel to
// dst/rel. What is removed meanwhile is skipped; symbolic links and other
// files are left out.
func copyTree(src, dst, rel string) error {
	root := filepath.Join(src, filepath.FromSlash(rel))
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		inner, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		inner = filepath.ToSlash(inner)
		if skipped(inner) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, filepath.FromSlash(inner))
		switch {
		case d.IsDir():
			if err := os.Mkdir(target, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
		case d.Type().IsRegular():
			_, err := copyFile(p, target)
			return err
		}
		return nil
	})
}

// copyFile copies the regular file src to the new private file dst, through
// one descriptor. It reports false, and creates nothing, when src does not
// exist.
func copyFile(src, dst string) (bool, error) {
	in, err := os.Open(src)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", src)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	_, err = io.Copy(out, in)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return err == nil, err
}

// private makes every file under dir 0600 and every directory 0700, as Git
// in the copy leaves its packs read-only and its directories open.
func private(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if d.IsDir() {
			mode = 0o700
		}
		return os.Chmod(p, mode)
	})
}

// gitTimeout bounds each Git command on a copy, as the exporter bounds its
// repacks and checks.
const gitTimeout = 15 * time.Minute

// packCopy keeps in the repository copy dir only what its branch reaches,
// in one pack, and checks that nothing it reaches is missing.
func packCopy(ctx context.Context, dir string) error {
	for _, args := range [][]string{
		{"repack", "-a", "-d", "-q"},
		{"prune", "--expire=now"},
		{"fsck", "--connectivity-only", "--no-dangling", "--no-reflogs", "--no-progress"},
	} {
		if err := runGit(ctx, dir, args...); err != nil {
			return err
		}
	}
	return nil
}

// runGit runs Git in the repository copy dir without user or system
// configuration, hooks, prompts, replacement objects or automatic
// maintenance.
func runGit(ctx context.Context, dir string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	config := []string{"-c", "core.hooksPath=/dev/null", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "core.commitGraph=false",
		"-c", "core.multiPackIndex=false", "-c", "pack.threads=1", "-c", "protocol.allow=never"}
	cmd := procgroup.Command(ctx, "git", append(config, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_DIR=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "LC_ALL=C"}
	errOut := &procgroup.Output{Limit: 4 << 10}
	cmd.Stdout, cmd.Stderr = io.Discard, errOut
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if ctx.Err() != nil || !errors.As(err, &exit) {
			return fmt.Errorf("git %s: %w", args[0], errors.Join(err, ctx.Err()))
		}
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errOut.String()))
	}
	return nil
}

// branch returns the commit the repository at dir's main branch names, or
// the zero ID before its first export.
func branch(dir string) (gitobj.ID, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "refs", "heads", "main"))
	if err == nil {
		return gitobj.ParseID(strings.TrimSpace(string(raw)))
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return gitobj.ID{}, err
	}
	f, err := os.Open(filepath.Join(dir, "packed-refs"))
	if errors.Is(err, fs.ErrNotExist) {
		return gitobj.ID{}, nil
	}
	if err != nil {
		return gitobj.ID{}, err
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		if sha, name, ok := strings.Cut(lines.Text(), " "); ok && name == "refs/heads/main" {
			return gitobj.ParseID(sha)
		}
	}
	return gitobj.ID{}, lines.Err()
}

// checkExports drops each copied export whose branch names no revision of
// its notebook in the snapshot tx. Copied before the snapshot, an export is
// never ahead of it; it can name a commit the snapshot lacks when a purge
// rewrote the notebook's history before the export was rebuilt for it, when
// the notebook was deleted, or when the export diverged. Such a copy may
// hold content a purge in the snapshot erased, so it is left out, and the
// restored instance writes the export again.
func (c *capture) checkExports(ctx context.Context, tx pgx.Tx) error {
	for _, id := range c.exports {
		rel := "data/exports/" + id + ".git"
		dir := filepath.Join(c.out, filepath.FromSlash(rel))
		head, err := branch(dir)
		if err != nil {
			return fmt.Errorf("the copy of %s: %w", rel, err)
		}
		if head.IsZero() {
			continue
		}
		var held bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM revisions WHERE org = $1 AND notebook = $2 AND commit_sha = $3)`, c.o.DB.Org, id, head[:]).Scan(&held); err != nil {
			return err
		}
		if held {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		c.included = slices.DeleteFunc(c.included, func(path string) bool { return path == rel })
		c.excluded = append(c.excluded, Exclusion{rel, "Its branch names a commit that is not one of the notebook's revisions in the database snapshot: a purge rewrote it before the export was rebuilt, the notebook was deleted, or the export diverged. The restored instance writes the export again; a purge flag it held is not kept, so a remote at the old commit stops as diverged, for review."})
	}
	return nil
}
