package catalog

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"
)

// The notes store holds every notebook's content and history; the catalog
// lists the notebooks people can reach. A notebook's state directory,
// DATA/projects/ID, keeps only what is derived from it, and its Root,
// DATA/projects/ID/workspace, is a name, never a Git working tree. There the
// previous release kept each notebook's repository, which the cutover
// (forgetmenote admin storage import) copies into the notes store and leaves
// as it is.
func (c *Catalog) stateDir(id string) string { return filepath.Join(c.data, "projects", id) }

// openStore opens catalog notebook id from the notes store. A notebook the
// store lacks is refused rather than replaced: one that is still a Git
// repository names the import, and nothing converts or changes it here. So
// is one whose storage import did not finish, or whose Git repository holds
// commits the store lacks (checkImported).
func (c *Catalog) openStore(id string) (*workspace.Store, error) {
	ctx, cancel := context.WithTimeout(context.Background(), catalogTimeout)
	defer cancel()
	store, err := workspace.OpenDB(ctx, c.notes, id, c.stateDir(id), c.browseExcluded)
	if err == nil {
		if err = c.checkImported(ctx, id); err != nil {
			store.Close()
			return nil, err
		}
		return store, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return store, err
	}
	if repository, err := c.gitRepository(id); err != nil || repository != "" {
		if err == nil {
			err = fmt.Errorf("notebook %s is still the Git repository %s, which the notes store lacks: stop the instance and run `forgetmenote admin storage import` to copy it into the database (the repository is left as it is)", id, repository)
		}
		return nil, err
	}
	if owner, ok := c.state.RetiredOwners[id]; ok {
		return nil, fmt.Errorf("notebook %s is on the notebook owner %s, which this release removed, and the notes store lacks it: stop the owner and every gateway, and run `forgetmenote admin storage import` on the owner's data directory to copy it into the database (deploy/production/storage-cutover.md)", id, owner)
	}
	return nil, fmt.Errorf("notebook %s is in the catalog but not in the notes store; restore it from a backup: %w", id, err)
}

// checkImported refuses notebook id, which the notes store holds, while its
// storage import has not finished (StorageImportUnfinished), and while the
// Git repository the previous release kept it in is still there and its HEAD
// is not in the notebook's history: the previous release committed to the
// notebook after the cutover imported it, or the import stopped part way.
// Served, the notebook would miss those commits, or be one no import
// verified, and new revisions would build on that history. A HEAD a purge
// rewrote counts as imported, as only a notebook served after the cutover is
// purged. It reads the repository without running Git and never changes it.
func (c *Catalog) checkImported(ctx context.Context, id string) error {
	unfinished, err := StorageImportUnfinished(ctx, c.db, id)
	if err != nil {
		return err
	}
	if unfinished {
		return fmt.Errorf("notebook %s: its storage import did not finish: it stopped part way, or before it verified the notebook. Stop the instance and run `forgetmenote admin storage import` again, which resumes and verifies it (docs/storage-cutover.md)", id)
	}
	gitDir, err := c.gitRepository(id)
	if err != nil || gitDir == "" {
		return err
	}
	head, ok, err := repositoryHead(gitDir)
	if err != nil {
		return fmt.Errorf("notebook %s: cannot read the HEAD of the Git repository the previous release kept it in: %w", id, err)
	}
	if !ok {
		return nil // a branch with no commit holds nothing the store lacks
	}
	nb := c.notes.Notebook(id)
	if _, err = nb.Commit(ctx, head); !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if _, rewritten, err := nb.Rewritten(ctx, head); err != nil || rewritten {
		return err
	}
	stored, err := nb.Head(ctx)
	if err != nil {
		return err
	}
	return fmt.Errorf("notebook %s is at %s in the notes store, but the Git repository %s the previous release kept it in is at %s, which the store lacks: the import stopped part way, or the previous release committed to the notebook after it was imported. Stop the instance and run `forgetmenote admin storage verify`; then run `forgetmenote admin storage import`, which resumes an import that stopped, or else restore the database from the dump taken at the freeze and import every notebook again (docs/storage-cutover.md)",
		id, stored.Commit, gitDir, head)
}

// createStore creates notebook id in the notes store for a catalog entry the
// caller then publishes, and reports whether it created it. A notebook the
// store already holds without a catalog entry was left by a creation that
// never published it, after a crash or a publication of unknown outcome that
// did not commit; it is adopted only while it has nothing but its first
// commit, so no one receives content they never created or imported.
func (c *Catalog) createStore(id string) (*workspace.Store, bool, error) {
	if err := c.checkNoRepository(id); err != nil {
		return nil, false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), catalogTimeout)
	defer cancel()
	store, err := workspace.CreateDB(ctx, c.notes, id, c.stateDir(id), c.browseExcluded)
	if !errors.Is(err, fs.ErrExist) {
		return store, err == nil, err
	}
	nb := c.notes.Notebook(id)
	head, err := nb.Head(ctx)
	if err != nil {
		return nil, false, err
	}
	files, err := nb.Files(ctx, gitobj.ID{})
	if err != nil {
		return nil, false, err
	}
	if head.Seq != 1 || len(files) != 0 {
		return nil, false, fmt.Errorf("%w: the notes store holds an unpublished notebook %s with history; choose a different ID", workspace.ErrConflict, id)
	}
	store, err = workspace.OpenDB(ctx, c.notes, id, c.stateDir(id), c.browseExcluded)
	return store, false, err
}

