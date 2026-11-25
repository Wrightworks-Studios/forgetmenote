package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/notestore"
)

// Edit groups and organization actions for notebooks kept in the notes
// store. Each keeps its Git original's checks, results, errors, commit
// subjects and operation trailers; a group or action is one revision, and
// the operation's Gitnotes-Operation and Gitnotes-Request trailers are its
// durable receipt, found again with the notes store's operation index. Notes
// moved into or out of the Trash, or relocated, are committed as renames, so
// they keep their entries and histories.

func (n *dbNotebook) ApplyGroup(ctx context.Context, group EditGroup) (GroupResult, error) {
	ctx = n.mutationContext(ctx)
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(group.ID) < 1 || len(group.ID) > 128 || strings.TrimSpace(group.Summary) == "" || len(group.Summary) > 300 || len(group.Edits) < 1 || len(group.Edits) > 50 {
		return GroupResult{}, invalidInput("group needs an ID, a summary (up to 300 bytes), and 1–50 edits")
	}
	if strings.ContainsRune(group.Summary, 0) {
		return GroupResult{}, invalidInput("a group summary cannot contain NUL")
	}
	encoded, _ := json.Marshal(group)
	if len(encoded) > 4<<20 {
		return GroupResult{}, invalidInput("group exceeds 4 MiB")
	}
	op := operationIdentity{ID: ContentHash([]byte(attribution.From(ctx).ID + "\x00" + group.ID)), Request: ContentHash(encoded)}
	prior, err := n.groupReceipt(ctx, group, attribution.From(ctx).ID)
	if err != nil {
		return GroupResult{}, err
	}
	if prior.Replayed {
		return prior, nil
	}
	if err = n.loadLocks(ctx); err != nil {
		return GroupResult{}, err
	}
	if err = n.clean(ctx); err != nil {
		return GroupResult{}, err
	}
	head, err := n.mutateBase(ctx)
	if err != nil {
		return GroupResult{}, err
	}
	changes := map[string][]byte{}
	renames := map[string]string{}
	index, err := n.loadTrash(ctx)
	if err != nil {
		return GroupResult{}, err
	}
	selected := map[string]bool{}
	for _, edit := range group.Edits {
		if !n.mutateVisibleNote(ctx, edit.Path) || edit.Path == "AGENTS.md" {
			return GroupResult{}, ErrForbidden
		}
		if edit.Delete {
			err = n.resolveLeaving(edit.Path)
		} else {
			_, err = n.resolve(edit.Path, true)
		}
		if err != nil {
			return GroupResult{}, err
		}
		if selected[edit.Path] {
			return GroupResult{}, invalidInput("duplicate path in group")
		}
		selected[edit.Path] = true
	}
	// Hash one bounded immutable preimage at a time; only deletion staging
	// needs its full originals. The same head binds every conflict check.
	preimages := map[string]string{}
	if err = n.forEachCommittedContent(ctx, head, func(path string) bool { return selected[path] }, func(path string, raw []byte) error {
		preimages[path] = ContentHash(raw)
		return nil
	}); err != nil {
		return GroupResult{}, err
	}
	ancestors := map[string]bool{head: true}
	deleted := []string{}
	total := 0
	for _, edit := range group.Edits {
		before, exists := preimages[edit.Path]
		if edit.Revision == "" {
			if exists || edit.Hash != "" || edit.Delete {
				return GroupResult{}, ErrConflict
			}
		} else {
			if !revisionPattern.MatchString(edit.Revision) {
				return GroupResult{}, ErrConflict
			}
			if !ancestors[edit.Revision] {
				if ok, e := n.isAncestor(ctx, edit.Revision, head); e != nil || !ok {
					return GroupResult{}, ErrConflict
				}
				ancestors[edit.Revision] = true
			}
			if !exists || edit.Hash != before {
				return GroupResult{}, ErrConflict
			}
			if edit.Revision != head {
				// A historical base still requires path-local revision
				// identity: changing then reverting text remains a conflict.
				oldVersion, e := n.fileVersion(ctx, edit.Path, edit.Revision)
				if e != nil {
					return GroupResult{}, e
				}
				nowVersion, e := n.fileVersion(ctx, edit.Path, head)
				if e != nil {
					return GroupResult{}, e
				}
				if oldVersion != nowVersion {
					return GroupResult{}, ErrConflict
				}
			}
		}
		if !utf8.ValidString(edit.Content) || strings.ContainsRune(edit.Content, 0) {
			return GroupResult{}, invalidInput("notes must be UTF-8 text")
		}
		total += len(edit.Content)
		if total > 4<<20 {
			return GroupResult{}, invalidInput("group exceeds 4 MiB")
		}
		if edit.Delete {
			deleted = append(deleted, edit.Path)
		} else if err = validateContainerWrite([]byte(edit.Content)); err != nil {
			return GroupResult{}, err
		}
	}
	var prepared trashPreparation
	if len(deleted) > 0 {
		if prepared, err = n.prepareTrash(ctx, deleted, index, head); err != nil {
			return GroupResult{}, err
		}
	}
	for _, edit := range group.Edits {
		if edit.Delete {
			entry, err := n.stageTrash(ctx, &index, edit.Path, attribution.From(ctx).ID, changes, &prepared)
			if err != nil {
				return GroupResult{}, err
			}
			addTrashMove(renames, changes, entry.OriginalPath, entry.Path)
		} else {
			changes[edit.Path] = []byte(edit.Content)
		}
	}
	if len(deleted) > 0 {
		if err = stageTrashIndex(index, len(deleted), changes); err != nil {
			return GroupResult{}, err
		}
	}
	ctx = context.WithValue(ctx, operationKey{}, op)
	result, err := n.applyChanges(ctx, head, group.Summary, orderedChanges(changes, renames))
	if err != nil {
		return GroupResult{}, err
	}
	// Another gateway may have committed this very group meanwhile; the
	// store then answers with that revision.
	return GroupResult{Revision: result.Head.Commit.String(), Replayed: result.Replayed}, nil
}

