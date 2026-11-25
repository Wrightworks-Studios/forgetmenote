package workspace

import (
	"strings"
	"testing"
)

func TestOrganizationDuplicateRejectsUnsupportedTrashLinkRewrite(t *testing.T) {
	s := newTestStore(t)
	ctx := organizationTestContext()
	body := "# Same\n[image \\]](assets/picture.png)\n"
	orgSave(t, s, "a.md", body)
	orgSave(t, s, "b.md", body)
	before := headOf(t, s)
	action := OrganizationAction{ID: "escaped:1", Kind: "duplicate", Path: "b.md", Target: "a.md", Allowed: []string{"b.md"}, Sources: []MutationSource{orgSource(t, s, "b.md"), orgSource(t, s, "a.md")}}
	if _, err := s.Organize(ctx, action, true); err == nil || !strings.Contains(err.Error(), "Markdown dependency") {
		t.Fatalf("unsupported Trash relocation must fail before commit: %v", err)
	}
	if after := headOf(t, s); after != before {
		t.Fatal("rejected cleanup changed history", after)
	}
	for _, path := range []string{"a.md", "b.md"} {
		doc, err := s.Read(ctx, path, "")
		if err != nil || string(doc.Content) != body {
			t.Fatal("rejected cleanup changed original", path, err)
		}
	}
	trash, err := s.ListTrash(ctx)
	if err != nil || len(trash.Entries) != 0 {
		t.Fatal("rejected cleanup published Trash", trash, err)
	}
	if status, err := s.Status(ctx); err != nil || status.Dirty {
		t.Fatal("rejected cleanup left partial changes", status, err)
	}
}

func TestOrganizationUndoReplayDoesNotRequireLaterProofs(t *testing.T) {
	s := newTestStore(t)
	ctx := organizationTestContext()
	orgSave(t, s, "a.md", "# Original\n")
	action := OrganizationAction{ID: "replay:1", Kind: "tags", Path: "a.md", Tags: []string{"workshop"}, Allowed: []string{"a.md"}, Sources: []MutationSource{orgSource(t, s, "a.md")}}
	applied, err := s.Organize(ctx, action, true)
	if err != nil {
		t.Fatal(err)
	}
	undo := OrganizationUndo{ID: action.ID, Action: action, Actor: "organizer-test", Revision: applied.Revision}
	restored, err := s.UndoOrganization(ctx, undo)
	if err != nil {
		t.Fatal(err)
	}
	undo.Later = make([]OrganizationUndo, 1001)
	replay, err := s.UndoOrganization(ctx, undo)
	if err != nil || !replay.Replayed || replay.Revision != restored.Revision {
		t.Fatal("durable Undo receipt must recover without revalidating unused later evidence", replay, err)
	}
}
