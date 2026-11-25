package catalog

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"forgetmenote/internal/gitobj"
)

// repositoryHead returns the commit HEAD names in gitDir, the .git directory
// of a repository the previous release kept a notebook in, reading it as Git
// does without running Git: HEAD holds a commit, detached, or names a branch,
// whose commit is in the branch's loose ref file or in packed-refs. ok is
// false for a branch with no commit yet. A .git file (a linked worktree or
// submodule) and the reftable format, which the previous release never
// wrote, are refused.
func repositoryHead(gitDir string) (head gitobj.ID, ok bool, err error) {
	info, err := os.Lstat(gitDir)
	if err != nil {
		return head, false, err
	}
	if !info.IsDir() {
		return head, false, fmt.Errorf("%s is not a Git directory", gitDir)
	}
	if _, err := os.Lstat(filepath.Join(gitDir, "reftable")); err == nil {
		return head, false, fmt.Errorf("%s keeps its references in the reftable format, which this check does not read", gitDir)
	}
	ref := "HEAD"
	for range 5 {
		value, found, err := readGitRef(gitDir, ref)
		if err != nil || !found {
			return head, false, err
		}
		target, symbolic := strings.CutPrefix(value, "ref: ")
		if !symbolic {
			head, err = gitobj.ParseID(value)
			if err != nil {
				return head, false, fmt.Errorf("%s: %s does not name a commit: %w", gitDir, ref, err)
			}
			return head, true, nil
		}
		ref = strings.TrimSpace(target)
		if !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") || strings.ContainsAny(ref, "\\\x00") {
			return head, false, fmt.Errorf("%s: HEAD names the invalid reference %q", gitDir, ref)
		}
	}
	return head, false, fmt.Errorf("%s: HEAD names a chain of more than five references", gitDir)
}

// readGitRef returns the value of ref in gitDir: its loose file's content,
// or the commit packed-refs records for it. found is false when neither has
// it. HEAD itself must exist.
func readGitRef(gitDir, ref string) (value string, found bool, err error) {
	raw, err := os.ReadFile(filepath.Join(gitDir, filepath.FromSlash(ref)))
	if err == nil {
		return strings.TrimSpace(string(raw)), true, nil
	}
	if ref == "HEAD" || !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}
	packed, err := os.ReadFile(filepath.Join(gitDir, "packed-refs"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	lines := bufio.NewScanner(bytes.NewReader(packed))
	lines.Buffer(nil, 1<<20)
	for lines.Scan() {
		line := lines.Text()
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue // the header, and the commit a tag above peels to
		}
		if sha, name, ok := strings.Cut(line, " "); ok && name == ref {
			return sha, true, nil
		}
	}
	return "", false, lines.Err()
}