func (n *dbNotebook) GroupReceipt(ctx context.Context, group EditGroup, identityID string) (GroupResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.groupReceipt(ctx, group, identityID)
}

// mutateVisibleNote is visibleNote with mutateResolve's checks of the head,
// which the Git backend's resolve made in the working tree.
func (n *dbNotebook) mutateVisibleNote(ctx context.Context, path string) bool {
	return n.visibleNote(path) && n.mutateResolve(ctx, path) == nil
}

// ValidateMutationGroup checks a proposal with the closure an applied group
// uses, before its gateway-owned record is persisted. Applying checks again.
func (n *dbNotebook) ValidateMutationGroup(ctx context.Context, group EditGroup) error {
	if mutationScopeFrom(ctx) == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	changes := map[string][]byte{}
	for _, edit := range group.Edits {
		if edit.Delete {
			changes[edit.Path] = nil
		} else {
			changes[edit.Path] = []byte(edit.Content)
		}
	}
	return n.checkMutationChanges(ctx, changes)
}

func (n *dbNotebook) organizationSource(ctx context.Context, source MutationSource) (Document, error) {
	if source.Historical || source.Path == "AGENTS.md" || IsTrash(source.Path) || !n.visibleNote(source.Path) || !revisionPattern.MatchString(source.Revision) {
		return Document{}, ErrForbidden
	}
	current, err := n.read(ctx, source.Path, "")
	if err != nil {
		return current, err
	}
	metadata, err := ParseMetadata(current.Content)
	if err != nil || metadata.Container != "" || metadata.ContainerError != "" {
		return current, ErrForbidden
	}
	before, e1 := n.fileVersion(ctx, source.Path, source.Revision)
	now, e2 := n.fileVersion(ctx, source.Path, current.Revision)
	if e1 != nil || e2 != nil || before != now || ContentHash(current.Content) != source.Hash {
		return current, ErrConflict
	}
	if ancestor, err := n.isAncestor(ctx, source.Revision, current.Revision); err != nil || !ancestor {
		return current, ErrConflict
	}
	return current, nil
}

// OrganizationSources validates exact current committed evidence without
// checkpointing live text. It is also used just before and after a
// provider request.
func (n *dbNotebook) OrganizationSources(ctx context.Context, sources []MutationSource) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(sources) == 0 || len(sources) > 100 {
		return ErrForbidden
	}
	if n.hasLivePending() {
		return ErrLivePending
	}
	if err := n.loadLocks(ctx); err != nil {
		return err
	}
	for _, source := range sources {
		if !n.editable(source.Path) {
			return ErrForbidden
		}
		if _, err := n.organizationSource(ctx, source); err != nil {
			return err
		}
	}
	return nil
}

func (n *dbNotebook) OrganizationReceipt(ctx context.Context, a OrganizationAction, actor string) (OrganizationResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	receipt, err := n.operationReceipt(ctx, organizationOperation(a, actor))
	if err != nil || !receipt.Replayed {
		return OrganizationResult{}, err
	}
	return n.actionResult(ctx, receipt.Revision, true, a)
}

