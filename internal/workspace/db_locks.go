package workspace

import (
	"context"
	"encoding/json"
	"sort"

	"forgetmenote/internal/gitobj"
)

// Lock rules for notebooks kept in the notes store: the committed
// .gitnotes-locks.json at the head, in the Git backend's format, which
// loadLocks reads and SetLock commits as "Update .gitnotes-locks.json".

func (n *dbNotebook) Locks(ctx context.Context, path string) (LockState, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if path != "" {
		if _, err := n.resolve(path, false); err != nil {
			return LockState{}, err
		}
	}
	if err := n.loadLocks(ctx); err != nil {
		return LockState{}, err
	}
	return n.lockRules(path, n.locksRevision), nil
}

func (n *dbNotebook) SetLock(ctx context.Context, path string, locked bool, expected, actor string) (LockState, error) {
	ctx = n.mutationContext(ctx)
	if IsTrash(path) {
		return LockState{}, ErrForbidden
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.checkCaptureLock(ctx, path); err != nil {
		return LockState{}, err
	}
	if _, err := n.resolve(path, false); err != nil {
		return LockState{}, err
	}
	if err := n.loadLocks(ctx); err != nil {
		return LockState{}, err
	}
	if err := n.clean(ctx); err != nil {
		return LockState{}, err
	}
	state, err := n.lockState(ctx, path)
	if err != nil {
		return state, err
	}
	if expected != state.Revision {
		return state, ErrConflict
	}
	if err := n.checkMutationLockTarget(ctx, path); err != nil {
		return state, err
	}
	paths := []string{}
	exists := false
	for _, p := range n.projectLocks {
		if p == path {
			exists = true
		} else {
			paths = append(paths, p)
		}
	}
	if exists == locked {
		return state, nil
	}
	if locked {
		paths = append(paths, path)
	}
	if len(paths) > 256 {
		return state, capacityReached("project lock limit reached")
	}
	sort.Strings(paths)
	raw, err := json.MarshalIndent(lockPolicy{1, paths}, "", "  ")
	if err != nil {
		return state, err
	}
	if len(raw)+1 > 65536 {
		return state, capacityReached("lock policy exceeds 64 KiB")
	}
	content := append(raw, '\n')
	document, err := n.save(ctx, locksPath, content, expected)
	if err != nil {
		return state, err
	}
	// loadLocks trusts its cache while the policy's blob is unchanged, so
	// the cache names the blob of these rules; a later head that brings
	// back the policy parsed before is then parsed again.
	n.projectLocks, n.locksRevision, n.locksBlob = paths, document.Revision, gitobj.BlobID(content)
	return n.lockRules(path, document.Revision), nil
}
