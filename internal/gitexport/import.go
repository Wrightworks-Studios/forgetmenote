package gitexport

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

var (
	// ErrNotEmpty is an import into a notebook that already has history.
	ErrNotEmpty = errors.New("the notebook already has history; import only into a new notebook")
	// ErrMismatch is a notebook whose stored history differs from the Git
	// history it was imported from.
	ErrMismatch = errors.New("the notebook does not match its Git history")
)

// Bounds of an import from a Git directory, which an operator chose.
var (
	importTimeout   = 6 * time.Hour
	maxImportObject = int64(4 << 30)
)

// ImportGitDir imports the history of HEAD in the Git directory gitDir (a
// bare repository, or a working tree's .git) into nb, which must not exist
// yet, and returns its head, which is HEAD. It streams the commits of git
// log --reverse --topo-order HEAD through git cat-file --batch into
// notestore's ImportCommit, sending each commit with the trees the notebook
// does not have yet, so every commit and tree keeps its bytes and ID.
// Entries take the identities notestore.Options.ImportEntryID gives them and
// otherwise follow the renames Git detects, edited files included, as the
// Git backend's history did.
//
// Other branches and tags are not imported. An import that fails leaves the
// notebook with the commits imported so far; the caller deletes it, or
// finishes it with ResumeGitDir.
func ImportGitDir(ctx context.Context, nb *notestore.Notebook, gitDir string) (notestore.Head, error) {
	return importGitDir(ctx, nb, gitDir, maxImportObject, nil, false)
}

// ResumeGitDir finishes an ImportGitDir of gitDir into nb that stopped part
// way, so nb holds the commits git log --reverse --topo-order HEAD lists
// first and its head is one of them: it streams the same commits, and
// ImportCommit skips those nb has. The caller checks that nb holds nothing
// else. An import that fails again leaves what it stored, for the next
// resume.
func ResumeGitDir(ctx context.Context, nb *notestore.Notebook, gitDir string) (notestore.Head, error) {
	return importGitDir(ctx, nb, gitDir, maxImportObject, nil, true)
}

// importGitDir imports HEAD's history in gitDir into nb, which must not exist
// unless resume is set, and reads no object over objectLimit. It calls
// created, if set, with the incarnation of the notebook (see
// notestore.Notebook.StoredIncarnation) once its first commit created it, so a
// caller removes only what the import stored.
func importGitDir(ctx context.Context, nb *notestore.Notebook, gitDir string, objectLimit int64, created func(incarnation int64), resume bool) (notestore.Head, error) {
	if head, err := nb.Head(ctx); err == nil && !resume {
		return notestore.Head{}, fmt.Errorf("notebook %s at %s: %w", nb.ID(), head.Commit, ErrNotEmpty)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return notestore.Head{}, err
	}
	want, err := revParse(ctx, gitDir, "HEAD")
	if err != nil {
		return notestore.Head{}, err
	}
	cat, err := openCatFile(ctx, gitDir, objectLimit)
	if err != nil {
		return notestore.Head{}, err
	}
	defer cat.Close()
	blob := func(id gitobj.ID) ([]byte, error) { return cat.read(id, "blob") }
	sent := map[gitobj.ID]bool{} // trees the notebook has
	var head notestore.Head
	first := true
	err = streamCommits(ctx, gitDir, importTimeout, want, func(id gitobj.ID, renames map[string]string) error {
		raw, err := cat.read(id, "commit")
		if err != nil {
			return err
		}
		c, err := gitobj.ParseCommit(raw)
		if err != nil {
			return fmt.Errorf("%w: commit %s: %v", notestore.ErrInvalid, id, err)
		}
		// Every tree the notebook lacks, which is every tree no earlier
		// commit had: the store finds the rest itself.
		trees := map[gitobj.ID][]byte{}
		for queue := []gitobj.ID{c.Tree}; len(queue) > 0; {
			t := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			if sent[t] || trees[t] != nil {
				continue
			}
			body, err := cat.read(t, "tree")
			if err != nil {
				return err
			}
			entries, err := gitobj.ParseTree(body)
			if err != nil {
				return fmt.Errorf("%w: tree %s: %v", notestore.ErrInvalid, t, err)
			}
			trees[t] = body
			for _, e := range entries {
				if e.IsTree() && !sent[e.ID] {
					queue = append(queue, e.ID)
				}
			}
		}
		if head, err = nb.ImportCommit(ctx, raw, trees, blob, renames); err != nil {
			return fmt.Errorf("import commit %s: %w", id, err)
		}
		if first && !resume {
			first = false
			// The notebook was empty when the import began, so its first
			// commit creates it, unless someone created it meanwhile: then
			// the notebook is not the import's to continue, or to remove.
			if head.Seq != 1 {
				return fmt.Errorf("notebook %s was created while its import began: %w", nb.ID(), ErrNotEmpty)
			}
			if created != nil {
				// The notebook this commit created is the import's while it
				// is still at that commit: no other writer knows it yet. An
				// import cancelled now still learns it, to remove it.
				stored, incarnation, err := nb.StoredIncarnation(context.WithoutCancel(ctx))
				if err != nil {
					return err
				}
				if stored == head {
					created(incarnation)
				}
			}
		}
		for t := range trees {
			sent[t] = true
		}
		return nil
	})
	if err != nil {
		return notestore.Head{}, err
	}
	if head.Commit != want {
		return head, fmt.Errorf("%w: the import ended at %s, not HEAD %s", ErrMismatch, head.Commit, want)
	}
	return head, nil
}