func (n *dbNotebook) Organize(ctx context.Context, a OrganizationAction, apply bool) (OrganizationResult, error) {
	ctx = n.mutationContext(ctx)
	n.mu.Lock()
	defer n.mu.Unlock()
	if a.ID == "" || len(a.ID) > 128 || len(a.Allowed) > 10000 || len(a.Sources) == 0 || len(a.Sources) > 100 || !containsPath(a.Allowed, a.Path) {
		return OrganizationResult{}, ErrForbidden
	}
	op := organizationOperation(a, attribution.From(ctx).ID)
	if apply {
		receipt, err := n.operationReceipt(ctx, op)
		if err != nil {
			if errors.Is(err, ErrConflict) {
				return OrganizationResult{}, err
			}
			return OrganizationResult{}, errors.Join(ErrOrganizationOutcomeUnknown, err)
		}
		if receipt.Replayed {
			result, err := n.actionResult(ctx, receipt.Revision, true, a)
			if err != nil {
				return OrganizationResult{}, errors.Join(ErrOrganizationOutcomeUnknown, err)
			}
			return result, nil
		}
	}
	if n.hasLivePending() {
		return OrganizationResult{}, ErrLivePending
	}
	if err := n.loadLocks(ctx); err != nil {
		return OrganizationResult{}, err
	}
	if err := n.clean(ctx); err != nil {
		return OrganizationResult{}, err
	}
	head, err := n.mutateBase(ctx)
	if err != nil {
		return OrganizationResult{}, err
	}
	docs := map[string]Document{}
	for _, source := range a.Sources {
		doc, err := n.organizationSource(ctx, source)
		if err != nil {
			return OrganizationResult{}, err
		}
		docs[source.Path] = doc
	}
	doc, ok := docs[a.Path]
	if !ok || !n.editable(a.Path) {
		return OrganizationResult{}, ErrForbidden
	}
	changes := map[string][]byte{}
	renames := map[string]string{}
	summary := "Organize note: " + a.Kind
	var entry *TrashEntry
	var trashLinks []OrganizationTrashLink
	var trashRewrites map[string][]byte
	switch a.Kind {
	case "tags":
		if len(a.Tags) == 0 || len(a.Tags) > 100 || a.Target != "" {
			return OrganizationResult{}, ErrForbidden
		}
		content, err := WithAdditionalTags(doc.Content, a.Tags)
		if err != nil {
			return OrganizationResult{}, err
		}
		if !bytes.Equal(content, doc.Content) {
			changes[a.Path] = content
		}
	case "links":
		if _, ok := docs[a.Target]; !ok || a.Target == a.Path || len(a.Tags) > 0 {
			return OrganizationResult{}, ErrForbidden
		}
		// Resolve existing Markdown links using the transaction owner's link rules.
		if !bytes.Equal(doc.Content, rewriteLinks(doc.Content, a.Path, a.Path, a.Target, a.Target+".probe")) {
			break
		}
		relative, err := filepath.Rel(filepath.Dir(a.Path), a.Target)
		if err != nil {
			return OrganizationResult{}, err
		}
		href := (&url.URL{Path: filepath.ToSlash(relative)}).String()
		changes[a.Path] = append(append([]byte{}, doc.Content...), []byte("\n\nRelated: [Related note](<"+href+">)\n")...)
	case "move", "duplicate":
		if len(a.Tags) > 0 || a.Target == a.Path {
			return OrganizationResult{}, ErrForbidden
		}
		if a.Kind == "move" {
			body := string(MarkdownBody(doc.Content))
			if strings.Contains(body, "[[") || strings.Contains(body, "<a ") || strings.Contains(body, "<img ") {
				return OrganizationResult{}, errors.New("unsupported outgoing references require manual relocation")
			}
			folder := path.Dir(a.Target)
			if folder == "." {
				folder = ""
			}
			if path.Base(a.Target) != path.Base(a.Path) || !containsPath(a.Folders, folder) || !n.mutateVisibleNote(ctx, a.Target) || IsTrash(a.Target) || !n.editable(a.Target) {
				return OrganizationResult{}, ErrForbidden
			}
			if folder != "" {
				if err := n.mutateResolve(ctx, folder); err != nil {
					return OrganizationResult{}, err
				}
				if dir, err := n.isDir(ctx, head, folder); err != nil || !dir {
					return OrganizationResult{}, errors.New("approved destination no longer exists")
				}
			}
			if taken, err := n.trashOccupied(ctx, a.Target); err != nil || taken {
				return OrganizationResult{}, ErrConflict
			}
			changes[a.Path] = nil
			changes[a.Target] = rewriteLinks(doc.Content, a.Path, a.Target, a.Path, a.Target)
			if err := verifyOrganizationLinks(doc.Content, changes[a.Target], a.Path, a.Target, a.Path, a.Target); err != nil {
				return OrganizationResult{}, err
			}
			expectedRevision := ""
			for _, source := range a.Sources {
				if source.Path == a.Path {
					expectedRevision = source.Revision
				}
			}
			for _, p := range []string{commentPath(a.Path), commentPath(a.Target)} {
				before, e1 := n.fileVersion(ctx, p, expectedRevision)
				now, e2 := n.fileVersion(ctx, p, head)
				if e1 != nil || e2 != nil || before != now {
					return OrganizationResult{}, ErrConflict
				}
			}
			if err := n.stageTrashDiscussion(ctx, a.Path, a.Target, changes); err != nil {
				return OrganizationResult{}, err
			}
			addTrashMove(renames, changes, a.Path, a.Target)
			summary = fmt.Sprintf("Move %q to %q", a.Path, a.Target)
		} else {
			retained, ok := docs[a.Target]
			if !ok {
				return OrganizationResult{}, ErrForbidden
			}
			if err := n.verifyDuplicateDependencies(ctx, doc, retained); err != nil {
				return OrganizationResult{}, err
			}
			index, err := n.loadTrash(ctx)
			if err != nil {
				return OrganizationResult{}, err
			}
			trashed, err := n.stageTrash(ctx, &index, a.Path, attribution.From(ctx).ID, changes, nil)
			if err != nil {
				return OrganizationResult{}, err
			}
			if err := verifyOrganizationLinks(doc.Content, changes[trashed.Path], a.Path, trashed.Path, a.Path, trashed.Path); err != nil {
				return OrganizationResult{}, err
			}
			entry = &trashed
			if err = stageTrashIndex(index, 1, changes); err != nil {
				return OrganizationResult{}, err
			}
			addTrashMove(renames, changes, a.Path, trashed.Path)
		}
		// Read every committed Markdown path, including excluded and locked
		// backlinks. Required out-of-scope repairs reject the whole action.
		// Notes in the Trash are outside every scope: a move updates their
		// links on a best-effort basis below, and a duplicate leaves them,
		// as moving a note to the Trash does.
		err := n.forEachCommittedContent(ctx, doc.Revision, func(p string) bool {
			return strings.HasSuffix(p, ".md") && p != a.Path && !IsTrash(p)
		}, func(p string, content []byte) error {
			if unsupportedReference(content, a.Path) {
				return fmt.Errorf("unresolved backlink in %s", p)
			}
			after := rewriteLinks(content, p, p, a.Path, a.Target)
			if err := verifyOrganizationLinks(content, after, p, p, a.Path, a.Target); err != nil {
				return fmt.Errorf("backlink %s: %w", p, err)
			}
			if bytes.Equal(after, content) {
				return nil
			}
			if !containsPath(a.Allowed, p) || !n.visibleNote(p) || p == "AGENTS.md" || !n.editable(p) {
				return fmt.Errorf("backlink %s is locked, excluded or outside the selected scope: %w", p, ErrForbidden)
			}
			if a.Expected != nil {
				found := false
				for _, expected := range a.Expected {
					if expected.Path == p {
						found = true
						if _, err := n.organizationSource(ctx, expected); err != nil {
							return err
						}
						break
					}
				}
				if !found {
					return ErrConflict
				}
			}
			meta, err := ParseMetadata(content)
			if err != nil || meta.Container != "" || meta.ContainerError != "" {
				return ErrForbidden
			}
			// Incoming fragment links are safe only when the complete
			// heading-bearing body is identical; a semantic duplicate
			// judgment alone cannot prove anchors.
			if a.Kind == "duplicate" && strings.Contains(string(content), "#") && !bytes.Equal(MarkdownBody(doc.Content), MarkdownBody(docs[a.Target].Content)) {
				return errors.New("fragment references require identical retained headings and body")
			}
			changes[p] = after
			return nil
		})
		if err != nil {
			return OrganizationResult{}, err
		}
		if a.Kind == "move" {
			index, err := n.loadTrash(ctx)
			if err != nil {
				return OrganizationResult{}, err
			}
			trashLinks, trashRewrites, err = n.planTrashLinks(ctx, doc.Revision, trashOrigins(index), a.Path, a.Target, n.projectLocks, n.readOnly.Load(), mutationScopeFrom(ctx) != nil)
			if err != nil {
				return OrganizationResult{}, err
			}
			for p, after := range trashRewrites {
				changes[p] = after
			}
		}
	default:
		return OrganizationResult{}, ErrForbidden
	}
	result := OrganizationResult{Revision: doc.Revision, Files: []OrganizationFile{}, Trash: entry, TrashLinks: trashLinks}
	total := 0
	for p, after := range changes {
		if _, rewritten := trashRewrites[p]; rewritten || trashFile(p) {
			continue
		}
		old, err := n.organizationBlob(ctx, head, p)
		if err != nil {
			return result, err
		}
		var next *string
		if after != nil {
			v := string(after)
			next = &v
		}
		if old != nil {
			total += len(*old)
		}
		total += len(after)
		if total > 2<<20 {
			return result, errors.New("organization action exceeds 2 MiB; review manually")
		}
		result.Files = append(result.Files, OrganizationFile{Path: p, Before: old, After: next})
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Path < result.Files[j].Path })
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > 1<<20 {
		return result, errors.New("organization receipt exceeds 1 MiB; review manually")
	}
	if !apply || len(changes) == 0 {
		return result, nil
	}
	return n.commitOrganization(ctx, op, head, summary, changes, renames, func(ctx context.Context, revision string, replayed bool) (OrganizationResult, error) {
		if a.Kind != "move" {
			return n.actionResult(ctx, revision, replayed, a)
		}
		return n.plannedResult(ctx, revision, replayed, head, trashRewrites, trashLinks, func(result *OrganizationResult, info notestore.CommitInfo) error {
			return n.moveTrashLinks(ctx, result, info, a)
		})
	})
}

