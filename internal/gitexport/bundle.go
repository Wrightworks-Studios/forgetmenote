package gitexport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"forgetmenote/internal/capacity"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/procgroup"
)

var (
	// ErrBundle is a bundle the notebook import refuses; the error wrapping it
	// says why.
	ErrBundle = errors.New("cannot import notebook bundle")
	// ErrBundleTooLarge is an upload over Limits.BundleBytes.
	ErrBundleTooLarge = errors.New("bundle exceeds FMN_LIMIT_BUNDLE_BYTES")
	// ErrBundleTimeout is a bundle whose check ran out of time, which does
	// not show that the bundle is damaged.
	ErrBundleTimeout = errors.New("checking the notebook bundle timed out")
)

// Limits bound a bundle import and vet the files of its HEAD, as the
// notebook import always has. Zero sizes take the capacity budgets.
type Limits struct {
	// BundleBytes bounds the upload (FMN_LIMIT_BUNDLE_BYTES).
	BundleBytes int64
	// ObjectBytes bounds every object of the history, expanded, together
	// (FMN_LIMIT_IMPORT_BYTES), and Objects their number
	// (FMN_LIMIT_IMPORT_OBJECTS).
	ObjectBytes int64
	Objects     int
	// FileBytes is the largest a file at HEAD may be; nil allows
	// FMN_LIMIT_FILE_BYTES for every path.
	FileBytes func(path string) int64
	// Allowed reports whether a notebook may hold a file at path. Required.
	Allowed func(path string) bool
	// NoteID returns the fmn-id a note declares, "" for none, or an error
	// for a malformed one. Required.
	NoteID func(content []byte) (string, error)
	// Staging is the private directory the upload is unbundled in; empty
	// is the system's temporary directory.
	Staging string
	// Created, if set, is called with the incarnation of the notebook once
	// the import's first commit created it, so a caller that removes a failed
	// import removes only what it stored (notestore.Store.DeleteIncarnation).
	Created func(incarnation int64)
}

// The Git check of a bundle may take checkTimeout, as before storage v2, and
// one second more for every checkRate bytes of the bundle: fetching and
// checking a history takes time in proportion to it. On a slow disk F2's
// 952 MB bundle (20,000 revisions) took 28 s to fetch and 8 s to check, and
// its import twice failed, most likely because the check ran out of the 90 s
// that bounded every bundle before.
// Its check now has about 9 minutes; the notebook import around the check has
// a limit of its own (workspace.ImportBundleDB), as a caller's ctx may.
var (
	checkTimeout       = 90 * time.Second
	checkRate    int64 = 2 << 20
)

// checkLimit is how long the Git check of a bundle of size bytes may take.
func checkLimit(size int64) time.Duration {
	return checkTimeout + time.Duration(float64(size)/float64(checkRate)*float64(time.Second))
}

var revision = regexp.MustCompile(`^[a-f0-9]{40}$`)

