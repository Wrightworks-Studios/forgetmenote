package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

func organizationTestContext() context.Context {
	return attribution.With(context.Background(), attribution.Identity{ID: "organizer-test", Name: "Synthetic", Email: "synthetic@example.invalid", Source: "jev"})
}
func orgSource(t *testing.T, s *Store, p string) MutationSource {
	t.Helper()
	d, e := s.Read(context.Background(), p, "")
	if e != nil {
		t.Fatal(e)
	}
	return MutationSource{Path: p, Revision: d.Revision, Hash: ContentHash(d.Content)}
}
func orgSave(t *testing.T, s *Store, p, c string) {
	t.Helper()
	rev := ""
	if d, e := s.Read(context.Background(), p, ""); e == nil {
		rev = d.Revision
	}
	if _, e := s.Save(context.Background(), p, []byte(c), rev, "test"); e != nil {
		t.Fatal(e)
	}
}
func TestOrganizationMoveAndGuardedUndo(t *testing.T) {
	s := newTestStore(t)
	ctx := organizationTestContext()
	orgSave(t, s, "notes/a.md", "# Original\n[image](../assets/picture.png)\n")
	orgSave(t, s, "dest/index.md", "# Destination\n")
	orgSave(t, s, "backlink.md", "[note](notes/a.md)\n")
	a := OrganizationAction{ID: "run:1", Kind: "move", Path: "notes/a.md", Target: "dest/a.md", Sources: []MutationSource{orgSource(t, s, "notes/a.md")}, Allowed: []string{"notes/a.md", "backlink.md"}, Folders: []string{"dest"}}
	preview, e := s.Organize(ctx, a, false)
	if e != nil || len(preview.Files) < 3 {
		t.Fatal(preview, e)
	}
	result, e := s.Organize(ctx, a, true)
	if e != nil {
		t.Fatal(e)
	}
	again, e := s.Organize(ctx, a, true)
	if e != nil || !again.Replayed || again.Revision != result.Revision {
		t.Fatal(again, e)
	}
	d, _ := s.Read(ctx, "backlink.md", "")
	if !strings.Contains(string(d.Content), "dest/a.md") {
		t.Fatal(string(d.Content))
	}
	undo := OrganizationUndo{ID: "undo:1", Action: a, Actor: "organizer-test", Revision: result.Revision}
	restored, e := s.UndoOrganization(ctx, undo)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Read(ctx, "notes/a.md", ""); e != nil {
		t.Fatal(e)
	}
	again, e = s.UndoOrganization(ctx, undo)
	if e != nil || !again.Replayed || again.Revision != restored.Revision {
		t.Fatal(again, e)
	}
}
func TestOrganizationDuplicateAndUndoPreserveTrashAndBacklinks(t *testing.T) {
	s := newTestStore(t)
	ctx := organizationTestContext()
	for _, p := range []string{"a.md", "b.md", "c.md"} {
		orgSave(t, s, p, "# Shared fact\nWorkshop begins at 09:00.\n")
	}
	orgSave(t, s, "links.md", "[current](b.md)\n")
	a := OrganizationAction{ID: "run:1", Kind: "duplicate", Path: "b.md", Target: "a.md", Sources: []MutationSource{orgSource(t, s, "b.md"), orgSource(t, s, "a.md")}, Allowed: []string{"b.md", "links.md"}}
	result, e := s.Organize(ctx, a, true)
	if e != nil || result.Trash == nil {
		t.Fatal(result, e)
	}
	if _, e = s.Read(ctx, "a.md", ""); e != nil {
		t.Fatal("lost retained note", e)
	}
	trash, e := s.ListTrash(ctx)
	if e != nil || len(trash.Entries) != 1 {
		t.Fatal(trash, e)
	}
	if _, e = s.UndoOrganization(ctx, OrganizationUndo{ID: "run:undo1", Action: a, Actor: "organizer-test", Revision: result.Revision}); e != nil {
		t.Fatal(e)
	}
	d, _ := s.Read(ctx, "links.md", "")
	if string(d.Content) != "[current](b.md)\n" {
		t.Fatal(string(d.Content))
	}
	trash, e = s.ListTrash(ctx)
	if e != nil || len(trash.Entries) != 0 {
		t.Fatal(trash, e)
	}
}
func TestOrganizationBlocksUnsafeTargetsAndStaleEvidence(t *testing.T) {
	for _, kind := range []string{"retained-change", "collision", "locked-backlink", "scope-backlink", "metadata", "relative-resource", "traversal", "empty-trash", "symlink", "newer-undo"} {
		t.Run(kind, func(t *testing.T) {
			s := newTestStore(t)
			ctx := organizationTestContext()
			orgSave(t, s, "a.md", "# Shared\nFact 1.\n")
			orgSave(t, s, "b.md", "# Shared\nFact 1.\n")
			orgSave(t, s, "links.md", "[note](b.md)\n")
			orgSave(t, s, "dest/index.md", "# Folder\n")
			a := OrganizationAction{ID: "run:1", Kind: "duplicate", Path: "b.md", Target: "a.md", Sources: []MutationSource{orgSource(t, s, "b.md"), orgSource(t, s, "a.md")}, Allowed: []string{"b.md", "links.md"}}
			switch kind {
			case "retained-change":
				orgSave(t, s, "a.md", "# Changed\n")
			case "collision":
				a.Kind = "move"
				a.Target = "dest/b.md"
				a.Folders = []string{"dest"}
				orgSave(t, s, a.Target, "Occupied\n")
			case "locked-backlink":
				setLocks(t, s, "links.md")
			case "scope-backlink":
				a.Allowed = []string{"b.md"}
			case "metadata":
				orgSave(t, s, "b.md", "---\ntags: [unique]\n---\n# Shared\nFact 1.\n")
				a.Sources[0] = orgSource(t, s, "b.md")
			case "relative-resource":
				orgSave(t, s, "nested/b.md", "# Shared\n[image](image.png)\n")
				orgSave(t, s, "a.md", "# Shared\n[image](image.png)\n")
				a.Path = "nested/b.md"
				a.Allowed = []string{a.Path}
				a.Sources = []MutationSource{orgSource(t, s, a.Path), orgSource(t, s, "a.md")}
			case "traversal":
				a.Kind = "move"
				a.Target = "../b.md"
				a.Folders = []string{".."}
			case "empty-trash":
				a.Kind = "empty_trash"
			case "symlink":
				commitFixture(t, s, notestore.FileChange{Path: "outside", Content: []byte("/elsewhere"), Mode: gitobj.ModeSymlink})
				a.Kind = "move"
				a.Target = "outside/b.md"
				a.Folders = []string{"outside"}
			case "newer-undo":
				result, e := s.Organize(ctx, a, true)
				if e != nil {
					t.Fatal(e)
				}
				orgSave(t, s, "links.md", "User changed backlink\n")
				if _, e = s.UndoOrganization(ctx, OrganizationUndo{ID: "run:undo", Action: a, Actor: "organizer-test", Revision: result.Revision}); !errors.Is(e, ErrConflict) {
					t.Fatal("undo overwrote newer edit", e)
				}
				return
			}
			before := headOf(t, s)
			if _, e := s.Organize(ctx, a, true); e == nil {
				t.Fatal("unsafe action succeeded")
			}
			if after := headOf(t, s); after != before {
				t.Fatal("blocked action partially committed")
			}
		})
	}
}

