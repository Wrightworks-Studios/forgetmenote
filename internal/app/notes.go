package app

import (
	"context"
	"path/filepath"
	"slices"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/notestore"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"
)

// Notes is an instance's notes store, which holds every notebook's content
// and history in Postgres and the blob store, and, once Export runs, the
// exporter that mirrors each notebook into a bare Git repository under
// DATA/exports, which backups bundle and the backup worker pushes.
type Notes struct {
	Store    *notestore.Store
	Exporter *gitexport.Exporter
	release  func()
}

// OpenNotes opens the notes store of db's instance, keeping content blocks
// in blobs.
func OpenNotes(ctx context.Context, db *pg.DB, blobs blob.Store) (*Notes, error) {
	store, err := notestore.Open(ctx, db, blobs, notestore.Options{ImportEntryID: workspace.ImportEntryID})
	if err != nil {
		return nil, err
	}
	return &Notes{Store: store}, nil
}

// Export mirrors the store's notebooks into data/exports, which one process
// at a time may hold, and makes it the source of their backups. It exports
// on demand, for each backup, until Exporter.Start runs it in the background.
func (n *Notes) Export(data string, o gitexport.Options) error {
	exporter, err := gitexport.New(n.Store, filepath.Join(data, "exports"), o)
	if err != nil {
		return err
	}
	n.Exporter, n.release = exporter, workspace.UseExporter(n.Store, exporter)
	return nil
}

// Close stops the export, waiting for its background work, and then the
// store. Callers close the catalog and every workspace store first.
func (n *Notes) Close() {
	if n.Exporter != nil {
		n.release()
		n.Exporter.Close()
	}
	n.Store.Close()
}

// catalogNotebooks lists every notebook the catalog holds, so the export's
// catch-up covers those not changed since this process started.
func catalogNotebooks(c *catalog.Catalog) func(context.Context) ([]string, error) {
	return func(context.Context) ([]string, error) {
		var ids []string
		for _, p := range c.List(catalog.Bootstrap) {
			ids = append(ids, p.ID)
		}
		slices.Sort(ids)
		return ids, nil
	}
}
