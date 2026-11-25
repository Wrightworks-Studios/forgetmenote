package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"forgetmenote/internal/workspace"
)

// A purge (docs/purge.md) removes content from every version of a
// notebook, and with it what the knowledge engine derived from that content:
// passages, the keyword index, semantic vectors, the history index and the
// note map, on every gateway. The engine that runs the purge's clean-up
// records the purge in the notebook's knowledge state, which every engine
// rereads: one that finds a purge it has not seen forgets everything it
// derived, and a cache file written before the purge is never loaded again.

// ErrPurged reports a pinned reference to a version a purge removed.
var ErrPurged = errors.New("this version was permanently erased by a purge")

// PurgeCleanup counts what Purged changed in the knowledge state.
type PurgeCleanup struct {
	Proposals   int `json:"proposals"`
	Checkpoints int `json:"checkpoints"`
	Vectors     int `json:"vectors"`
}

// Purged clears what this engine and every other derived from the notebook
// before purge rewrote it. redact rewrites a stored JSON document, removing
// the purged content and mapping revisions the purge rewrote; it is applied
// to the knowledge state, whose proposals and consumer checkpoints may quote
// the content, and to the notebook's archived proposals. The semantic vector
// set is dropped and rebuilt from the purged notebook, since no vector says
// which content it came from. Without index, for a purge that left the
// notebook as it was, only the records are redacted.
func (e *Engine) Purged(ctx context.Context, purge string, redact func([]byte) ([]byte, bool, error), index bool) (PurgeCleanup, error) {
	if purge == "" {
		return PurgeCleanup{}, errors.New("a purge clean-up needs the purge's ID")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var out PurgeCleanup
	archived, err := e.redactArchived(ctx, redact)
	if err != nil {
		return out, err
	}
	out.Proposals += archived
	for attempt := 0; ; attempt++ {
		if err := e.refresh(ctx); err != nil {
			return out, err
		}
		next, proposals, checkpoints, err := e.redactedState(redact)
		if err != nil {
			return out, err
		}
		vectors := 0
		if index {
			next.Purge, vectors = purge, len(e.stored)
		} else if proposals+checkpoints == 0 {
			break
		}
		err = e.saveState(ctx, next, nil, index)
		if errors.Is(err, workspace.ErrConflict) && attempt < 10 {
			continue
		}
		if err != nil {
			return out, err
		}
		out.Proposals += proposals
		out.Checkpoints, out.Vectors = checkpoints, vectors
		break
	}
	if index {
		e.forgetDerived()
		e.signal()
	}
	return out, nil
}

// redactedState returns the engine's state with redact applied to it, and
// how many proposals and checkpoints that changed.
func (e *Engine) redactedState(redact func([]byte) ([]byte, bool, error)) (state, int, int, error) {
	next := state{Config: e.state.Config, Purge: e.state.Purge, Checkpoints: map[string]Checkpoint{}, Proposals: map[string]Proposal{}}
	proposals, checkpoints := 0, 0
	for id, p := range e.state.Proposals {
		if redact != nil {
			changed, err := redactValue(redact, &p)
			if err != nil {
				return state{}, 0, 0, fmt.Errorf("proposal %s: %w", id, err)
			}
			if changed {
				proposals++
			}
		}
		next.Proposals[id] = p
	}
	for id, cp := range e.state.Checkpoints {
		if redact != nil {
			changed, err := redactValue(redact, &cp)
			if err != nil {
				return state{}, 0, 0, fmt.Errorf("checkpoint %s: %w", id, err)
			}
			if changed {
				checkpoints++
			}
		}
		next.Checkpoints[id] = cp
	}
	return next, proposals, checkpoints, nil
}

// redactArchived applies redact to every archived proposal of the notebook.
func (e *Engine) redactArchived(ctx context.Context, redact func([]byte) ([]byte, bool, error)) (int, error) {
	if redact == nil {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storageTimeout)
	defer cancel()
	ids, err := e.db.DocumentIDs(ctx, e.db.Pool, archiveKind)
	if err != nil {
		return 0, unavailable(err)
	}
	changed := 0
	for _, id := range ids {
		proposal, ok := strings.CutPrefix(id, e.notebook+"/")
		if !ok {
			continue
		}
		redacted, err := e.redactArchivedProposal(ctx, id, proposal, redact)
		if err != nil {
			return changed, err
		}
		if redacted {
			changed++
		}
	}
	return changed, nil
}

// redactArchivedProposal applies redact to one archived proposal while
// holding its document.
func (e *Engine) redactArchivedProposal(ctx context.Context, id, proposal string, redact func([]byte) ([]byte, bool, error)) (bool, error) {
	tx, err := e.db.Pool.Begin(ctx)
	if err != nil {
		return false, unavailable(err)
	}
	defer tx.Rollback(ctx)
	version, raw, err := e.db.LockDocument(ctx, tx, archiveKind, id, -1)
	if err != nil || version == 0 {
		return false, unavailable(err)
	}
	p, err := decodeProposal(raw, proposal)
	if err != nil {
		return false, err
	}
	redacted, err := redactValue(redact, &p)
	if err != nil || !redacted {
		return false, err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return false, err
	}
	if _, err = e.db.WriteDocument(ctx, tx, archiveKind, id, version, body); err != nil {
		return false, storageError(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return false, unavailable(err)
	}
	return true, nil
}

// redactValue applies redact to value's JSON form and decodes the result
// back into it.
func redactValue[T any](redact func([]byte) ([]byte, bool, error), value *T) (bool, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return false, err
	}
	next, changed, err := redact(raw)
	if err != nil || !changed {
		return false, err
	}
	var out T
	if err := json.Unmarshal(next, &out); err != nil {
		return false, fmt.Errorf("the redacted record no longer decodes: %w", err)
	}
	*value = out
	return true, nil
}

// forgetDerived drops everything the engine derived from notebook content,
// in memory and in its cache files, so it is derived again from the notebook
// as it is now. The caller holds mu.
func (e *Engine) forgetDerived() {
	e.resetIndex()
	e.retrieval = nil
	e.writingCache, e.writingCacheBytes = nil, 0
	e.referenceRevision, e.referenceVisibility, e.referenceLinks = "", "", nil
	e.mapCache = nil
	e.history = historyCache{}
	e.historyLoaded, e.historyDirty = false, false
	e.historyFailure, e.historyPersistFailure = "", ""
	RemoveDerivedFiles(e.dir)
}

// RemoveDerivedFiles deletes the cache files a notebook's engine keeps in
// its directory dir, which hold text derived from its notes: the history
// index and the note map. A gateway that hears of a purge removes them even
// when it has no engine of the notebook open.
func RemoveDerivedFiles(dir string) {
	for _, name := range []string{filepath.Join("cache", "history.json"), "note-map.json"} {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// readPinned reads path at revision, mapping a revision a purge rewrote to
// what stands for it now; a version the purge removed is ErrPurged.
func (e *Engine) readPinned(ctx context.Context, path, revision string) (workspace.Document, error) {
	doc, err := e.Store.KnowledgeRead(ctx, path, revision)
	if err == nil || revision == "" {
		return doc, err
	}
	now, purged, resolveErr := e.Store.ResolvePurgedRevision(ctx, path, revision)
	switch {
	case resolveErr != nil || now == revision:
		return doc, err
	case purged:
		return workspace.Document{}, ErrPurged
	}
	return e.Store.KnowledgeRead(ctx, path, now)
}

// PurgeHolds counts the proposals and consumer checkpoints, active or
// archived, whose records redact would change, for a purge plan, which
// reports counts only. It changes nothing.
func (e *Engine) PurgeHolds(ctx context.Context, redact func([]byte) ([]byte, bool, error)) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.refresh(ctx); err != nil {
		return 0, err
	}
	_, proposals, checkpoints, err := e.redactedState(redact)
	if err != nil {
		return 0, err
	}
	count := proposals + checkpoints
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storageTimeout)
	defer cancel()
	ids, err := e.db.DocumentIDs(readCtx, e.db.Pool, archiveKind)
	if err != nil {
		return count, unavailable(err)
	}
	for _, id := range ids {
		proposal, ok := strings.CutPrefix(id, e.notebook+"/")
		if !ok {
			continue
		}
		version, raw, err := e.db.ReadDocument(readCtx, e.db.Pool, archiveKind, id, -1)
		if err != nil {
			return count, unavailable(err)
		}
		if version == 0 {
			continue
		}
		p, err := decodeProposal(raw, proposal)
		if err != nil {
			return count, err
		}
		if changed, err := redactValue(redact, &p); err != nil {
			return count, err
		} else if changed {
			count++
		}
	}
	return count, nil
}