// ImportBundle imports the history of the HEAD of the Git bundle r into nb,
// which must not exist yet, and returns the new head. It refuses, with an
// ErrBundle or ErrBundleTooLarge error and before storing anything, an
// upload over the limits, an incomplete bundle, references other than HEAD,
// branches, tags and remote branches, invalid objects (git fsck --strict),
// history over the object limits, and a HEAD with a file that is not an
// allowed notebook file: a symlink, a submodule, an unsafe path, a file over
// its budget, or a note with a malformed or duplicate fmn-id. The bundle is
// unpacked into a temporary bare repository and imported with ImportGitDir,
// so entries take the identities notestore.Options.ImportEntryID gives them
// (workspace.ImportEntryID).
//
// Only the history of HEAD is imported: the bundle's branches, tags and
// remote branches are validated and then left out, where the notebook import
// before storage v2 kept every reference. Only the Git validation has its own
// bound, 90 seconds and one more for every 2 MiB of the bundle; storing the
// history runs under ctx, which a caller on a request path bounds. A check
// that runs out of either is an ErrBundleTimeout error, never a refusal of
// the bundle. An import that fails while storing leaves the commits stored so
// far; the caller deletes the notebook.
func ImportBundle(ctx context.Context, nb *notestore.Notebook, r io.Reader, l Limits) (notestore.Head, error) {
	if l.Allowed == nil || l.NoteID == nil {
		return notestore.Head{}, errors.New("gitexport: a bundle import needs Limits.Allowed and Limits.NoteID")
	}
	if l.BundleBytes <= 0 {
		l.BundleBytes = int64(capacity.BundleBytes.Limit())
	}
	if l.ObjectBytes <= 0 {
		l.ObjectBytes = int64(capacity.ImportBytes.Limit())
	}
	if l.Objects <= 0 {
		l.Objects = capacity.ImportObjects.Limit()
	}
	if l.FileBytes == nil {
		l.FileBytes = func(string) int64 { return int64(capacity.FileBytes.Limit()) }
	}
	if _, err := nb.Head(ctx); err == nil {
		return notestore.Head{}, fmt.Errorf("notebook %s: %w", nb.ID(), ErrNotEmpty)
	}
	staging, err := os.MkdirTemp(l.Staging, "fmn-bundle-")
	if err != nil {
		return notestore.Head{}, err
	}
	defer os.RemoveAll(staging)
	repo, err := checkBundle(ctx, staging, r, l)
	if err != nil {
		return notestore.Head{}, err
	}
	head, err := importGitDir(ctx, nb, repo, l.ObjectBytes, l.Created)
	if errors.Is(err, notestore.ErrInvalid) {
		return head, fmt.Errorf("%w: %v", ErrBundle, err)
	}
	return head, err
}