// commitOrganization commits an organization action or its undo on base
// under its operation, answering with receipt's result of the revision.
// The store may have committed even when the request failed, for instance
// when it was cancelled; the durable operation receipt, read without the
// caller's cancellation, owns the outcome.
func (n *dbNotebook) commitOrganization(ctx context.Context, op operationIdentity, base, summary string, changes map[string][]byte, renames map[string]string, receipt func(context.Context, string, bool) (OrganizationResult, error)) (OrganizationResult, error) {
	committed, applyErr := n.applyChanges(context.WithValue(ctx, operationKey{}, op), base, summary, orderedChanges(changes, renames))
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	found, err := n.operationReceipt(recovery, op)
	if err != nil {
		return OrganizationResult{}, errors.Join(ErrOrganizationOutcomeUnknown, applyErr, err)
	}
	if !found.Replayed {
		if applyErr != nil {
			return OrganizationResult{}, applyErr
		}
		return OrganizationResult{}, ErrOrganizationOutcomeUnknown
	}
	// Another gateway may have committed this very action meanwhile; the
	// store then answers with that revision.
	result, err := receipt(recovery, found.Revision, applyErr == nil && committed.Replayed)
	if err != nil {
		return result, errors.Join(ErrOrganizationOutcomeUnknown, err)
	}
	return result, nil
}

