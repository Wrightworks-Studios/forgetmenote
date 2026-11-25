package knowledge

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"forgetmenote/internal/pg"
	"forgetmenote/internal/workspace"
)

// BackgroundCheckpoint retains only revision, stored versions and the history
// cache's file metadata. It lets the background scheduler check a cold, fully
// processed notebook without loading its corpus. It never authorizes access or
// supplies results to a caller.
type BackgroundCheckpoint struct {
	revision, visibility string
	db                   *pg.DB
	notebook, dir        string
	versions             [3]int64
	history              os.FileInfo
}

func (e *Engine) BackgroundCheckpoint() *BackgroundCheckpoint {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.hasRecoveryRequirement() || e.historyDirty {
		return nil
	}
	revision, visibility := e.snapshot.Revision, e.snapshotVisibility
	if e.state.Config.Enabled && !e.complete() {
		return nil
	}
	if e.historyResume || e.historyLoaded {
		if !e.historyLoaded || e.history.Head == "" || e.history.Head != e.history.Completed || e.history.Visibility != visibility && e.state.Config.Enabled {
			return nil
		}
		if !e.state.Config.Enabled {
			revision, visibility = e.history.Head, e.history.Visibility
		}
		if e.state.Config.Enabled {
			for _, version := range e.history.Versions {
				for _, passage := range e.historyParts(version) {
					if _, ok := e.cache.Vectors[passage.Hash]; !ok {
						return nil
					}
				}
			}
		}
	}
	if revision == "" || visibility == "" || e.stateVersion < 0 || e.retrievalVersion < 0 {
		return nil
	}
	checkpoint := &BackgroundCheckpoint{revision: revision, visibility: visibility, db: e.db, notebook: e.notebook, dir: e.dir, versions: [3]int64{e.stateVersion, e.retrievalVersion, e.vectorVersion}}
	info, err := os.Lstat(filepath.Join(e.dir, "cache", "history.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && !info.Mode().IsRegular() {
		return nil
	}
	checkpoint.history = info
	return checkpoint
}

// Unchanged reports whether the owner's notes, another engine's stored state and
// the local history cache are all as the checkpoint saw them. It reads only
// versions and file metadata, and holds nothing of the engine it came from.
func (c *BackgroundCheckpoint) Unchanged(ctx context.Context, store *workspace.Store) bool {
	status, err := store.Status(ctx)
	if err != nil || status.Revision != c.revision {
		return false
	}
	visibility, err := store.HistoryVisibilityID(ctx)
	if err != nil || visibility != c.visibility {
		return false
	}
	versions, err := storedVersions(ctx, c.db, c.notebook)
	if err != nil || versions != c.versions {
		return false
	}
	current, err := os.Lstat(filepath.Join(c.dir, "cache", "history.json"))
	if c.history == nil && !errors.Is(err, os.ErrNotExist) || c.history != nil && (err != nil || !sameKnowledgeFile(c.history, current)) {
		return false
	}
	return ctx.Err() == nil
}

// RecoveryRequired is read while an engine lease still protects its processing
// state. The registry can invalidate old idle markers without sharing mutable
// failure fields or doing owner/filesystem I/O under its lock.
func (e *Engine) RecoveryRequired() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hasRecoveryRequirement()
}
func (e *Engine) hasRecoveryRequirement() bool {
	return e.failure != "" || e.syncFailure != "" || e.currentVectorPersistFailure != "" || e.historyVectorPersistFailure != "" || e.historyPersistFailure != "" || e.historyFailure != "" || e.historyOwnerFailure != "" || e.historySyncFailure != "" || e.historyEmbeddingFailure != "" || e.historySubmissionFailure != ""
}