// checkBundle stores the upload in staging, validates it and fetches its HEAD
// into a bare repository as refs/heads/main, which it returns.
func checkBundle(ctx context.Context, staging string, input io.Reader, l Limits) (string, error) {
	bundle := filepath.Join(staging, "upload.bundle")
	f, err := os.OpenFile(bundle, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	n, copyErr := io.Copy(f, io.LimitReader(input, l.BundleBytes+1))
	closeErr := f.Close()
	if n > l.BundleBytes {
		return "", ErrBundleTooLarge
	}
	if err := errors.Join(copyErr, closeErr); err != nil {
		return "", err
	}
	// The bound starts once the upload is stored, and grows with it.
	limit := checkLimit(n)
	ctx, cancel := context.WithTimeoutCause(ctx, limit, fmt.Errorf("%w after %v, which does not show that the bundle is damaged; import it again when the server is less busy",
		ErrBundleTimeout, limit.Round(time.Second)))
	defer cancel()
	repo := filepath.Join(staging, "repo.git")
	if err := os.Mkdir(repo, 0o700); err != nil {
		return "", err
	}
	git := func(refusal string, args ...string) (string, error) {
		return validationGit(ctx, repo, refusal, args...)
	}
	if _, err := git("Git validation failed", "init", "--quiet", "--bare", "--initial-branch=main", "--template="); err != nil {
		return "", err
	}
	// Verification against an empty repository rejects incremental bundles.
	if _, err := git("choose a complete Git bundle exported from a notebook", "bundle", "verify", "--quiet", bundle); err != nil {
		return "", err
	}
	refs, err := git("Git validation failed", "bundle", "list-heads", bundle)
	if err != nil {
		return "", err
	}
	head := ""
	for _, line := range strings.Split(refs, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !revision.MatchString(fields[0]) {
			return "", fmt.Errorf("%w: invalid references", ErrBundle)
		}
		if fields[1] == "HEAD" {
			head = fields[0]
			continue
		}
		if !strings.HasPrefix(fields[1], "refs/heads/") && !strings.HasPrefix(fields[1], "refs/tags/") && !strings.HasPrefix(fields[1], "refs/remotes/") {
			return "", fmt.Errorf("%w: unsupported Git references", ErrBundle)
		}
	}
	if head == "" {
		return "", fmt.Errorf("%w: the bundle must include HEAD", ErrBundle)
	}
	if _, err := git("damaged or incomplete Git history", "fetch", "--quiet", "--no-tags", "--update-head-ok", "--", bundle, "+HEAD:"+Branch); err != nil {
		return "", err
	}
	if _, err := git("invalid Git objects", "fsck", "--strict", "--no-reflogs", "--no-dangling", "--no-progress"); err != nil {
		return "", err
	}
	// Check all of the history before storing any of it. The upload and
	// expanded object limits bound ordinary exports, compressed blobs too.
	sizes, err := git("Git validation failed", "cat-file", "--batch-all-objects", "--batch-check=%(objectsize)")
	if err != nil {
		return "", err
	}
	var total int64
	for i, size := range strings.Fields(sizes) {
		n, err := strconv.ParseInt(size, 10, 64)
		if err != nil || n < 0 || n > l.ObjectBytes-total || i >= l.Objects {
			return "", fmt.Errorf("%w: history exceeds FMN_LIMIT_IMPORT_BYTES or FMN_LIMIT_IMPORT_OBJECTS; use an operator restore", ErrBundle)
		}
		total += n
	}
	tree, err := git("HEAD must identify a commit", "ls-tree", "-r", "-l", "-z", head)
	if err != nil {
		return "", err
	}
	var notes []gitobj.ID
	var notePaths []string
	for _, entry := range strings.Split(tree, "\x00") {
		if entry == "" {
			continue
		}
		metadata, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 4 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
			return "", fmt.Errorf("%w: symlinks and submodules are not supported", ErrBundle)
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || size > l.FileBytes(name) {
			return "", fmt.Errorf("%w: a file exceeds its configured file or source byte budget", ErrBundle)
		}
		if len(name) > 512 || strings.ContainsAny(name, "\\\r\n") {
			return "", fmt.Errorf("%w: unsupported file path", ErrBundle)
		}
		for _, part := range strings.Split(name, "/") {
			if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
				return "", fmt.Errorf("%w: unsafe file path", ErrBundle)
			}
		}
		if !l.Allowed(name) {
			return "", fmt.Errorf("%w: unsupported notebook file: %s", ErrBundle, name)
		}
		if strings.HasSuffix(name, ".md") && name != "AGENTS.md" {
			id, err := gitobj.ParseID(fields[2])
			if err != nil {
				return "", fmt.Errorf("%w: invalid Git objects", ErrBundle)
			}
			notes, notePaths = append(notes, id), append(notePaths, name)
		}
	}
	if len(notes) > 0 {
		cat, err := openCatFile(ctx, repo, l.ObjectBytes)
		if err != nil {
			return "", checkFailed(ctx, err)
		}
		defer cat.Close()
		identities := map[string]bool{}
		for i, blob := range notes {
			content, err := cat.read(blob, "blob")
			if err != nil {
				return "", checkFailed(ctx, fmt.Errorf("%w: Git validation failed", ErrBundle))
			}
			id, err := l.NoteID(content)
			if err != nil {
				return "", fmt.Errorf("%w: invalid fmn-id in %s", ErrBundle, notePaths[i])
			}
			if id == "" {
				continue
			}
			if identities[id] {
				return "", fmt.Errorf("%w: duplicate fmn-id in %s", ErrBundle, notePaths[i])
			}
			identities[id] = true
		}
	}
	return repo, nil
}

// validationGit runs Git over an untrusted upload with bounded output. Any
// failure, too much output included, refuses the bundle with an ErrBundle
// error saying refusal, which reveals nothing of the repository, unless the
// check ran out of time (checkFailed).
func validationGit(ctx context.Context, repo, refusal string, args ...string) (string, error) {
	cmd := gitCommand(ctx, repo, args...)
	output := procgroup.Output{Limit: 8 << 20, Err: fmt.Errorf("%w: repository metadata is too large", ErrBundle)}
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return "", checkFailed(ctx, fmt.Errorf("%w: %s", ErrBundle, refusal))
	}
	return strings.TrimSuffix(output.String(), "\n"), nil
}

// checkFailed is the error of a step of a bundle's check that failed with
// err. Once ctx is done the step may have failed for that alone, so it is the
// check's timeout, or the caller's deadline as a timeout too, or the caller's
// cancellation; otherwise it is err.
func checkFailed(ctx context.Context, err error) error {
	if ctx.Err() == nil {
		return err
	}
	cause := context.Cause(ctx)
	if errors.Is(cause, context.DeadlineExceeded) {
		return fmt.Errorf("%w: the import ran out of time, which does not show that the bundle is damaged; import it again when the server is less busy", ErrBundleTimeout)
	}
	return cause
}