func (n *dbNotebook) verifyDuplicateDependencies(ctx context.Context, duplicate, retained Document) error {
	if duplicate.Path <= retained.Path {
		return errors.New("retain the deterministically first note; duplicate ordering changed")
	}
	if ParseSource(duplicate.Content) != nil || ParseSource(retained.Content) != nil {
		return errors.New("source provenance needs manual review")
	}
	left, right := MarkdownBody(duplicate.Content), MarkdownBody(retained.Content)
	if !bytes.Equal(duplicate.Content[:len(duplicate.Content)-len(left)], retained.Content[:len(retained.Content)-len(right)]) {
		return errors.New("meaningful metadata differs")
	}
	for _, doc := range []Document{duplicate, retained} {
		if strings.Contains(string(MarkdownBody(doc.Content)), "[[") || strings.Contains(string(MarkdownBody(doc.Content)), "<") {
			return errors.New("unresolved HTML, wiki links or explicit markup needs manual review")
		}
	}
	// Comparing normalized destinations detects identical bytes with
	// different relative attachments. For semantic bodies with links, refuse
	// to infer that unique linked resources are expendable.
	normalized := rewriteLinks(duplicate.Content, duplicate.Path, retained.Path, "__no_target__", "__no_target__")
	if err := verifyOrganizationLinks(duplicate.Content, normalized, duplicate.Path, retained.Path, "__no_target__", "__no_target__"); err != nil {
		return err
	}
	if !bytes.Equal(normalized, duplicate.Content) {
		return errors.New("relative links resolve differently")
	}
	if !bytes.Equal(left, right) && (strings.Contains(string(left), "](") || strings.Contains(string(left), "]:") || strings.Contains(string(right), "](") || strings.Contains(string(right), "]:")) {
		return errors.New("linked resources require identical complete bodies")
	}
	head, err := n.head(ctx)
	if err != nil {
		return err
	}
	raw, err := n.organizationBlob(ctx, head, commentPath(duplicate.Path))
	if err != nil {
		return err
	}
	if raw != nil {
		var discussion []Comment
		if json.Unmarshal([]byte(*raw), &discussion) != nil || len(discussion) > 0 {
			return errors.New("duplicate has discussion or retained discussion history")
		}
	}
	return nil
}

