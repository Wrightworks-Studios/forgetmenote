package main

import (
	"context"

	runtimeapp "forgetmenote/internal/app"
	"forgetmenote/internal/blob"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/gitexport"
	"forgetmenote/internal/pg"
)

// openCatalog opens a stopped instance's catalog over its notes store, as
// serving it does: content blocks in the configured blob store, and the Git
// export of its notebooks under data/exports, which a backup bundles. The
// returned function closes them in order.
func openCatalog(ctx context.Context, db *pg.DB, blobs *blob.Config, data string, browseExcluded []string) (*catalog.Catalog, func(), error) {
	store, err := openBlobs(ctx, blobs, data)
	if err != nil {
		return nil, nil, err
	}
	notes, err := runtimeapp.OpenNotes(ctx, db, store)
	if err != nil {
		closeBlobs(store)
		return nil, nil, err
	}
	c, err := catalog.OpenWithNotes(data, browseExcluded, db, notes.Store)
	if err == nil {
		if err = notes.Export(data, gitexport.Options{}); err != nil {
			c.Close()
		}
	}
	if err != nil {
		notes.Close()
		closeBlobs(store)
		return nil, nil, err
	}
	return c, func() { c.Close(); notes.Close(); closeBlobs(store) }, nil
}
