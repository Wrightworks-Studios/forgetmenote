package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
)

// The mutation policy checks of mutation_scope.go, read from the notes
// store. Callers hold mu.

// clean is the precondition of every ordinary write: accepted live edits are
// checkpointed first. With no working tree there are no external changes.
func (n *dbNotebook) clean(ctx context.Context) error {
	if n.liveStorageFailed {
		return errors.New("live storage failed; restart the notebook owner to recover before writing")
	}
	if n.checkpointing {
		return nil
	}
	if err := n.liveRecover(ctx); err != nil {
		return errors.Join(ErrLivePending, err)
	}
	if n.hasLivePending() {
		for path, v := range n.live {
			if len(v.Uncommitted) > 0 {
				if _, err := n.checkpointLive(ctx, path); err != nil {
					return errors.Join(ErrLivePending, err)
				}
			}
		}
	}
	return nil
}

// checkMutationTarget checks the committed note a scoped mutation targets.
func (n *dbNotebook) checkMutationTarget(ctx context.Context, path string, allowNew bool) error {
	m := mutationScopeFrom(ctx)
	if m == nil {
		return nil
	}
	if err := m.Validate(); err != nil {
		return err
	}
	if strings.HasPrefix(path, "assets/") {
		return n.checkMutationAsset(ctx, path)
	}
	if path == "AGENTS.md" || IsTrash(path) || !n.visibleNote(path) {
		return ErrForbidden
	}
	doc, err := n.read(ctx, path, "")
	if errors.Is(err, os.ErrNotExist) && allowNew {
		return nil
	}
	if err != nil {
		return err
	}
	return m.checkContent(doc.Content)
}

// checkMutationLockTarget checks every note a scoped lock rule would cover,
// excluded ones included, so an excluded descendant rejects the whole rule.
func (n *dbNotebook) checkMutationLockTarget(ctx context.Context, path string) error {
	if err := n.checkAuthority(ctx); err != nil {
		return err
	}
	m := mutationScopeFrom(ctx)
	if m == nil {
		return nil
	}
	if err := m.Validate(); err != nil {
		return err
	}
	if m.Control != "set_note_lock" {
		return ErrForbidden
	}
	files, err := n.files(ctx, "")
	if err != nil {
		return err
	}
	found := false
	for _, f := range files {
		if !matchesPrefix(f.Path, []string{path}) || !strings.HasSuffix(f.Path, ".md") {
			continue
		}
		if err := n.checkMutationTarget(ctx, f.Path, false); err != nil {
			return err
		}
		found = true
	}
	if !found {
		return ErrForbidden
	}
	return nil
}

// checkMutationChanges checks authority, filing reservations and a scoped
// mutation's sources, target and every changed path.
func (n *dbNotebook) checkMutationChanges(ctx context.Context, changes map[string][]byte) error {
	if err := n.checkAuthority(ctx); err != nil {
		return err
	}
	if err := n.checkCaptureChanges(ctx, changes); err != nil {
		return err
	}
	m := mutationScopeFrom(ctx)
	if m == nil {
		return nil
	}
	if err := m.Validate(); err != nil {
		return err
	}
	for _, source := range m.Sources {
		if !source.Historical {
			if err := n.checkMutationTarget(ctx, source.Path, false); err != nil {
				return err
			}
		}
		if source.Path == "AGENTS.md" || IsTrash(source.Path) || !n.visibleNote(source.Path) {
			return ErrForbidden
		}
		doc, err := n.read(ctx, source.Path, source.Revision)
		if err != nil {
			return err
		}
		if ContentHash(doc.Content) != source.Hash {
			return ErrConflict
		}
		if err = m.checkContent(doc.Content); err != nil {
			return err
		}
	}
	if m.Target != "" {
		if err := n.checkMutationTarget(ctx, m.Target, false); err != nil {
			return err
		}
	}
	for path, after := range changes {
		if path == "AGENTS.md" {
			if m.Control != "set_project_instructions" {
				return ErrForbidden
			}
			continue
		}
		if IsTrash(path) {
			switch m.Control {
			case "trash_notes", "restore_trash", "empty_trash":
				continue
			}
			// The Trash is outside every scope; a move's link rewrites
			// there are the one exception the system maintains.
			if err := n.checkTrashLinkRewrite(ctx, path, after); err != nil {
				return err
			}
			continue
		}
		if !strings.HasSuffix(path, ".md") {
			if strings.HasPrefix(path, "assets/") {
				if err := n.checkMutationAsset(ctx, path); err != nil {
					return err
				}
			}
			continue
		}
		if err := n.checkMutationTarget(ctx, path, true); err != nil {
			return err
		}
		if after == nil || m.Control == "set_note_container" {
			continue
		}
		if err := m.checkContent(after); err != nil {
			return err
		}
		before, err := n.read(ctx, path, "")
		if err == nil {
			old, e1 := ParseContainer(before.Content)
			next, e2 := ParseContainer(after)
			if e1 != nil || e2 != nil || old != next {
				return ErrForbidden
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// checkMutationAsset finds every current owner of a shared media path with
// the link rules a transfer uses, and refuses one the scope excludes. A
// note in the Trash is no owner of media the move in ctx moves: the move
// rewrites its embeds as it does its links, or skips them, the one change
// a scoped move makes outside its scope (checkTrashLinkRewrites).
func (n *dbNotebook) checkMutationAsset(ctx context.Context, path string) error {
	scope := mutationScopeFrom(ctx)
	if scope == nil {
		return nil
	}
	move, moving := ctx.Value(trashLinkMoveKey{}).(trashLinkMove)
	moving = moving && move.moves(path)
	snapshot, err := n.snapshot(ctx, "")
	if err != nil {
		return err
	}
	for _, file := range snapshot.Files {
		original := []byte(file.Content)
		if !bytes.Equal(original, rewriteLinks(original, file.Path, file.Path, path, path+".scope-check")) {
			if moving && IsTrash(file.Path) {
				continue
			}
			if file.Path == "AGENTS.md" || IsTrash(file.Path) || scope.Content.Tier(file.Metadata) < 0 {
				return ErrForbidden
			}
		}
	}
	return nil
}