// abandonStore undoes createStore after the catalog failed to publish
// notebook id, and returns failure with anything that went wrong. It closes
// store, if any; a notebook created is removed as abandonNotebook removes
// it. An adopted notebook is not this call's: it stays for the next load or
// creation to find.
func (c *Catalog) abandonStore(id string, store *workspace.Store, created bool, failure error) error {
	if !created {
		if store != nil {
			store.Close()
		}
		return failure
	}
	return c.abandonNotebook(id, store, 0, failure)
}

// abandonNotebook undoes the creation or import of notebook id, of
// incarnation created only unless created is 0, after the catalog failed to
// publish it, and returns failure with anything that went wrong. It closes
// store, if any, and deletes the notebook and its Git export, so the ID can
// be created again and backs up. A publication of unknown outcome may have
// committed, and a catalog that, reloaded, lists the ID, or cannot be read,
// may name this notebook: then it stays for the next load or creation to
// find, so a gateway never deletes a notebook another one published.
func (c *Catalog) abandonNotebook(id string, store *workspace.Store, created int64, failure error) error {
	if store != nil {
		store.Close()
	}
	if errors.Is(failure, errCommitUnknown) {
		return failure
	}
	c.refresh() // a failed commit forgot the stored version
	if _, listed := c.state.Projects[id]; listed || c.version < 0 {
		return failure
	}
	ctx, cancel := context.WithTimeout(context.Background(), catalogTimeout)
	defer cancel()
	var err error
	if created != 0 {
		if err = c.notes.DeleteIncarnation(ctx, id, created); errors.Is(err, fs.ErrNotExist) {
			return failure // deleted, or someone else's since
		}
	} else if err = workspace.DeleteDB(ctx, c.notes, id); errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if err == nil {
		err = workspace.RemoveExport(ctx, c.notes, id)
	}
	if err != nil {
		return errors.Join(failure, fmt.Errorf("remove the unpublished notebook %s: %w", id, err))
	}
	os.Remove(c.stateDir(id)) // only while empty, as creating it left it
	return failure
}

// reserveNotebook holds notebook ID id for this caller on every gateway of
// the instance until release runs, so that one creation or import of an ID
// runs at a time: a creation holds it while it creates or adopts the
// notebook and publishes it, an import from before it checks that the notes
// store lacks the notebook until it published or removed what it stored.
// Without it, another gateway could create the ID while an import read its
// upload, or adopt a notebook an import was still storing, and the import's
// failure would then delete a published notebook. While another holds the
// ID, it is an ErrConflict error. The reservation is a session of its own
// in the database: if that connection drops, it ends early, and the
// incarnation checks on deletion still hold.
func (c *Catalog) reserveNotebook(id string) (release func(), err error) {
	ctx, cancel := context.WithTimeout(context.Background(), catalogTimeout)
	defer cancel()
	release, ok, err := c.db.TryExclusive(ctx, "notebook-id "+id)
	if err != nil {
		return nil, errors.Join(ErrStorageUnavailable, err)
	}
	if !ok {
		return nil, fmt.Errorf("%w: notebook %s is being created or imported; choose a different ID, or try again shortly", workspace.ErrConflict, id)
	}
	return release, nil
}

// checkNoRepository refuses a new notebook id, created or imported, over the
// Git repository the previous release left for that ID, which stays as it is.
func (c *Catalog) checkNoRepository(id string) error {
	repository, err := c.gitRepository(id)
	if err == nil && repository != "" {
		err = fmt.Errorf("%w: the Git repository %s of an earlier notebook %s is still there; import it with `forgetmenote admin storage import`, or move it away, before creating or importing a notebook with this ID", workspace.ErrConflict, repository, id)
	}
	return err
}

// gitRepository returns the Git directory the previous release kept notebook
// id in under this data directory, or "" when there is none.
func (c *Catalog) gitRepository(id string) (string, error) {
	path := filepath.Join(c.stateDir(id), "workspace", ".git")
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	return path, nil
}

// ownNotes is the notes store Open makes when no caller supplies one: the
// instance's default blob directory, the data directory itself, and the Git
// export of its notebooks in data/exports, which backups bundle from. Its
// exporter runs on demand only, for each backup, without background work.
type ownNotes struct {
	blobs    *blob.Dir
	notes    *notestore.Store
	exporter *gitexport.Exporter
	release  func()
}

func openOwnNotes(data string, db *pg.DB) (*ownNotes, error) {
	ctx, cancel := context.WithTimeout(context.Background(), catalogTimeout)
	defer cancel()
	blobs, err := blob.OpenDir(data)
	if err != nil {
		return nil, err
	}
	notes, err := notestore.Open(ctx, db, blobs, notestore.Options{ImportEntryID: workspace.ImportEntryID})
	if err != nil {
		blobs.Close()
		return nil, err
	}
	exporter, err := gitexport.New(notes, filepath.Join(data, "exports"), gitexport.Options{})
	if err != nil {
		notes.Close()
		blobs.Close()
		return nil, err
	}
	return &ownNotes{blobs: blobs, notes: notes, exporter: exporter, release: workspace.UseExporter(notes, exporter)}, nil
}

func (o *ownNotes) close() {
	o.release()
	o.exporter.Close()
	o.notes.Close()
	o.blobs.Close()
}
