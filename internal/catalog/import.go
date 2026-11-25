package catalog

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"forgetmenote/internal/workspace"
)

// Import publishes a new notebook only after a complete, validated restore
// of the Git bundle into the notes store, with every commit keeping its ID.
// The restore happens outside the catalog lock while the ID stays reserved,
// in this process and in the database for every gateway (reserveNotebook);
// permissions and the ID are checked again immediately before publication,
// and a restore the catalog does not publish is removed.
func (c *Catalog) Import(ctx context.Context, actor, id, name string, bundle io.Reader) error {
	c.lock()
	err := c.checkImport(actor, id, name)
	if err == nil {
		err = c.checkNoRepository(id)
	}
	if err == nil {
		var release func()
		if release, err = c.reserveNotebook(id); err == nil {
			defer release()
		}
	}
	if err == nil {
		if c.importing == nil {
			c.importing = map[string]bool{}
		}
		c.importing[id] = true
		defer func() { c.mu.Lock(); delete(c.importing, id); c.mu.Unlock() }()
	}
	c.mu.Unlock()
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp(c.data, ".notebook-import-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := workspace.ImportBundleDB(ctx, c.notes, id, stage, bundle); err != nil {
		return err
	}
	// The reservation keeps every other creation and import of the ID out,
	// so the notebook stored now is this import's.
	_, created, err := c.notes.Notebook(id).StoredIncarnation(context.WithoutCancel(ctx))
	c.lock()
	defer c.mu.Unlock()
	delete(c.importing, id)
	var store *workspace.Store
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = c.checkImport(actor, id, name)
	}
	if err == nil {
		store, err = c.openStore(id)
	}
	if err == nil {
		c.state.Projects[id] = Project{ID: id, Name: strings.TrimSpace(name), Members: map[string]string{actor: "owner"}}
		if err = c.persist(); err != nil {
			delete(c.state.Projects, id)
		}
	}
	if err != nil {
		// A failed commit may still have published the notebook: then it
		// stays until the catalog shows whether it did.
		return c.abandonNotebook(id, store, created, err)
	}
	c.stores[id] = store
	return nil
}

func (c *Catalog) checkImport(actor, id, name string) error {
	if !c.canCreateProjects(actor) {
		return workspace.ErrForbidden
	}
	if !projectID.MatchString(id) || strings.TrimSpace(name) == "" || len(name) > 120 {
		return fmt.Errorf("%w: use a lowercase notebook ID and a name of at most 120 characters", workspace.ErrBundle)
	}
	if ReservedNotebookID(id) {
		return fmt.Errorf("%w: notebook IDs beginning with inbox- or assistant- are reserved for personal notebooks", workspace.ErrBundle)
	}
	if _, ok := c.state.Projects[id]; ok || c.importing[id] {
		return fmt.Errorf("%w: a notebook with this ID already exists; choose a different ID", workspace.ErrConflict)
	}
	if len(c.state.Projects) >= 256 {
		return fmt.Errorf("%w: notebook limit reached", workspace.ErrBundle)
	}
	return nil
}
