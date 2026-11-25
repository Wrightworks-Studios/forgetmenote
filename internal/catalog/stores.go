package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"

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
// DATA/projects/ID/workspace, is a name, never a Git working tree.
func (c *Catalog) stateDir(id string) string { return filepath.Join(c.data, "projects", id) }

// openStore opens catalog notebook id from the notes store. A notebook the
// store lacks is refused rather than replaced.
func (c *Catalog) openStore(id string) (*workspace.Store, error) {
	ctx, cancel := context.WithTimeout(context.Background(), catalogTimeout)
	defer cancel()
	store, err := workspace.OpenDB(ctx, c.notes, id, c.stateDir(id), c.browseExcluded)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("notebook %s is in the catalog but not in the notes store; restore it from a backup: %w", id, err)
	}
	return store, err
}

// StoredNotebooks lists the IDs of the notebooks the stored catalog has,
// sorted, reading the document without the catalog's directory lock, which
// a serving gateway holds.
func StoredNotebooks(ctx context.Context, db *pg.DB) ([]string, error) {
	version, body, err := db.ReadDocument(ctx, db.Pool, catalogKind, catalogID, -1)
	if err != nil || version == 0 {
		return nil, err
	}
	var envelope catalogEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	if envelope.Version != catalogStorageVersion {
		return nil, fmt.Errorf("unsupported catalog version %d", envelope.Version)
	}
	return slices.Sorted(maps.Keys(envelope.Data.Projects)), nil
}

// createStore creates notebook id in the notes store for a catalog entry the
// caller then publishes, and reports whether it created it. A notebook the
// store already holds without a catalog entry was left by a creation that
// never published it, after a crash or a publication of unknown outcome that
// did not commit; it is adopted only while it has nothing but its first
// commit, so no one receives content they never created or imported.
func (c *Catalog) createStore(id string) (*workspace.Store, bool, error) {
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
