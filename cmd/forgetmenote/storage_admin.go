package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/procgroup"
	"forgetmenote/internal/workspace"
)

// runStorageAdmin runs the storage v2 cutover commands, all but check on a
// stopped instance:
//
//   - import stores every notebook repository under DATA/projects/ID/workspace
//     in the configured Postgres and blob store with its exact commits, then
//     verifies it. A notebook the store already has is never imported over:
//     with the same head it is verified again, otherwise refused, unless an
//     import of it stopped part way, which is then resumed.
//   - verify proves each stored notebook holds exactly its repository's
//     history.
//   - export brings each notebook's Git export, DATA/exports/ID.git, up to its
//     head, repacks and checks it, and verifies it against the store. An
//     export that no longer matches its notebook fails; with -recreate it is
//     deleted and exported again from scratch.
//   - check proves, once the new release has started and before anyone
//     writes, that the cutover succeeded for every notebook the catalog
//     lists or DATA/projects holds (checkNotebook): the history, the export,
//     every file at HEAD through the server's read path, and each note's
//     whole history. It only reads, so it runs beside the gateways; it fails
//     on any difference, when the notebook -notebook names does not exist,
//     for a notebook the catalog lists whose repository is missing though
//     retire never removed it, and when it compared no notebook at all, so
//     that it never passes having proved nothing.
//   - retire removes the previous release's files of each notebook, once the
//     rollback window has closed: its Git working tree with its .git, its
//     live documents and its capture records. It removes them only with -yes
//     and only for a notebook whose store verifiably holds its repository's
//     whole history; otherwise, as with -dry-run, it reports what it would
//     remove. It records each removal (catalog.RecordStorageRetired). The
//     runbook retires them as soon as check passes.
//
// Each notebook's outcome is one JSON line, and check and retire end with a
// summary line; any failure fails the command.
func runStorageAdmin(args []string, stdout io.Writer) error {
	usage := errors.New("usage: forgetmenote admin storage import|verify|export|check|retire [-data DIR] [-notebook ID] [-recreate] [-dry-run|-yes] (stop the instance first, except for check)")
	if len(args) == 0 || !slices.Contains([]string{"import", "verify", "export", "check", "retire"}, args[0]) {
		return usage
	}
	command := args[0]
	flags := flag.NewFlagSet("admin storage "+command, flag.ContinueOnError)
	data := flags.String("data", env("DATA_DIR", "/data"), "stopped instance data directory")
	only := flags.String("notebook", "", "only this notebook ID")
	var recreate, dryRun, yes *bool
	switch command {
	case "export":
		recreate = flags.Bool("recreate", false, "delete each export and export it again from scratch, to repair one that diverged or fails its checks")
	case "retire":
		dryRun = flags.Bool("dry-run", false, "only report what would be removed and the verification results (the default)")
		yes = flags.Bool("yes", false, "remove the previous release's files of every notebook that verifies")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || command == "retire" && *dryRun && *yes {
		return usage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, dir, config, err := stateDatabase(ctx, flags, *data)
	if err != nil {
		return err
	}
	defer db.Close()
	// Only check runs beside a serving gateway: it reads, and changes nothing.
	// Retire stays refused: a storage v2 gateway reads each repository's HEAD
	// while it starts (catalog's checkImported), which a removal under it can
	// fail, and a previous release's gateway still running commits into the
	// repositories; presence does not tell the two apart.
	if command != "check" {
		if live, err := db.LiveGateways(ctx, db.Pool); err != nil || live > 0 {
			if err == nil {
				err = fmt.Errorf("%d gateways are serving; stop the instance first", live)
			}
			return err
		}
	}
	var ids []string
	var cataloged map[string]bool
	if command == "check" {
		ids, cataloged, err = checkNotebooks(ctx, db, dir, *only)
	} else {
		ids, err = storageNotebooks(dir, *only)
	}
	if err != nil {
		return err
	}
	blobs, err := openBlobs(ctx, config.Server.Blobs, dir)
	if err != nil {
		return err
	}
	defer closeBlobs(blobs)
	store, err := notestore.Open(ctx, db, blobs, notestore.Options{ImportEntryID: workspace.ImportEntryID})
	if err != nil {
		return err
	}
	defer store.Close()
	var exporter *gitexport.Exporter
	if command == "export" {
		if exporter, err = gitexport.New(store, filepath.Join(dir, "exports"), gitexport.Options{}); err != nil {
			return err
		}
		defer exporter.Close()
	}
	out := json.NewEncoder(stdout)
	failed := 0
	statuses := map[string]int{}
	var checked checkTally
	for _, id := range ids {
		result := map[string]any{"notebook": id}
		started := time.Now()
		switch command {
		case "import":
			err = importNotebook(ctx, db, store, dir, id, result)
		case "verify":
			err = verifyNotebook(ctx, store, dir, id, result)
		case "export":
			err = exportNotebook(ctx, store, exporter, id, *recreate, result)
		case "check":
			err = checkNotebook(ctx, db, store, dir, id, cataloged[id], result)
		case "retire":
			err = retireNotebook(ctx, db, store, dir, id, *yes, result)
		}
		result["seconds"] = time.Since(started).Round(time.Millisecond).Seconds()
		if err != nil {
			result["error"] = err.Error()
			failed++
		}
		if status, _ := result["status"].(string); status != "" {
			statuses[status]++
		}
		if command == "check" {
			checked.add(result, err)
		}
		if err := out.Encode(result); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if command == "import" && statuses["imported"]+statuses["resumed"] > 0 {
		// Autovacuum gathers the statistics of the rows just stored only
		// later. Until then Postgres plans a note's history as a scan of the
		// notebook's changes per revision, about a hundred times slower, which
		// check and the new release's first history reads would pay.
		if _, err := db.Pool.Exec(ctx, "ANALYZE notebook_heads, revisions, git_trees, file_versions, content_segments, content_blocks, revision_changes, entries"); err != nil {
			return err
		}
	}
	switch command {
	case "check":
		if err := out.Encode(checked.summary(len(ids), statuses)); err != nil {
			return err
		}
	case "retire":
		if err := out.Encode(map[string]any{"summary": statuses, "notebooks": len(ids), "dryRun": !*yes}); err != nil {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("storage %s failed for %d of %d notebooks", command, failed, len(ids))
	}
	// A check passes only if it compared a notebook: every one it found was
	// skipped, as after retire, or it found none.
	if command == "check" && checked.compared == 0 {
		return checked.nothingCompared(len(ids), statuses)
	}
	return nil
}

// storageNotebooks lists the notebooks under data/projects, or only.
func storageNotebooks(data, only string) ([]string, error) {
	if only != "" {
		if !workspace.ValidNotebookID(only) {
			return nil, fmt.Errorf("invalid notebook ID %q", only)
		}
		return []string{only}, nil
	}
	entries, err := os.ReadDir(filepath.Join(data, "projects"))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		if entry.IsDir() && workspace.ValidNotebookID(entry.Name()) {
			ids = append(ids, entry.Name())
		}
	}
	slices.Sort(ids)
	return ids, nil
}

func importNotebook(ctx context.Context, db *pg.DB, store *notestore.Store, data, id string, result map[string]any) error {
	project := filepath.Join(data, "projects", id)
	gitDir := filepath.Join(project, "workspace", ".git")
	if _, err := os.Stat(gitDir); errors.Is(err, fs.ErrNotExist) {
		result["status"] = "skipped: no notebook repository"
		return nil
	}
	if err := checkQuiescent(ctx, project); err != nil {
		return err
	}
	source, err := cutoverGit(ctx, gitDir, "", "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return err
	}
	nb := store.Notebook(id)
	unfinished, err := catalog.StorageImportUnfinished(ctx, db, id)
	if err != nil {
		return err
	}
	if head, err := nb.Head(ctx); err == nil {
		if head.Commit.String() == source {
			result["status"] = "already imported"
			if err = verified(ctx, nb, gitDir, result); err == nil {
				err = catalog.FinishStorageImport(ctx, db, id, source)
			}
			return err
		}
		if !unfinished {
			return fmt.Errorf("the store already has this notebook at %s, not at Git HEAD %s, and its import finished, so it is never imported over: the repository changed after the import, or the notebook did; to import it again, restore the database from the dump taken at the freeze and run the import for every notebook (docs/storage-cutover.md)", head.Commit, source)
		}
		// An import that stopped part way, killed or cut off from the
		// database, left a prefix of HEAD's history: a commit of the
		// repository, as no other writer runs while the import is unfinished.
		if _, err := cutoverGit(ctx, gitDir, "", "merge-base", "--is-ancestor", head.Commit.String(), source); err != nil {
			return fmt.Errorf("the store has this notebook at %s from an import that did not finish, but that is not a commit of Git HEAD %s's history, so the import cannot resume; restore the database from the dump taken at the freeze and run the import for every notebook (docs/storage-cutover.md)", head.Commit, source)
		}
		if _, err = gitexport.ResumeGitDir(ctx, nb, gitDir); err == nil {
			err = verified(ctx, nb, gitDir, result)
		}
		if err != nil {
			return fmt.Errorf("%w; run the import again to resume it", err)
		}
		result["status"] = "resumed"
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	} else {
		if err = catalog.BeginStorageImport(ctx, db, id, source); err != nil {
			return err
		}
		if _, err = gitexport.ImportGitDir(ctx, nb, gitDir); err == nil {
			err = verified(ctx, nb, gitDir, result)
		}
		if err != nil {
			// What this run stored is removed, so a rerun starts afresh; if
			// that fails too, the rerun resumes it.
			if errors.Is(err, gitexport.ErrNotEmpty) {
				return err
			}
			removeCtx := context.WithoutCancel(ctx)
			removed := store.Delete(removeCtx, id)
			if removed == nil || errors.Is(removed, fs.ErrNotExist) {
				removed = catalog.FinishStorageImport(removeCtx, db, id, "")
			}
			if removed != nil {
				err = errors.Join(err, fmt.Errorf("removing the partial import: %w; run the import again to resume it", removed))
			}
			return err
		}
		result["status"] = "imported"
	}
	if err := catalog.FinishStorageImport(ctx, db, id, source); err != nil {
		return err
	}
	// Only HEAD's history moves; name what else the repository holds.
	refs, err := cutoverGit(ctx, gitDir, "", "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		return err
	}
	var outside []string
	for _, line := range strings.Split(refs, "\n") {
		sha, name, ok := strings.Cut(line, " ")
		if ok {
			if _, err := cutoverGit(ctx, gitDir, "", "merge-base", "--is-ancestor", sha, source); err != nil {
				outside = append(outside, name)
			}
		}
	}
	if len(outside) > 0 {
		result["notImported"] = outside
	}
	return nil
}

func verifyNotebook(ctx context.Context, store *notestore.Store, data, id string, result map[string]any) error {
	gitDir := filepath.Join(data, "projects", id, "workspace", ".git")
	if _, err := os.Stat(gitDir); errors.Is(err, fs.ErrNotExist) {
		result["status"] = "skipped: no notebook repository"
		return nil
	}
	return verified(ctx, store.Notebook(id), gitDir, result)
}

func verified(ctx context.Context, nb *notestore.Notebook, gitDir string, result map[string]any) error {
	rep, err := gitexport.Verify(ctx, nb, gitDir)
	if err != nil {
		return err
	}
	if result["status"] == nil {
		result["status"] = "verified"
	}
	result["head"], result["commits"], result["trees"], result["blobs"] = rep.Head.String(), rep.Commits, rep.Trees, rep.Blobs
	return nil
}

func exportNotebook(ctx context.Context, store *notestore.Store, exporter *gitexport.Exporter, id string, recreate bool, result map[string]any) error {
	export, status := exporter.Sync, "exported"
	if recreate {
		export, status = exporter.Recreate, "recreated"
	}
	head, err := export(ctx, id)
	if errors.Is(err, fs.ErrNotExist) {
		result["status"] = "skipped: not in the notebook store"
		return nil
	}
	if err != nil {
		return err
	}
	if err = exporter.Maintain(ctx, id); err != nil {
		return err
	}
	result["status"], result["repository"] = status, exporter.Path(id)
	if err = verified(ctx, store.Notebook(id), exporter.Path(id), result); err == nil && result["head"] != head.Commit.String() {
		err = fmt.Errorf("the export is at %s, the notebook at %s", result["head"], head.Commit)
	}
	return err
}

// previousReleaseFiles are the files the previous release kept for each
// notebook in DATA/projects/ID and storage v2 never writes: the Git working
// tree with its .git, the live documents and the capture records.
var previousReleaseFiles = []string{"workspace", "collaboration", "captures"}

// retiredFiles is where retire moves a notebook's previous release files,
// each in one rename, before it deletes them: a removal that stops part way
// leaves no repository behind that startup would read (checkImported), and
// the next retire deletes what is left.
const retiredFiles = ".previous-release-retired"

// retireNotebook removes notebook id's previous release files, with remove,
// once the notes store verifiably holds the history of their repository's
// HEAD (gitexport.VerifyHeld): every commit, or the commit a recorded purge
// made of it, and HEAD's tree, unless a purge rewrote it. It removes nothing
// of a notebook that fails, nor of one whose files hold work their history
// lacks (retireCheck); without remove it only reports.
func retireNotebook(ctx context.Context, db *pg.DB, store *notestore.Store, data, id string, remove bool, result map[string]any) (err error) {
	project := filepath.Join(data, "projects", id)
	var names, paths []string
	for _, name := range append(slices.Clone(previousReleaseFiles), retiredFiles) {
		if _, err := os.Lstat(filepath.Join(project, name)); err == nil {
			names, paths = append(names, name), append(paths, filepath.Join(project, name))
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if len(names) == 0 {
		result["status"] = "skipped: no previous release files"
		return nil
	}
	result["files"] = paths
	status := "kept"
	defer func() {
		if err != nil {
			result["status"] = status
		}
	}()
	unfinished, err := catalog.StorageImportUnfinished(ctx, db, id)
	if err != nil {
		return err
	}
	if unfinished {
		return errors.New("its storage import did not finish; run `forgetmenote admin storage import` again, which resumes and verifies it")
	}
	if err := retireCheck(ctx, project); err != nil {
		return err
	}
	gitDir := filepath.Join(project, "workspace", ".git")
	if _, err := os.Lstat(gitDir); err == nil {
		outside, err := refsOutsideHead(ctx, gitDir)
		if err != nil {
			return err
		}
		if len(outside) > 0 {
			return fmt.Errorf("the repository has refs the store lacks, %s: the import keeps only the history of HEAD, and no annotated tag; keep them (git bundle create FILE --all), then remove the files by hand", strings.Join(outside, ", "))
		}
		rep, err := gitexport.VerifyHeld(ctx, store.Notebook(id), gitDir)
		if errors.Is(err, fs.ErrNotExist) {
			err = errors.New("the notes store does not hold this notebook, so nothing shows it holds the repository's history; import it first (docs/storage-cutover.md)")
		}
		if err != nil {
			return err
		}
		result["head"], result["commits"], result["rewritten"], result["trees"], result["blobs"] = rep.Head.String(), rep.Commits, rep.Rewritten, rep.Trees, rep.Blobs
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if !remove {
		result["status"] = "would remove"
		return nil
	}
	// The verification can take minutes: refuse a gateway that started
	// meanwhile, as at the start.
	if live, err := db.LiveGateways(ctx, db.Pool); err != nil || live > 0 {
		if err == nil {
			err = fmt.Errorf("%d gateways started serving; stop the instance and run retire again", live)
		}
		return err
	}
	// Recorded first, so check never takes a repository retire removed, even
	// part way, for one that went missing.
	head, _ := result["head"].(string)
	if err := catalog.RecordStorageRetired(ctx, db, id, head); err != nil {
		return err
	}
	status = "removal failed"
	retired := filepath.Join(project, retiredFiles)
	if err := os.RemoveAll(retired); err != nil {
		return err
	}
	if err := os.Mkdir(retired, 0o700); err != nil {
		return err
	}
	for _, name := range names {
		if name == retiredFiles {
			continue
		}
		if err := os.Rename(filepath.Join(project, name), filepath.Join(retired, name)); err != nil {
			return fmt.Errorf("%w; fix this and run retire again, which deletes what it moved to %s", err, retired)
		}
	}
	if err := os.RemoveAll(retired); err != nil {
		return fmt.Errorf("%w; fix this and run retire again, which deletes what is left in %s", err, retired)
	}
	result["status"] = "removed"
	return nil
}

// refsOutsideHead lists the refs of the repository at gitDir that hold
// anything the history of its HEAD does not: a branch or tag at a commit
// outside that history, such as a stash, or an annotated tag, whose tag
// object no import stores.
func refsOutsideHead(ctx context.Context, gitDir string) ([]string, error) {
	head, err := cutoverGit(ctx, gitDir, "", "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, err
	}
	refs, err := cutoverGit(ctx, gitDir, "", "for-each-ref", "--format=%(objectname) %(objecttype) %(refname)")
	if err != nil {
		return nil, err
	}
	var outside []string
	for _, line := range strings.Split(refs, "\n") {
		fields := strings.SplitN(line, " ", 3)
		if len(fields) != 3 {
			continue
		}
		if fields[1] != "commit" {
			outside = append(outside, fields[2])
		} else if _, err := cutoverGit(ctx, gitDir, "", "merge-base", "--is-ancestor", fields[0], head); err != nil {
			outside = append(outside, fields[2])
		}
	}
	return outside, nil
}

// retireCheck refuses the previous release files of the notebook in project
// while they hold work its Git history lacks, which removing them would
// lose: an interrupted write, live edits not checkpointed, or a file in the
// working tree that is not committed there (ignored files included), or in
// a working tree that has no repository. The import refused each of these,
// so they are there only when something changed the files since.
func retireCheck(ctx context.Context, project string) error {
	kept := func(what string) error {
		return fmt.Errorf("%s, which only the previous release's files hold, so they are kept; inspect them, keep what you need, and then remove them by hand", what)
	}
	if _, err := os.Lstat(filepath.Join(project, "write-journal.json")); err == nil {
		return kept("an interrupted write is pending (write-journal.json)")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	pending, err := pendingLiveEdits(filepath.Join(project, "collaboration"))
	if err != nil {
		return err
	}
	if pending != "" {
		return kept(fmt.Sprintf("live edits are not checkpointed (%s)", pending))
	}
	workTree := filepath.Join(project, "workspace")
	if _, err := os.Lstat(filepath.Join(workTree, ".git")); err == nil {
		status, err := cutoverGit(ctx, filepath.Join(workTree, ".git"), workTree, "status", "--porcelain", "--untracked-files=all", "--ignored")
		if err != nil {
			return err
		}
		if status != "" {
			return kept("the working tree has files its Git history lacks (git status --ignored lists them)")
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return filepath.WalkDir(workTree, func(path string, d fs.DirEntry, err error) error {
		if path == workTree && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil || d.IsDir() {
			return err
		}
		return kept(fmt.Sprintf("the working tree has no Git repository, so nothing shows the store holds %s", path))
	})
}

// checkQuiescent refuses a notebook whose old storage holds work its Git
// history lacks: an interrupted write, uncommitted files, or live edits not
// checkpointed. The previous release recovers or checkpoints them.
func checkQuiescent(ctx context.Context, project string) error {
	if _, err := os.Lstat(filepath.Join(project, "write-journal.json")); err == nil {
		return errors.New("an interrupted write is pending (write-journal.json); start the previous release once to recover it")
	}
	workTree := filepath.Join(project, "workspace")
	status, err := cutoverGit(ctx, filepath.Join(workTree, ".git"), workTree, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("the notebook repository has uncommitted changes; commit or remove them first")
	}
	pending, err := pendingLiveEdits(filepath.Join(project, "collaboration"))
	if err != nil {
		return err
	}
	if pending != "" {
		return fmt.Errorf("live edits are not checkpointed (%s); checkpoint them with the previous release first", pending)
	}
	return nil
}

// pendingLiveEdits names a live document of the previous release with edits
// its Git history lacks: uncommitted updates in its snapshot, updates its
// append log holds after the snapshot, or an append log with entries but no
// snapshot. It reads the files in that release's live storage format and
// refuses any it cannot read.
func pendingLiveEdits(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case strings.HasSuffix(name, ".jsonl"):
			// An empty log can outlive a crash before its first snapshot;
			// the previous release removes it. Entries without a snapshot
			// are accepted edits an incomplete restore lost the record of.
			if _, err := os.Lstat(filepath.Join(dir, strings.TrimSuffix(name, "l"))); errors.Is(err, fs.ErrNotExist) {
				info, err := entry.Info()
				if err != nil {
					return "", err
				}
				if !info.Mode().IsRegular() || info.Size() > 0 {
					return name, nil
				}
			}
		case strings.HasSuffix(name, ".json"):
			snapshot, err := readLiveSnapshot(filepath.Join(dir, name))
			if err != nil {
				return "", err
			}
			if len(snapshot.Uncommitted) > 0 {
				return snapshot.Path, nil
			}
			appended, err := appendedAfter(filepath.Join(dir, name+"l"), snapshot.Generation, snapshot.Sequence)
			if err != nil || appended {
				return snapshot.Path, err
			}
		}
	}
	return "", nil
}

// liveSnapshot is what the cutover needs of a live document's snapshot.
type liveSnapshot struct {
	Path        string   `json:"path"`
	Generation  string   `json:"generation"`
	Sequence    uint64   `json:"sequence"`
	Uncommitted []string `json:"uncommitted"`
}

// readLiveSnapshot reads a live document's snapshot as the previous release
// stores it: version 2 wraps the record in "data", version 0 (written before
// it, and by its rollback) is the bare record. Any other version is state
// this release does not know, which the cutover refuses.
func readLiveSnapshot(name string) (liveSnapshot, error) {
	fail := func(err error) (liveSnapshot, error) {
		return liveSnapshot{}, fmt.Errorf("collaboration state %s: %w", filepath.Base(name), err)
	}
	raw, err := os.ReadFile(name)
	if err != nil {
		return liveSnapshot{}, err
	}
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return fail(err)
	}
	var snapshot liveSnapshot
	switch header.Version {
	case 0:
		err = json.Unmarshal(raw, &snapshot)
	case 2:
		var wrapped struct {
			Data liveSnapshot `json:"data"`
		}
		err = json.Unmarshal(raw, &wrapped)
		snapshot = wrapped.Data
	default:
		return fail(fmt.Errorf("unknown collaboration storage version %d", header.Version))
	}
	if err == nil && (snapshot.Path == "" || len(snapshot.Generation) != 32) {
		err = errors.New("not a live document snapshot")
	}
	if err != nil {
		return fail(err)
	}
	return snapshot, nil
}

// appendedAfter reports whether the append log at path holds an update of
// generation after sequence, which the previous release replays as
// uncommitted. Like that release it reads complete lines only, since a torn
// last write was never acknowledged, and refuses an entry it cannot read.
func appendedAfter(path, generation string, sequence uint64) (bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, line := range bytes.Split(raw[:bytes.LastIndexByte(raw, '\n')+1], []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var entry struct {
			Generation string `json:"generation"`
			Sequence   uint64 `json:"sequence"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			return false, fmt.Errorf("collaboration append log %s: %w", filepath.Base(path), err)
		}
		if entry.Generation == generation && entry.Sequence > sequence {
			return true, nil
		}
	}
	return false, nil
}

// cutoverGit runs Git read-only on a notebook repository of the previous
// release (cutoverCommand) and returns its output.
func cutoverGit(ctx context.Context, gitDir, workTree string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := cutoverCommand(ctx, gitDir, workTree, args...)
	out, stderr := &procgroup.Output{Limit: 16 << 20}, &procgroup.Output{Limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = out, stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// cutoverCommand is Git run read-only on a notebook repository of the
// previous release: no configuration but the repository's, no hooks, no
// optional index writes.
func cutoverCommand(ctx context.Context, gitDir, workTree string, args ...string) *exec.Cmd {
	cmd := procgroup.Command(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_DIR=" + gitDir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C"}
	cmd.Dir = gitDir
	if workTree != "" {
		cmd.Env = append(cmd.Env, "GIT_WORK_TREE="+workTree)
		cmd.Dir = workTree
	}
	return cmd
}
