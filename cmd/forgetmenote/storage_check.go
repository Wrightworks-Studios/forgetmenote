package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/procgroup"
	"forgetmenote/internal/workspace"
)

// checkNotebooks are the notebooks admin storage check covers: every one the
// catalog lists and every directory under DATA/projects, or only.
func checkNotebooks(ctx context.Context, db *pg.DB, data, only string) (ids []string, cataloged map[string]bool, err error) {
	listed, err := catalog.StoredNotebooks(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	cataloged = map[string]bool{}
	for _, id := range listed {
		cataloged[id] = true
	}
	if only != "" {
		ids, err = storageNotebooks(data, only)
		return ids, cataloged, err
	}
	ids, err = storageNotebooks(data, "")
	if errors.Is(err, fs.ErrNotExist) {
		ids, err = nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, id := range listed {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids, cataloged, nil
}

// A checkTally counts what admin storage check found: the notebooks it
// compared with their previous release repository, and those that passed,
// failed and were skipped.
type checkTally struct{ compared, passed, failed, skipped int }

func (t *checkTally) add(result map[string]any, err error) {
	if result["compared"] == true {
		t.compared++
	}
	status, _ := result["status"].(string)
	switch {
	case err != nil:
		t.failed++
	case strings.HasPrefix(status, "passed"):
		t.passed++
	default:
		t.skipped++
	}
}

// summary is check's last line: the count of each status (summary), as
// retire's last line has it, and of the notebooks compared, passed, failed
// and skipped (counts). It passes only when check compared a notebook and
// none failed.
func (t checkTally) summary(notebooks int, statuses map[string]int) map[string]any {
	return map[string]any{
		"summary":   statuses,
		"counts":    map[string]int{"compared": t.compared, "passed": t.passed, "failed": t.failed, "skipped": t.skipped},
		"notebooks": notebooks,
		"passed":    t.failed == 0 && t.compared > 0,
	}
}

// nothingCompared is the error check fails with when it compared no
// notebook, though none failed: such a check proves nothing.
func (t checkTally) nothingCompared(notebooks int, statuses map[string]int) error {
	if notebooks == 0 {
		return errors.New("storage check compared no notebook: it found none, as the catalog lists none and the data directory's projects/ holds none; is -data the instance's data directory?")
	}
	return fmt.Errorf("storage check compared no notebook: it found %d, and none has a previous release's repository to compare with (%s), so it proves nothing. Is -data the instance's data directory? Once retire has removed the repositories, there is nothing left to compare", notebooks, countStatuses(statuses))
}

// countStatuses lists statuses with their counts, in order.
func countStatuses(statuses map[string]int) string {
	var counts []string
	for _, status := range slices.Sorted(maps.Keys(statuses)) {
		counts = append(counts, fmt.Sprintf("%d %s", statuses[status], status))
	}
	return strings.Join(counts, ", ")
}

// checkNotebook proves, right after the cutover, that the notes store serves
// notebook id exactly as its previous release's repository holds it:
//
//  1. the store holds exactly the history of the repository's HEAD
//     (gitexport.Verify), and the previous release's files hold no work that
//     history lacks (checkQuiescent);
//  2. the export, DATA/exports/ID.git, ends at HEAD and passes git fsck
//     --strict (checkExport);
//  3. every file of HEAD's tree is in the store's read model of its head,
//     and reads through workspace.Store.Read, the read path the server
//     serves notes and attachments with, as bytes that hash to the blob Git
//     names, unless that path refuses the file as the previous release did
//     (checkServedFiles);
//  4. each note's whole history, through workspace.Store.WholeHistory, the
//     walk whose newest 50 entries the server lists, names the commits and
//     paths the repository gives when its renames are followed as the import
//     follows them (checkHistories).
//
// A notebook that took revisions after HEAD, written after the start, is
// proved at HEAD instead, once HEAD is the commit the import recorded
// (checkImportedHead): the store holds the whole history of HEAD
// (gitexport.VerifyHeld), each file reads at revision HEAD as Git has it,
// each note's history as of HEAD matches, and the export's branch holds
// HEAD. Its status says so.
//
// It only reads the store, the repository and the export, so it runs beside
// the new release's gateways, as the runbook has it. A notebook without a
// repository has nothing to compare against (checkWithoutRepository).
func checkNotebook(ctx context.Context, db *pg.DB, store *notestore.Store, data, id string, cataloged bool, result map[string]any) (err error) {
	result["catalog"], result["compared"] = cataloged, false
	defer func() {
		if err != nil {
			result["status"] = "failed"
		}
	}()
	project := filepath.Join(data, "projects", id)
	gitDir := filepath.Join(project, "workspace", ".git")
	nb := store.Notebook(id)
	unfinished, err := catalog.StorageImportUnfinished(ctx, db, id)
	if err != nil {
		return err
	}
	if unfinished {
		return errors.New("its storage import did not finish; run `forgetmenote admin storage import` again, which resumes and verifies it")
	}
	if _, err := os.Lstat(gitDir); errors.Is(err, fs.ErrNotExist) {
		return checkWithoutRepository(ctx, db, nb, project, id, cataloged, result)
	} else if err != nil {
		return err
	}
	result["compared"] = true

	// 1. The history.
	if err := checkQuiescent(ctx, project); err != nil {
		return err
	}
	source, err := cutoverGit(ctx, gitDir, "", "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return err
	}
	head, err := gitobj.ParseID(source)
	if err != nil {
		return err
	}
	result["head"] = head.String()
	stored, err := nb.Head(ctx)
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("the notes store does not hold this notebook; import it (`forgetmenote admin storage import`)")
	}
	if err != nil {
		return fmt.Errorf("history: the notes store cannot read the notebook's head: %w", err)
	}
	latest := stored.Commit == head
	var rep gitexport.Report
	if latest {
		rep, err = gitexport.Verify(ctx, nb, gitDir)
	} else {
		at, err := nb.Commit(ctx, head)
		if errors.Is(err, fs.ErrNotExist) {
			return headDifference(ctx, gitDir, stored.Commit, head)
		} else if err != nil {
			return err
		}
		if err := checkImportedHead(ctx, db, id, head); err != nil {
			return err
		}
		result["storeHead"], result["laterRevisions"] = stored.Commit.String(), stored.Seq-at.Seq
		rep, err = gitexport.VerifyHeld(ctx, nb, gitDir)
	}
	if err != nil {
		return fmt.Errorf("history: %w", err)
	}
	result["commits"], result["trees"], result["blobs"] = rep.Commits, rep.Trees, rep.Blobs
	if outside, err := refsOutsideHead(ctx, gitDir); err != nil {
		return err
	} else if len(outside) > 0 {
		result["notImported"] = outside
	}

	// 2. The export.
	export := filepath.Join(data, "exports", id+".git")
	result["export"] = export
	problems := checkExport(ctx, nb, gitDir, export, head, stored.Commit)

	// 3 and 4 read the notebook through the workspace, as the server does.
	// OpenDB creates the state directory it is given; project, which holds
	// the repository, exists.
	s, err := workspace.OpenDB(ctx, store, id, project, nil)
	if err != nil {
		return err
	}
	defer s.Close()
	files, err := gitFiles(ctx, gitDir, head)
	if err != nil {
		return err
	}
	served, refused, wrong, err := checkServedFiles(ctx, nb, s, files, head, latest)
	if err != nil {
		return fmt.Errorf("files: %w", err)
	}
	result["files"], result["refused"] = served, refused
	problems = append(problems, wrong...)
	notes, wrong, err := checkHistories(ctx, s, gitDir, head, files)
	if err != nil {
		return fmt.Errorf("note histories: %w", err)
	}
	result["notes"] = notes
	problems = append(problems, wrong...)
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	result["status"] = "passed"
	if !latest {
		result["status"] = "passed at Git HEAD"
	}
	return nil
}

// checkWithoutRepository checks notebook id, whose previous release
// repository is not in project, so there is nothing to compare it with. It
// fails when the notebook is nowhere at all, which a mistyped -notebook
// gives, and when the catalog lists it, unless retire removed its repository
// (catalog.StorageRetired): the new release refuses a notebook the store
// lacks, and a repository that went missing otherwise means a wrong -data,
// a repository moved or lost, or a notebook created after the start, none
// of which proves the cutover. Anything else is skipped.
func checkWithoutRepository(ctx context.Context, db *pg.DB, nb *notestore.Notebook, project, id string, cataloged bool, result map[string]any) error {
	_, err := nb.Head(ctx)
	held := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	retired, err := catalog.StorageRetired(ctx, db, id)
	if err != nil {
		return err
	}
	_, err = os.Lstat(project)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	switch {
	case !cataloged && !held && err != nil:
		return errors.New("no such notebook: the catalog does not list it, and neither the notes store nor the data directory holds it")
	case cataloged && !held:
		return errors.New("the catalog lists this notebook, but neither the notes store nor a Git repository holds it, so the new release refuses to serve it; restore it from a backup")
	case !retired.IsZero():
		result["status"], result["retired"] = "skipped: retired", retired.UTC().Format(time.RFC3339)
		return nil
	case cataloged:
		return fmt.Errorf("its previous release repository, %s, is missing, and retire never removed it, so nothing proves what the notes store holds: is -data the instance's data directory? Otherwise the repository was moved or lost, or the notebook was created after the new release started; restore the repository from the freeze backups to check the notebook", filepath.Join(project, "workspace"))
	}
	result["status"] = "skipped: no notebook repository"
	return nil
}

// checkImportedHead proves that head, Git HEAD of notebook id, which the
// store holds with revisions after it, is the commit the import imported
// (catalog.StorageImported), so those revisions were written after the
// start. A repository older than the one the import read, from an older
// copy of the data directory or a HEAD moved back, holds only commits the
// store holds too, and proves nothing about what the import read.
func checkImportedHead(ctx context.Context, db *pg.DB, id string, head gitobj.ID) error {
	imported, err := catalog.StorageImported(ctx, db, id)
	switch {
	case err != nil:
		return err
	case imported == "":
		return fmt.Errorf("history: the import recorded no commit it imported, as an import by an earlier build did, so nothing tells the notes store's revisions after Git HEAD %s from a repository older than the one the import read", head)
	case imported != head.String():
		return fmt.Errorf("history: Git HEAD %s is not %s, the commit the import imported, which the notes store holds with revisions after it: -data is an older copy of the data directory than the import read, or the repository's HEAD moved; check the data directory the import read", head, imported)
	}
	return nil
}

// headDifference explains a stored head that is neither the repository's
// HEAD nor a revision after it.
func headDifference(ctx context.Context, gitDir string, stored, head gitobj.ID) error {
	if _, err := cutoverGit(ctx, gitDir, "", "merge-base", "--is-ancestor", stored.String(), head.String()); err == nil {
		n, err := cutoverGit(ctx, gitDir, "", "rev-list", "--count", stored.String()+".."+head.String())
		if err != nil {
			return err
		}
		return fmt.Errorf("history: Git HEAD %s has %s the notes store lacks; its head is %s: the previous release committed after the import, or the import stopped part way. Run `forgetmenote admin storage import`, which resumes an import that stopped; otherwise restore the database from the dump taken at the freeze and import every notebook again (docs/storage-cutover.md)", head, countCommits(n), stored)
	}
	return fmt.Errorf("history: the notes store's head %s is not Git HEAD %s, and neither history holds the other's head: the repository's HEAD moved to another history after the import; restore the database from the dump taken at the freeze and import every notebook again (docs/storage-cutover.md)", stored, head)
}

// checkExport proves that the export, export, holds HEAD and passes git
// fsck --strict: its branch is the store's head, stored, which is HEAD
// unless the notebook took revisions after it; then a branch the exporter
// has not brought up to the head yet passes as long as it is a revision of
// the notebook that holds HEAD. It returns what differs.
func checkExport(ctx context.Context, nb *notestore.Notebook, gitDir, export string, head, stored gitobj.ID) []string {
	branch, err := gitexport.VerifyExport(ctx, export, stored)
	if errors.Is(err, gitexport.ErrMismatch) && stored != head && !branch.IsZero() {
		if _, e := nb.Commit(ctx, branch); e == nil {
			if _, e := cutoverGit(ctx, export, "", "merge-base", "--is-ancestor", head.String(), branch.String()); e == nil {
				branch, err = gitexport.VerifyExport(ctx, export, branch)
			}
		}
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return []string{fmt.Sprintf("export: %s does not exist; run `forgetmenote admin storage export`", export)}
	case errors.Is(err, gitexport.ErrMismatch):
		return []string{fmt.Sprintf("export: %v; run `forgetmenote admin storage export`, and `-recreate` if that refuses it", exportDifference(ctx, gitDir, branch, head, err))}
	}
	return []string{fmt.Sprintf("export: %v; run `forgetmenote admin storage export -recreate`", err)}
}

// exportDifference explains an export branch that is not HEAD.
func exportDifference(ctx context.Context, gitDir string, branch, head gitobj.ID, err error) error {
	if branch.IsZero() {
		return fmt.Errorf("the export has no commit, Git HEAD is %s", head)
	}
	if _, e := cutoverGit(ctx, gitDir, "", "merge-base", "--is-ancestor", branch.String(), head.String()); e == nil {
		n, e := cutoverGit(ctx, gitDir, "", "rev-list", "--count", branch.String()+".."+head.String())
		if e == nil {
			return fmt.Errorf("the export is %s behind Git HEAD %s, at %s", countCommits(n), head, branch)
		}
	}
	return err
}

// countCommits is n, a count Git printed, with its noun.
func countCommits(n string) string {
	if n == "1" {
		return "1 commit"
	}
	return n + " commits"
}

// gitFile is one file of a Git tree, as git ls-tree -r -l lists it.
type gitFile struct {
	path string
	mode uint32
	blob gitobj.ID
	size int64 // -1 for a submodule
}

// gitFiles lists the files of rev's tree in gitDir.
func gitFiles(ctx context.Context, gitDir string, rev gitobj.ID) ([]gitFile, error) {
	var files []gitFile
	err := cutoverStream(ctx, gitDir, func(field string) error {
		meta, path, ok := strings.Cut(field, "\t")
		parts := strings.Fields(meta)
		if !ok || len(parts) != 4 {
			return fmt.Errorf("unexpected git ls-tree output %q", field)
		}
		f := gitFile{path: path, size: -1}
		mode, err := strconv.ParseUint(parts[0], 8, 32)
		if err == nil {
			f.mode = uint32(mode)
			f.blob, err = gitobj.ParseID(parts[2])
		}
		if err == nil && parts[3] != "-" {
			f.size, err = strconv.ParseInt(parts[3], 10, 64)
		}
		if err != nil {
			return fmt.Errorf("unexpected git ls-tree output %q: %w", field, err)
		}
		files = append(files, f)
		return nil
	}, "ls-tree", "-r", "-z", "-l", "--full-tree", rev.String())
	return files, err
}

// servablePath is whether the workspace's read path takes path at all: no
// hidden name, as the previous release had it (workspace.Store.resolve).
func servablePath(path string) bool {
	if path == "" || len(path) > 512 || strings.ContainsAny(path, "\\\x00\r\n") || filepath.IsAbs(path) {
		return false
	}
	for _, name := range strings.Split(path, "/") {
		if name == "" || strings.HasPrefix(name, ".") {
			return false
		}
	}
	return true
}

// checkServedFiles compares the store's head with files, HEAD's tree: its
// read model lists each file with the same blob and mode and nothing else,
// and s.Read at head returns bytes that hash to each file's blob. The read
// path refuses hidden paths, files that are not regular files and files
// over their byte budget, as the previous release did; those count as
// refused, and step 1 proved their bytes. When the store's head is not
// head, latest is false and the store's files at head stand in for its
// read model. It returns how many files were served identical and refused,
// and what differs.
func checkServedFiles(ctx context.Context, nb *notestore.Notebook, s *workspace.Store, files []gitFile, head gitobj.ID, latest bool) (served, refused int, problems []string, err error) {
	var wrong []string
	at, where := gitobj.ID{}, "the store's files at its head"
	if !latest {
		at, where = head, "the store's files at Git HEAD"
	}
	listed, err := nb.Files(ctx, at)
	if err != nil {
		return 0, 0, nil, err
	}
	model := map[string]notestore.File{}
	for _, f := range listed {
		model[f.Path] = f
	}
	for _, f := range files {
		if m, ok := model[f.path]; !ok {
			wrong = append(wrong, fmt.Sprintf("%s is not in %s", f.path, where))
		} else if m.Blob != f.blob || m.Mode != f.mode {
			wrong = append(wrong, fmt.Sprintf("%s is blob %s mode %o in %s, %s mode %o in Git HEAD's tree", f.path, m.Blob, m.Mode, where, f.blob, f.mode))
		}
		delete(model, f.path)
	}
	for _, p := range slices.Sorted(maps.Keys(model)) {
		wrong = append(wrong, fmt.Sprintf("%s is in %s but not in Git HEAD's tree", p, where))
	}
	for _, f := range files {
		servable := servablePath(f.path) && f.mode == gitobj.ModeFile && f.size <= int64(workspace.MaxReadBytes(f.path))
		doc, err := s.Read(ctx, f.path, head.String())
		switch {
		case err == nil && gitobj.BlobID(doc.Content) == f.blob:
			served++
		case err == nil:
			wrong = append(wrong, fmt.Sprintf("%s reads as %d bytes that differ from Git HEAD's %d", f.path, len(doc.Content), f.size))
		case ctx.Err() != nil:
			return 0, 0, nil, ctx.Err()
		case servable:
			wrong = append(wrong, fmt.Sprintf("%s does not read: %v", f.path, err))
		default:
			refused++
		}
	}
	return served, refused, listProblems("files", wrong), nil
}

// checkHistories compares the whole history s lists for each note of files,
// HEAD's tree, as of HEAD, with the one the repository gives, and returns
// how many it compared and what differs.
func checkHistories(ctx context.Context, s *workspace.Store, gitDir string, head gitobj.ID, files []gitFile) (int, []string, error) {
	commits, err := gitImportedChanges(ctx, gitDir, head)
	if err != nil {
		return 0, nil, err
	}
	var wrong []string
	notes := 0
	for _, f := range files {
		if !strings.HasSuffix(f.path, ".md") || !servablePath(f.path) {
			continue
		}
		notes++
		want := gitHistory(commits, f.path)
		got, err := s.WholeHistory(ctx, f.path, head.String())
		if err != nil {
			wrong = append(wrong, fmt.Sprintf("%s has no history: %v", f.path, err))
			continue
		}
		if i := historyDifference(got, want); i >= 0 {
			entry := func(list []historyEntry) string {
				if i >= len(list) {
					return "nothing"
				}
				return list[i].revision + " (" + list[i].path + ")"
			}
			var have []historyEntry
			for _, c := range got {
				have = append(have, historyEntry{c.Revision, c.Path})
			}
			wrong = append(wrong, fmt.Sprintf("%s has %s in the store and %d in Git, and entry %d is %s in the store but %s in Git", f.path, countEntries(len(got)), len(want), i+1, entry(have), entry(want)))
		}
	}
	return notes, listProblems("note histories", wrong), nil
}

func countEntries(n int) string {
	if n == 1 {
		return "1 history entry"
	}
	return fmt.Sprintf("%d history entries", n)
}

// historyDifference is the first entry where got and want differ, or -1.
func historyDifference(got []workspace.Commit, want []historyEntry) int {
	for i := range max(len(got), len(want)) {
		if i >= len(got) || i >= len(want) || got[i].Revision != want[i].revision || got[i].Path != want[i].path {
			return i
		}
	}
	return -1
}

// listProblems names the first few of what differs in one part of the check.
func listProblems(part string, wrong []string) []string {
	if len(wrong) == 0 {
		return nil
	}
	more := ""
	if len(wrong) > 5 {
		more = fmt.Sprintf(", and %d more", len(wrong)-5)
		wrong = wrong[:5]
	}
	return []string{fmt.Sprintf("%s: %s%s", part, strings.Join(wrong, "; "), more)}
}

// A historyEntry is one revision of a note's history and the note's path in
// it.
type historyEntry struct{ revision, path string }

// gitHistory is the whole history of the note at path as the server walks
// it (workspace's pathHistory), from commits: newest first, the revisions
// that changed a file at or under path, following path back through each
// "Move %q to %q" revision and each rename to it; a merge is followed but not
// listed.
func gitHistory(commits []importedCommit, path string) []historyEntry {
	var out []historyEntry
	for i := len(commits) - 1; i >= 0; i-- {
		c := commits[i]
		if !historyTouches(c.changes, path) {
			continue
		}
		changes := c.changes
		if c.merge {
			changes = nil
		} else {
			out = append(out, historyEntry{c.id.String(), path})
		}
		var from, to string
		if k, _ := fmt.Sscanf(c.subject, "Move %q to %q", &from, &to); k == 2 && from != "" && to != "" {
			path = movedPath(path, to, from)
			continue
		}
		for _, ch := range changes {
			if ch.oldPath != "" && ch.path == path {
				path = ch.oldPath
				break
			}
		}
	}
	return out
}

func historyTouches(changes []importedChange, path string) bool {
	under := func(p string) bool { return p == path || strings.HasPrefix(p, path+"/") }
	for _, c := range changes {
		if under(c.path) || c.oldPath != "" && under(c.oldPath) {
			return true
		}
	}
	return false
}

// movedPath is where path is once a move from from to to moved it.
func movedPath(path, from, to string) string {
	if path == from {
		return to
	}
	if strings.HasPrefix(path, from+"/") {
		return to + strings.TrimPrefix(path, from)
	}
	return path
}

// An importedCommit is a commit of HEAD's history and the changes the import
// records for it.
type importedCommit struct {
	id      gitobj.ID
	parents []gitobj.ID
	merge   bool
	subject string
	diff    []treeChange      // against the first parent, in the import's order
	renames map[string]string // added path → the deleted path Git pairs it with
	changes []importedChange
}

// An importedChange is one entry's change in a commit: its path, and for a
// rename the path it had.
type importedChange struct{ path, oldPath string }

// A treeChange is a path whose file differs from the first parent's.
type treeChange struct {
	path     string
	old, new *gitFile
}

// gitImportedChanges reads the commits of HEAD's history in gitDir as the
// import reads them (gitexport.ImportGitDir): git log --reverse --topo-order
// with each commit's changes against its first parent and the renames Git
// detects among them. It gives each file the entry the import gives it
// (notestore's ImportCommit): a path the first parent had keeps its entry;
// a new path takes the identity its content declares
// (workspace.ImportEntryID) while no other file holds it, else the entry of
// the deleted file Git pairs it with, else of an identical deleted file,
// else a new one. Then it lists each commit's changes by entry, as the store
// keeps them for history: an entry whose path changed is a rename.
func gitImportedChanges(ctx context.Context, gitDir string, head gitobj.ID) ([]importedCommit, error) {
	commits, err := gitLog(ctx, gitDir, head)
	if err != nil {
		return nil, err
	}
	blobs, err := openGitBlobs(ctx, gitDir)
	if err != nil {
		return nil, err
	}
	defer blobs.close()
	// A commit whose first parent is not the commit before it starts from
	// that parent's files, which are kept for it.
	keep := map[gitobj.ID]bool{}
	for i, c := range commits {
		if len(c.parents) > 0 && (i == 0 || commits[i-1].id != c.parents[0]) {
			keep[c.parents[0]] = true
		}
	}
	saved := map[gitobj.ID]map[string]string{}
	files := map[string]string{} // path → entry
	for i := range commits {
		c := &commits[i]
		if len(c.parents) == 0 {
			files = map[string]string{}
		} else if i == 0 || commits[i-1].id != c.parents[0] {
			parent, ok := saved[c.parents[0]]
			if !ok {
				return nil, fmt.Errorf("commit %s comes before its parent %s in git log --topo-order", c.id, c.parents[0])
			}
			files = maps.Clone(parent)
		}
		claimed, leaving, deleted := map[string]bool{}, map[string]bool{}, map[string]bool{}
		for _, d := range c.diff {
			if d.old != nil {
				leaving[files[d.path]] = true
				deleted[d.path] = d.new == nil
			}
		}
		for _, entry := range files {
			claimed[entry] = true
		}
		for entry := range leaving {
			delete(claimed, entry)
		}
		assigned := map[string]string{}
		for _, d := range c.diff {
			if d.new != nil && d.old != nil {
				entry := files[d.path]
				assigned[d.path], claimed[entry] = entry, true
			}
		}
		for _, d := range c.diff {
			if d.new == nil || d.old != nil {
				continue
			}
			entry := ""
			// ImportEntryID reads only notes, so only theirs are read.
			if d.new.mode != gitobj.ModeGitlink && strings.HasSuffix(d.path, ".md") {
				content, err := blobs.read(d.new.blob)
				if err != nil {
					return nil, err
				}
				if declared := workspace.ImportEntryID(d.path, content); declared != "" && !claimed[declared] {
					entry = declared
				}
			}
			if from, ok := c.renames[d.path]; entry == "" && ok && deleted[from] && !claimed[files[from]] {
				entry = files[from]
			}
			if entry == "" {
				for _, o := range c.diff {
					if o.old != nil && o.new == nil && o.old.blob == d.new.blob && !claimed[files[o.path]] {
						entry = files[o.path]
						break
					}
				}
			}
			if entry == "" {
				sum := sha1.Sum(append(bytes.Clone(c.id[:]), d.path...))
				entry = hex.EncodeToString(sum[:16])
			}
			assigned[d.path], claimed[entry] = entry, true
		}
		before, after := map[string]string{}, map[string]string{} // entry → path
		for _, d := range c.diff {
			if d.old != nil {
				before[files[d.path]] = d.path
			}
			if d.new == nil {
				delete(files, d.path)
			}
		}
		for _, d := range c.diff {
			if d.new != nil {
				files[d.path], after[assigned[d.path]] = assigned[d.path], d.path
			}
		}
		for entry, was := range before {
			now, has := after[entry]
			switch {
			case !has:
				c.changes = append(c.changes, importedChange{path: was})
			case now != was:
				c.changes = append(c.changes, importedChange{path: now, oldPath: was})
			default:
				c.changes = append(c.changes, importedChange{path: now})
			}
		}
		for entry, now := range after {
			if _, had := before[entry]; !had {
				c.changes = append(c.changes, importedChange{path: now})
			}
		}
		c.diff, c.renames = nil, nil
		if keep[c.id] {
			saved[c.id] = maps.Clone(files)
		}
	}
	return commits, nil
}

// gitLog reads the commits of head's history in gitDir, parents first in
// git log --reverse --topo-order order, the import's, each with its subject
// and its changes against its first parent with the renames Git detects, in
// the order the import's tree walk meets them (treeOrder).
func gitLog(ctx context.Context, gitDir string, head gitobj.ID) ([]importedCommit, error) {
	var commits []importedCommit
	var c *importedCommit
	subject, paths := false, 0
	var status string
	var old, new *gitFile
	var from string
	err := cutoverStream(ctx, gitDir, func(field string) error {
		switch {
		case subject:
			c.subject, subject = field, false
			return nil
		case paths > 0:
			paths--
			if paths == 1 {
				from = field
				return nil
			}
			switch status[0] {
			case 'A', 'C':
				c.diff = append(c.diff, treeChange{path: field, new: new})
			case 'D':
				c.diff = append(c.diff, treeChange{path: field, old: old})
			case 'M', 'T':
				c.diff = append(c.diff, treeChange{path: field, old: old, new: new})
			case 'R':
				c.diff = append(c.diff, treeChange{path: from, old: old}, treeChange{path: field, new: new})
				c.renames[field] = from
			default:
				return fmt.Errorf("unexpected change %q in git log output", status)
			}
			return nil
		}
		field = strings.TrimLeft(field, "\n")
		if header, ok := strings.CutPrefix(field, "\x01"); ok {
			ids := strings.Fields(header)
			if len(ids) == 0 {
				return fmt.Errorf("unexpected git log output %q", field)
			}
			commits = append(commits, importedCommit{renames: map[string]string{}})
			c, subject = &commits[len(commits)-1], true
			var err error
			if c.id, err = gitobj.ParseID(ids[0]); err != nil {
				return err
			}
			for _, p := range ids[1:] {
				id, err := gitobj.ParseID(p)
				if err != nil {
					return err
				}
				c.parents = append(c.parents, id)
			}
			c.merge = len(c.parents) > 1
			return nil
		}
		raw, ok := strings.CutPrefix(field, ":")
		parts := strings.Fields(raw)
		if !ok || c == nil || len(parts) != 5 || parts[4] == "" {
			if field == "" {
				return nil
			}
			return fmt.Errorf("unexpected git log output %q", field)
		}
		status = parts[4]
		var sides [2]*gitFile
		for i := range sides {
			mode, err := strconv.ParseUint(parts[i], 8, 32)
			if err != nil {
				return err
			}
			blob, err := gitobj.ParseID(parts[2+i])
			if err != nil {
				return err
			}
			if mode != 0 {
				sides[i] = &gitFile{mode: uint32(mode), blob: blob}
			}
		}
		old, new = sides[0], sides[1]
		paths = 1
		if status[0] == 'R' || status[0] == 'C' {
			paths = 2
		}
		return nil
	}, "log", "--reverse", "--topo-order", "--root", "--no-abbrev", "-z", "--format=%x01%H %P%x00%s", "--raw", "--find-renames",
		"--diff-merges=first-parent", "--no-color", "--no-ext-diff", "--no-show-signature", head.String(), "--")
	if err == nil && (subject || paths > 0) {
		err = errors.New("git log output ends part way through a commit")
	}
	for i := range commits {
		slices.SortStableFunc(commits[i].diff, func(a, b treeChange) int { return treeOrder(a.path, b.path) })
	}
	return commits, err
}

// treeOrder orders paths as the import's tree walk meets them: name by name,
// and a folder's files before a file of the folder's name.
func treeOrder(a, b string) int {
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	for i := range min(len(as), len(bs)) {
		if c := strings.Compare(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(bs), len(as))
}

// gitBlobs reads blobs from a repository through one git cat-file --batch.
type gitBlobs struct {
	in     io.WriteCloser
	out    *bufio.Reader
	wait   func() error
	cancel context.CancelFunc
}

func openGitBlobs(ctx context.Context, gitDir string) (*gitBlobs, error) {
	ctx, cancel := context.WithCancel(ctx)
	cmd := cutoverCommand(ctx, gitDir, "", "cat-file", "--batch")
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
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	return &gitBlobs{in: in, out: bufio.NewReaderSize(out, 64<<10), wait: cmd.Wait, cancel: cancel}, nil
}

func (b *gitBlobs) read(id gitobj.ID) ([]byte, error) {
	if _, err := fmt.Fprintf(b.in, "%s\n", id); err != nil {
		return nil, err
	}
	header, err := b.out.ReadString('\n')
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(header)
	if len(fields) != 3 || fields[0] != id.String() || fields[1] != "blob" {
		return nil, fmt.Errorf("git cat-file: blob %s: %q", id, strings.TrimSpace(header))
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return nil, err
	}
	content := make([]byte, size+1)
	if _, err := io.ReadFull(b.out, content); err != nil {
		return nil, err
	}
	return content[:size], nil
}

func (b *gitBlobs) close() {
	b.in.Close()
	b.wait()
	b.cancel()
}

// cutoverStream runs Git read-only on gitDir, as cutoverGit does, and calls
// fn with each NUL-terminated field of its output, so output of any length
// is never held whole.
func cutoverStream(ctx context.Context, gitDir string, fn func(field string) error, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	cmd := cutoverCommand(ctx, gitDir, "", args...)
	stderr := &procgroup.Output{Limit: 64 << 10}
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	fields := bufio.NewScanner(out)
	fields.Buffer(make([]byte, 64<<10), 1<<20)
	fields.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, 0); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	var fnErr error
	for fnErr == nil && fields.Scan() {
		fnErr = fn(fields.Text())
	}
	if fnErr == nil {
		fnErr = fields.Err()
	}
	if fnErr != nil {
		cancel()
	}
	if err := cmd.Wait(); err != nil && fnErr == nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return fnErr
}