func TestOrganizationRefusesPendingLiveAndPreservesFailedCommit(t *testing.T) {
	for _, live := range []bool{true, false} {
		t.Run(fmt.Sprint(live), func(t *testing.T) {
			s := newTestStore(t)
			ctx := organizationTestContext()
			original, e := s.Save(ctx, "note.md", []byte("😀 alpha\n\nbeta\n"), "", "")
			if e != nil {
				t.Fatal(e)
			}
			a := OrganizationAction{ID: "failure:1", Kind: "tags", Path: original.Path, Tags: []string{"workshop"}, Allowed: []string{original.Path}, Sources: []MutationSource{orgSource(t, s, original.Path)}}
			var release func()
			if live {
				// Accepted live edits that are not checkpointed yet.
				s.mu.Lock()
				s.live[original.Path] = &liveRecord{Path: original.Path, Uncommitted: []string{"pending"}}
				s.mu.Unlock()
			} else {
				release = failCommits(t, s)
			}
			if _, e := s.Organize(ctx, a, true); e == nil || live && !errors.Is(e, ErrLivePending) {
				t.Fatal("unsafe write succeeded", e)
			}
			if release != nil {
				release()
			}
			d, e := s.Read(ctx, original.Path, "")
			if e != nil || d.Revision != original.Revision || string(d.Content) != string(original.Content) {
				t.Fatal("failed action changed content or history", e)
			}
			if live {
				s.mu.Lock()
				pending := s.hasLivePending()
				s.mu.Unlock()
				if !pending {
					t.Fatal("pending draft lost")
				}
			}
		})
	}
}