// actionResult is the receipt of the organization action a at revision:
// what organizationRevision lists, with the notes in the Trash a move's
// links reach in TrashLinks.
func (n *dbNotebook) actionResult(ctx context.Context, revision string, replayed bool, a OrganizationAction) (OrganizationResult, error) {
	result, info, err := n.organizationRevision(ctx, revision, replayed)
	if err != nil {
		return result, err
	}
	return result, n.moveTrashLinks(ctx, &result, info, a)
}

// undoResult is the receipt of the undo at revision of the organization
// action input names: what organizationRevision lists, with the notes in
// the Trash a move rewrote in TrashLinks.
func (n *dbNotebook) undoResult(ctx context.Context, revision string, replayed bool, input OrganizationUndo) (OrganizationResult, error) {
	result, info, err := n.organizationRevision(ctx, revision, replayed)
	if err != nil {
		return result, err
	}
	return result, n.undoTrashLinks(ctx, &result, info, input)
}

// organizationRevision is the organization revision at revision, with its
// changes: each path it changed, as "git diff-tree -r" names them, a
// rename's source and destination both, before and after it, and the Trash
// entry it made.
func (n *dbNotebook) organizationRevision(ctx context.Context, revision string, replayed bool) (OrganizationResult, notestore.CommitInfo, error) {
	result := OrganizationResult{Revision: revision, Replayed: replayed, Files: []OrganizationFile{}}
	rev, err := parseRevision(revision)
	if err != nil || rev.IsZero() {
		return result, notestore.CommitInfo{}, errors.Join(errors.New("an organization receipt needs its revision"), err)
	}
	info, err := n.notes.Commit(ctx, rev)
	if err != nil {
		return result, info, err
	}
	// Like diff-tree, which compares a revision with its one parent and
	// prints nothing for the first revision or a merge.
	if len(info.Commit.Parents) != 1 {
		return result, info, nil
	}
	parent := info.Commit.Parents[0].String()
	changed := map[string]bool{}
	for _, c := range info.Changes {
		changed[c.Path] = true
		if c.OldPath != "" {
			changed[c.OldPath] = true
		}
	}
	trashChanged := false
	for _, p := range slices.Sorted(maps.Keys(changed)) {
		if trashFile(p) {
			// An archive a compaction wrote holds tombstones, not this
			// action's Trash entry, and its undo leaves it.
			trashChanged = trashChanged || p == trashIndexPath
			continue
		}
		item := OrganizationFile{Path: p}
		if item.Before, err = n.organizationBlob(ctx, parent, p); err != nil {
			return result, info, err
		}
		if item.After, err = n.organizationBlob(ctx, revision, p); err != nil {
			return result, info, err
		}
		result.Files = append(result.Files, item)
	}
	if trashChanged {
		raw, _, err := n.notes.Read(ctx, rev, trashIndexPath)
		if err != nil {
			return result, info, err
		}
		var index TrashIndex
		if json.Unmarshal(raw, &index) != nil {
			return result, info, ErrConflict
		}
		for _, r := range index.Records {
			for _, f := range result.Files {
				if f.Path == r.Path && f.Before == nil && f.After != nil {
					v := r.TrashEntry
					result.Trash = &v
				}
			}
		}
	}
	return result, info, nil
}

