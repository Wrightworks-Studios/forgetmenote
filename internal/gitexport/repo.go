package gitexport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"forgetmenote/internal/durable"
	"forgetmenote/internal/gitobj"
)

// Branch is the one branch an export repository has.
const Branch = "refs/heads/main"

// PurgeFlag is the file in an export repository that authorizes the backup
// worker to replace the remote branch once, after a purge rewrote history.
const PurgeFlag = "forgetmenote-purge.json"

// PurgeLock is the file of an export repository whose lock (flock) the
// exporter holds to replace the purge flag and the backup worker holds to
// remove a flag it spent, so the worker never removes a newer purge's flag.
const PurgeLock = "forgetmenote-purge.lock"

var (
	notebookID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	purgeID    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// initRepo creates the bare repository at path, on branch main with no
// commits, unless it exists. It is built beside path and renamed into place,
// so path is either absent or a complete repository.
func initRepo(ctx context.Context, path string) error {
	if info, err := os.Stat(filepath.Join(path, "HEAD")); err == nil && info.Mode().IsRegular() {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s exists but is not a Git repository", path)
	}
	parent := filepath.Dir(path)
	temporary, err := os.MkdirTemp(parent, "."+filepath.Base(path)+".init-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	if _, err = runGit(ctx, temporary, time.Minute, 64<<10, "init", "--quiet", "--bare", "--initial-branch=main", "--template="); err != nil {
		return err
	}
	for _, dir := range []string{"objects", "refs/heads", "refs/tags", "refs", ""} {
		if err = durable.SyncDir(filepath.Join(temporary, dir)); err != nil {
			return err
		}
	}
	if err = os.Chmod(temporary, 0o755); err != nil {
		return err
	}
	if err = os.Rename(temporary, path); err != nil {
		return err
	}
	return durable.SyncDir(parent)
}

// readRef returns the commit refs/heads/main names, or the zero ID before the
// first export. A packed ref counts too, in case someone packed the refs.
func readRef(path string) (gitobj.ID, error) {
	raw, err := os.ReadFile(filepath.Join(path, filepath.FromSlash(Branch)))
	if err == nil {
		return gitobj.ParseID(strings.TrimSpace(string(raw)))
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return gitobj.ID{}, err
	}
	f, err := os.Open(filepath.Join(path, "packed-refs"))
	if errors.Is(err, fs.ErrNotExist) {
		return gitobj.ID{}, nil
	}
	if err != nil {
		return gitobj.ID{}, err
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		if sha, name, ok := strings.Cut(lines.Text(), " "); ok && name == Branch {
			return gitobj.ParseID(sha)
		}
	}
	return gitobj.ID{}, lines.Err()
}

// writeRef points refs/heads/main at id as Git does: through main.lock, which
// Git never reads as a ref, synced and renamed into place.
func writeRef(path string, id gitobj.ID) error {
	ref := filepath.Join(path, filepath.FromSlash(Branch))
	return replaceFile(ref, ref+".lock", []byte(id.String()+"\n"))
}

// replaceFile replaces path with raw through temporary, durably.
func replaceFile(path, temporary string, raw []byte) error {
	f, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporary, path)
	}
	if err != nil {
		os.Remove(temporary)
		return err
	}
	return durable.SyncDir(filepath.Dir(path))
}

// A Purge is the content of an export's purge flag: the purge that rewrote
// the notebook's history, the newest rewritten commit, which carries the
// Gitnotes-Purge trailer naming it, and every commit the rewrite dropped from
// the export. The backup worker replaces the remote branch only when the
// remote is at one of those dropped commits and the revision it pushes
// contains Head; it then records the purge as pushed and removes the flag.
type Purge struct {
	Purge    string      `json:"purge"`
	Head     gitobj.ID   `json:"head"`
	Replaces []gitobj.ID `json:"replaces"`
}

// ReadPurge returns the purge flag of the export repository at path, if any.
func ReadPurge(path string) (Purge, bool, error) {
	raw, err := os.ReadFile(filepath.Join(path, PurgeFlag))
	if errors.Is(err, fs.ErrNotExist) {
		return Purge{}, false, nil
	}
	if err != nil {
		return Purge{}, false, err
	}
	var p Purge
	if err := json.Unmarshal(raw, &p); err != nil || !purgeID.MatchString(p.Purge) || p.Head.IsZero() {
		return Purge{}, false, fmt.Errorf("%s: invalid purge flag", path)
	}
	return p, true, nil
}

// lockPurge takes the purge lock of the export repository at path, waiting
// until ctx is done, and returns its release.
func lockPurge(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(path, PurgeLock), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func writePurge(path string, p Purge) error {
	slices.SortFunc(p.Replaces, func(a, b gitobj.ID) int { return bytes.Compare(a[:], b[:]) })
	p.Replaces = slices.Compact(p.Replaces)
	if p.Replaces == nil {
		p.Replaces = []gitobj.ID{}
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	flag := filepath.Join(path, PurgeFlag)
	return replaceFile(flag, flag+".tmp", append(raw, '\n'))
}

// pruneRefs removes every reference but refs/heads/main, packed references
// and reflogs, so nothing but the branch keeps objects reachable.
func pruneRefs(path string) error {
	refs := filepath.Join(path, "refs")
	keep := filepath.Join(path, filepath.FromSlash(Branch))
	err := filepath.WalkDir(refs, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || p == keep {
			return err
		}
		return os.Remove(p)
	})
	if err != nil {
		return err
	}
	for _, name := range []string{"packed-refs", "logs", "FETCH_HEAD", "ORIG_HEAD"} {
		if err := os.RemoveAll(filepath.Join(path, name)); err != nil {
			return err
		}
	}
	return durable.SyncDir(path)
}

// removeTemporaries deletes the temporary object and pack files that an
// interrupted export, rebuild or repack of the repository at path left, which
// take space and could still hold purged content. The caller holds the
// repository's lock, so none of them is being written.
func removeTemporaries(path string) error {
	objects := filepath.Join(path, "objects")
	return filepath.WalkDir(objects, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := d.Name()
		if strings.HasPrefix(name, "tmp_") || strings.HasPrefix(name, ".tmp-") {
			return os.Remove(p)
		}
		return nil
	})
}
