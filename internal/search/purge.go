package search

import (
	"context"
	"errors"
	"path/filepath"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
)

// purgeChannel carries the ID of a notebook whose derived data a purge has
// cleared, so every gateway drops what it derived from the purged content at
// once instead of when it next uses the notebook.
const purgeChannel = "notebook_purges"

// Purged clears the derived data of store's notebook after purge rewrote it,
// with redact applied to the knowledge records that may quote the purged
// content (knowledge.Engine.Purged), then tells every other gateway, even
// when this gateway's clean-up failed, so they stop serving what they cached
// of the purged revisions. Without index, for a purge that left the notebook
// as it was, only those records are redacted.
func (r *Indexes) Purged(ctx context.Context, store *workspace.Store, purge string, redact func([]byte) ([]byte, bool, error), index bool) (knowledge.PurgeCleanup, error) {
	var cleanup knowledge.PurgeCleanup
	engine, release, err := r.Acquire(ctx, store)
	if err == nil {
		cleanup, err = engine.Purged(ctx, purge, redact, index)
		release()
	}
	if !index {
		return cleanup, err
	}
	r.resetSlot(store)
	if r.db != nil {
		if announceErr := r.db.Announce(context.WithoutCancel(ctx), purgeChannel, filepath.Base(filepath.Dir(store.Root))); announceErr != nil {
			err = errors.Join(err, errors.New("the other gateways could not be told of the purge; they clear what they derived from it when they next use the notebook"), announceErr)
		}
	}
	return cleanup, err
}

// resetSlot makes store's open engine derive its index again on the next
// background pass.
func (r *Indexes) resetSlot(store *workspace.Store) {
	r.mu.Lock()
	slot := r.slots[store]
	r.mu.Unlock()
	if slot == nil {
		return
	}
	slot.checkpoint.Store(nil)
	slot.generation.Add(1)
	slot.pending.Store(true)
	slot.reset.Store(true)
	r.signal()
}

// listenPurges hears other gateways' purges until ctx ends.
func (r *Indexes) listenPurges(ctx context.Context) {
	if r.db == nil {
		return
	}
	r.db.Listen(ctx, purgeChannel, r.heardPurge)
}

// heardPurge drops what this gateway holds of notebook's purged content: its
// store's caches of the old revisions and live documents and its cache files
// now, and its open engine on the next background pass, which rereads the
// knowledge state that records the purge. "" is a reconnection, after which
// an engine still finds the purge when it next reads its state; every store
// the gateway serves forgets its caches, as a purge may have been missed,
// whether or not an engine is open on it.
func (r *Indexes) heardPurge(notebook string) {
	if notebook == "" {
		stores := map[*workspace.Store]bool{}
		r.mu.Lock()
		for store := range r.slots {
			stores[store] = true
		}
		r.mu.Unlock()
		if r.catalog != nil {
			for _, p := range r.catalog.List(catalog.Bootstrap) {
				if store, err := r.catalog.Access(catalog.Bootstrap, p.ID, "read"); err == nil && store != nil {
					stores[store] = true
				}
			}
		}
		for store := range stores {
			store.ForgetPurge()
		}
		return
	}
	if !workspace.ValidNotebookID(notebook) || r.catalog == nil {
		return
	}
	store, err := r.catalog.Access(catalog.Bootstrap, notebook, "read")
	if err != nil || store == nil {
		return
	}
	store.ForgetPurge()
	knowledge.RemoveDerivedFiles(filepath.Dir(store.Root))
	r.resetSlot(store)
}