// organizationBlob is the file at p in revision, or nil when there is none.
// Undo treats nil as the file's absence and may delete the current file, so
// only a revision's tree read successfully establishes absence, never a
// failed read, even of content whose blocks are missing; a folder or a
// submodule there is an error.
func (n *dbNotebook) organizationBlob(ctx context.Context, revision, p string) (*string, error) {
	rev, err := parseRevision(revision)
	if err != nil || rev.IsZero() {
		return nil, errors.Join(errors.New("an organization read needs a revision"), err)
	}
	if _, err = n.notes.Stat(ctx, rev, p); err == nil {
		raw, _, err := n.notes.Read(ctx, rev, p)
		if err != nil {
			return nil, err
		}
		value := string(raw)
		return &value, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if known := n.knownRevision(ctx, rev); known != nil {
		return nil, errors.Join(err, known)
	}
	dir, dirErr := n.isDir(ctx, revision, p)
	if dirErr != nil || dir {
		return nil, errors.Join(err, dirErr)
	}
	return nil, nil
}

func (n *dbNotebook) UndoOrganization(ctx context.Context, input OrganizationUndo) (OrganizationResult, error) {
	ctx = n.mutationContext(ctx)
	n.mu.Lock()
	defer n.mu.Unlock()
	if input.ID == "" || len(input.ID) > 128 {
		return OrganizationResult{}, ErrForbidden
	}
	op := undoOperation(input, attribution.From(ctx).ID)
	receipt, err := n.operationReceipt(ctx, op)
	if err != nil {
		return OrganizationResult{}, err
	}
	if receipt.Replayed {
		return n.undoResult(ctx, receipt.Revision, true, input)
	}
	original, err := n.operationReceipt(ctx, organizationOperation(input.Action, input.Actor))
	if err != nil || !original.Replayed || original.Revision != input.Revision {
		return OrganizationResult{}, ErrConflict
	}
	if n.hasLivePending() {
		return OrganizationResult{}, ErrLivePending
	}
	if err = n.loadLocks(ctx); err != nil {
		return OrganizationResult{}, err
	}
	if err = n.clean(ctx); err != nil {
		return OrganizationResult{}, err
	}
	head, err := n.mutateBase(ctx)
	if err != nil {
		return OrganizationResult{}, err
	}
	applied, info, err := n.organizationRevision(ctx, input.Revision, false)
	if err != nil {
		return OrganizationResult{}, err
	}
	permitted := map[string]bool{}
	for _, later := range input.Later {
		if len(later.Later) > 0 || !strings.HasPrefix(later.Action.ID, strings.Split(input.Action.ID, ":")[0]+":") {
			return OrganizationResult{}, ErrForbidden
		}
		r, e := n.operationReceipt(ctx, undoOperation(later, attribution.From(ctx).ID))
		if e != nil || !r.Replayed {
			return OrganizationResult{}, ErrConflict
		}
		permitted[r.Revision] = true
		forward, e := n.operationReceipt(ctx, organizationOperation(later.Action, later.Actor))
		if e != nil || !forward.Replayed || forward.Revision != later.Revision {
			return OrganizationResult{}, ErrConflict
		}
		permitted[forward.Revision] = true
	}
	changes := map[string][]byte{}
	var records map[string]TrashRecord
	var listing map[string]notestore.File
	var trashLinks []OrganizationTrashLink
	reverted := map[string][]byte{}
	for _, file := range applied.Files {
		p := file.Path
		if input.Action.Kind == "move" && trashLinkRewrite(file) {
			// Reverting the links of a note in the Trash is best effort:
			// one changed, gone or locked since stays as it is, and the
			// undo's result says so.
			if _, e := n.resolve(p, false); e != nil {
				return OrganizationResult{}, e
			}
			if records == nil {
				index, e := n.loadTrash(ctx)
				if e != nil {
					return OrganizationResult{}, e
				}
				files, e := n.files(ctx, head)
				if e != nil {
					return OrganizationResult{}, e
				}
				records, listing = trashRecords(index), trashListing(files)
			}
			link := OrganizationTrashLink{Path: p, OriginalPath: records[p].OriginalPath}
			link.Reason = undoTrashLinkSkip(listing, file, records, n.projectLocks, n.readOnly.Load(), mutationScopeFrom(ctx) != nil)
			if link.Reason == "" {
				// A note whose path no revision changed since the action
				// still has the action's version, which the listing shows;
				// only one changed since needs its later history searched,
				// a query that may scan the notebook's whole history.
				ok := listing[p].Version == info.ID
				if !ok {
					var e error
					if ok, e = n.undoLaterPermitted(ctx, info.Seq, p, permitted); e != nil {
						return OrganizationResult{}, e
					}
				}
				if ok {
					link.Updated = true
					reverted[p] = []byte(*file.Before)
					changes[p] = reverted[p]
				} else {
					link.Reason = trashLinkChanged
				}
			}
			trashLinks = append(trashLinks, link)
			continue
		}
		if p == "AGENTS.md" || strings.HasPrefix(p, "assets/") || p == locksPath {
			return OrganizationResult{}, ErrForbidden
		}
		if !strings.HasPrefix(p, "comments/") && !IsTrash(p) && (!n.visibleNote(p) || !n.editable(p)) {
			return OrganizationResult{}, ErrForbidden
		}
		if _, e := n.resolve(p, false); e != nil {
			return OrganizationResult{}, e
		}
		current, e := n.organizationBlob(ctx, head, p)
		if file.After == nil {
			if e != nil || current != nil {
				return OrganizationResult{}, ErrConflict
			}
		} else if e != nil || current == nil || *current != *file.After {
			return OrganizationResult{}, ErrConflict
		}
		// Even a user edit followed by a revert is newer work. Only verified
		// reverse actions from this run may sit between the original and
		// this restoration.
		if ok, e := n.undoLaterPermitted(ctx, info.Seq, p, permitted); e != nil {
			return OrganizationResult{}, e
		} else if !ok {
			return OrganizationResult{}, ErrConflict
		}
		if file.Before == nil {
			changes[p] = nil
		} else {
			changes[p] = []byte(*file.Before)
		}
	}
	if applied.Trash != nil {
		index, e := n.loadTrash(ctx)
		if e != nil {
			return OrganizationResult{}, e
		}
		found := false
		for i, r := range index.Records {
			if r.ID == applied.Trash.ID && !r.Emptied && !r.Restored {
				index.Records[i].Restored = true
				found = true
			}
		}
		if !found {
			return OrganizationResult{}, errors.New("Trash entry was already restored or emptied")
		}
		if err = stageTrashIndex(index, 0, changes); err != nil {
			return OrganizationResult{}, err
		}
	}
	if len(changes) == 0 {
		return OrganizationResult{}, os.ErrNotExist
	}
	// What the action moved, its undo moves back, so the note and its
	// discussion return with their entries.
	renames := map[string]string{}
	for _, c := range info.Changes {
		if c.Op != notestore.OpRename {
			continue
		}
		back, restored := changes[c.OldPath]
		away, removed := changes[c.Path]
		if restored && back != nil && removed && away == nil {
			renames[c.OldPath] = c.Path
		}
	}
	summary := "Undo organization: " + input.Action.Kind
	if input.Action.Kind == "move" {
		summary = "Move " + strconvQuote(input.Action.Target) + " to " + strconvQuote(input.Action.Path)
	}
	return n.commitOrganization(ctx, op, head, summary, changes, renames, func(ctx context.Context, revision string, replayed bool) (OrganizationResult, error) {
		if input.Action.Kind != "move" {
			return n.undoResult(ctx, revision, replayed, input)
		}
		return n.plannedResult(ctx, revision, replayed, head, reverted, trashLinks, func(result *OrganizationResult, info notestore.CommitInfo) error {
			return n.undoTrashLinks(ctx, result, info, input)
		})
	})
}

// undoLaterPage is how many revisions the first query of a path's later
// history reads; each further query reads twice as many, up to
// undoLaterPageMax. An undo usually follows its action closely, so few
// revisions are later.
const (
	undoLaterPage    = 8
	undoLaterPageMax = 1000
)

// undoLaterPermitted reports whether permitted has every revision of
// "git log --format=%H <revision>..HEAD -- p": the revisions after seq that
// changed a file at or under p. Every revision after an organization FMN
// made descends from it. It reads newest first and stops at the first
// revision not permitted.
func (n *dbNotebook) undoLaterPermitted(ctx context.Context, seq int64, p string, permitted map[string]bool) (bool, error) {
	before, limit := int64(0), undoLaterPage
	for {
		page, err := n.notes.History(ctx, notestore.HistoryQuery{Prefix: p, Before: before, Limit: limit})
		if err != nil {
			return false, err
		}
		for _, c := range page {
			if c.Seq <= seq {
				return true, nil
			}
			if !permitted[c.ID.String()] {
				return false, nil
			}
			before = c.Seq
		}
		if len(page) < limit {
			return true, nil
		}
		limit = min(2*limit, undoLaterPageMax)
	}
}
