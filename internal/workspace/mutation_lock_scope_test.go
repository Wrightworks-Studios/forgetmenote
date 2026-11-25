package workspace

import (
	"errors"
	"testing"
)

// A scoped lock rule rechecks, at the owner, every note it would cover,
// including notes another editor reclassified after the scope was chosen.
func TestMutationScopeLockTargetClosureAtOwner(t *testing.T) {
	for _, tc := range []struct {
		name, target, container, collection, control string
		changeAfterRead, allowed                     bool
	}{
		{name: "active-note", target: "notes/target.md", control: "set_note_lock", allowed: true},
		{name: "active-folder", target: "notes", control: "set_note_lock", allowed: true},
		{name: "archive-note", target: "notes/target.md", container: "archive", control: "set_note_lock"},
		{name: "archive-descendant", target: "notes", container: "archive", control: "set_note_lock"},
		{name: "draft-descendant", target: "notes", container: "draft", control: "set_note_lock"},
		{name: "collection-descendant", target: "notes", collection: "archives", control: "set_note_lock"},
		{name: "reclassified-note", target: "notes/target.md", container: "archive", control: "set_note_lock", changeAfterRead: true},
		{name: "reclassified-descendant", target: "notes", container: "archive", control: "set_note_lock", changeAfterRead: true},
		{name: "missing-control", target: "notes/target.md"},
		{name: "other-control", target: "notes/target.md", control: "set_note_container"},
		{name: "missing-note", target: "missing.md", control: "set_note_lock"},
		{name: "missing-folder", target: "missing", control: "set_note_lock"},
		{name: "guidance", target: "AGENTS.md", control: "set_note_lock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := newTestStoreExcluding(t, []string{"private"})
			ctx := t.Context()
			body := "# Target\n"
			if tc.container != "" && !tc.changeAfterRead {
				body = "---\nfmn-container: " + tc.container + "\n---\n" + body
			}
			note, err := owner.Save(ctx, "notes/target.md", []byte(body), "", "author")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = owner.Save(ctx, "notes/active.md", []byte("# Active sibling\n"), "", "author"); err != nil {
				t.Fatal(err)
			}
			commitFixture(t, owner, fixture("AGENTS.md", "# Guidance\n"))
			scope := MutationScope{Version: 1, Control: tc.control, Content: ContentScope{Policy: ContainerPolicy{Version: 1, Archives: "exclude", Drafts: "exclude"}, Collection: "all"}}
			if tc.collection != "" {
				scope.Content.Collection = tc.collection
			}
			scoped := WithMutationScope(ctx, scope)
			if tc.changeAfterRead {
				commitFixture(t, owner, fixture(note.Path, "---\nfmn-container: "+tc.container+"\n---\n# Target\n"))
			}
			for _, locked := range []bool{true, false} {
				before, err := owner.Locks(ctx, tc.target)
				if err != nil {
					t.Fatal(err)
				}
				got, err := owner.SetLock(scoped, tc.target, locked, before.Revision, "agent")
				if tc.allowed {
					if err != nil {
						t.Fatalf("locked=%v: %v", locked, err)
					}
					if got.Revision == before.Revision || (len(got.Rules) == 1) != locked {
						t.Fatalf("lock did not persist: %+v", got)
					}
				} else {
					if !errors.Is(err, ErrForbidden) {
						t.Errorf("locked=%v accepted excluded target or wrong control: %v", locked, err)
					}
					after, e := owner.Locks(ctx, tc.target)
					if e != nil {
						t.Fatal(e)
					}
					if after.Revision != before.Revision || len(after.Rules) != len(before.Rules) {
						t.Errorf("rejected lock changed policy/history: before=%+v after=%+v", before, after)
					}
					// Ordinary owner operations retain their existing authority, including
					// excluded notes and rules for paths without current notes.
					if _, e = owner.SetLock(ctx, tc.target, locked, after.Revision, "owner"); e != nil {
						t.Fatal(e)
					}
				}
			}
		})
	}
}