// A Report counts what Verify or VerifyHeld compared.
type Report struct {
	Head      gitobj.ID `json:"head"`
	Commits   int       `json:"commits"`
	Rewritten int       `json:"rewritten,omitempty"`
	Trees     int       `json:"trees"`
	Blobs     int       `json:"blobs"`
}

// Verify proves that nb holds exactly the history of HEAD in gitDir: its head
// is HEAD, every object it stores hashes to its ID, and the commits, trees
// and blobs it stores are exactly those HEAD reaches. A difference is an
// ErrMismatch error naming one object.
func Verify(ctx context.Context, nb *notestore.Notebook, gitDir string) (Report, error) {
	var rep Report
	head, err := nb.Head(ctx)
	if err != nil {
		return rep, err
	}
	want, err := revParse(ctx, gitDir, "HEAD")
	if err != nil {
		return rep, err
	}
	rep.Head = head.Commit
	if head.Commit != want {
		return rep, fmt.Errorf("%w: head %s, Git HEAD %s", ErrMismatch, head.Commit, want)
	}
	stored, err := storedObjects(ctx, nb, &rep)
	if err != nil {
		return rep, err
	}
	err = streamGit(ctx, gitDir, importTimeout, func(line string) error {
		id, err := gitobj.ParseID(line)
		if err != nil {
			return err
		}
		if _, ok := stored[id]; !ok {
			return fmt.Errorf("%w: Git object %s is not in the notebook", ErrMismatch, id)
		}
		delete(stored, id)
		return nil
	}, "rev-list", "--objects", "--no-object-names", want.String())
	if err != nil {
		return rep, err
	}
	for id := range stored {
		return rep, fmt.Errorf("%w: the notebook stores %s, which HEAD does not reach", ErrMismatch, id)
	}
	return rep, nil
}

// VerifyHeld proves that nb still holds the history of HEAD in gitDir, which
// it was imported from, though it may have taken new revisions since: every
// object nb stores hashes to its ID, every commit HEAD reaches is a commit of
// nb or one a purge nb recorded rewrote, and nb stores HEAD's tree and
// everything under it, unless a purge rewrote HEAD. Its report counts HEAD's
// commits nb holds, those purges rewrote, and the trees and blobs of HEAD's
// tree. A commit or object nb lacks is an ErrMismatch error naming it; a
// notebook nb lacks is an fs.ErrNotExist error.
func VerifyHeld(ctx context.Context, nb *notestore.Notebook, gitDir string) (Report, error) {
	var rep Report
	if _, err := nb.Head(ctx); err != nil {
		return rep, err
	}
	want, err := revParse(ctx, gitDir, "HEAD")
	if err != nil {
		return rep, err
	}
	rep.Head = want
	stored, err := storedObjects(ctx, nb, nil)
	if err != nil {
		return rep, err
	}
	rewritten, err := nb.RewrittenAll(ctx)
	if err != nil {
		return rep, err
	}
	err = streamGit(ctx, gitDir, importTimeout, func(line string) error {
		id, err := gitobj.ParseID(line)
		if err != nil {
			return err
		}
		if _, ok := rewritten[id]; ok {
			rep.Rewritten++
		} else if stored[id] == "commit" {
			rep.Commits++
		} else {
			return fmt.Errorf("%w: commit %s of Git HEAD's history is not in the notebook", ErrMismatch, id)
		}
		return nil
	}, "rev-list", want.String())
	if _, ok := rewritten[want]; ok || err != nil {
		return rep, err
	}
	err = streamGit(ctx, gitDir, importTimeout, func(line string) error {
		id, err := gitobj.ParseID(line)
		if err != nil {
			return err
		}
		switch stored[id] {
		case "tree":
			rep.Trees++
		case "blob":
			rep.Blobs++
		default:
			return fmt.Errorf("%w: Git object %s of HEAD's tree is not in the notebook", ErrMismatch, id)
		}
		return nil
	}, "rev-list", "--objects", "--no-object-names", want.String()+"^{tree}")
	return rep, err
}

// storedObjects returns the kind of every object nb stores by its ID, once
// each hashed to its ID, and counts them in rep, if set.
func storedObjects(ctx context.Context, nb *notestore.Notebook, rep *Report) (map[gitobj.ID]string, error) {
	stored := map[gitobj.ID]string{}
	err := nb.Objects(ctx, 0, func(kind string, id gitobj.ID, body []byte) error {
		if gitobj.HashObject(kind, body) != id {
			return fmt.Errorf("%w: the stored %s %s does not hash to its ID", ErrMismatch, kind, id)
		}
		if _, ok := stored[id]; !ok && rep != nil {
			switch kind {
			case "commit":
				rep.Commits++
			case "tree":
				rep.Trees++
			case "blob":
				rep.Blobs++
			}
		}
		stored[id] = kind
		return ctx.Err()
	})
	return stored, err
}