func TestOrganizationBlocksUnrewritableCommonMarkBacklinks(t *testing.T) {
	s := newTestStore(t)
	ctx := organizationTestContext()
	for _, p := range []string{"a.md", "b.md"} {
		orgSave(t, s, p, "# Same\nFact.\n")
	}
	orgSave(t, s, "links.md", "[duplicate \\]](b.md)\n")
	a := OrganizationAction{ID: "escape:1", Kind: "duplicate", Path: "b.md", Target: "a.md", Allowed: []string{"b.md", "links.md"}, Sources: []MutationSource{orgSource(t, s, "b.md"), orgSource(t, s, "a.md")}}
	if _, e := s.Organize(ctx, a, true); e == nil {
		t.Fatal("cleanup left a valid CommonMark backlink pointing to a missing note")
	}
	if _, e := s.Read(ctx, "b.md", ""); e != nil {
		t.Fatal("blocked dependency partially removed note", e)
	}
}

// A receipt whose original content can no longer be read is an unknown
// outcome to recover, never an absent file: undo refuses it and changes
// nothing, and the current note stays readable.
func TestOrganizationUndoRefusesUnreadableReceipt(t *testing.T) {
	s := newTestStore(t)
	ctx := organizationTestContext()
	// One paragraph over the inline limit is stored as a single block.
	original := "# Original\n" + strings.Repeat("A long paragraph of the original note. ", 300) + "\n"
	orgSave(t, s, "a.md", original)
	a := OrganizationAction{ID: "unreadable:1", Kind: "tags", Path: "a.md", Tags: []string{"workshop"}, Allowed: []string{"a.md"}, Sources: []MutationSource{orgSource(t, s, "a.md")}}
	result, e := s.Organize(ctx, a, true)
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256([]byte(original))
	if e = testEnv(t, s).blobs.Delete(context.Background(), "blocks/"+hex.EncodeToString(sum[:])); e != nil {
		t.Fatal(e)
	}
	s = reopenTestStore(t, s)
	if _, e = s.Organize(ctx, a, true); !errors.Is(e, ErrOrganizationOutcomeUnknown) {
		t.Fatal("unreadable committed receipt was not recoverable", e)
	}
	_, e = s.UndoOrganization(ctx, OrganizationUndo{ID: a.ID, Action: a, Actor: "organizer-test", Revision: result.Revision})
	if e == nil {
		t.Fatal("Undo accepted a partially unreadable receipt")
	}
	if head := headOf(t, s); head != result.Revision {
		t.Fatal("failed receipt read changed notebook", head)
	}
	if _, e = s.Read(ctx, "a.md", ""); e != nil {
		t.Fatal("receipt read failure deleted the note", e)
	}
}
