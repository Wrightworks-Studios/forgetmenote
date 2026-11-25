package workspace

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"forgetmenote/internal/notestore"
)

func TestScopedReadsFilterAndBindBeforeLimits(t *testing.T) {
	s := newTestStoreExcluding(t, []string{"private"})
	ctx := context.Background()
	var archived []notestore.FileChange
	for i := 0; i < 101; i++ {
		archived = append(archived, fixture(fmt.Sprintf("a%03d.md", i), "---\nfmn-container: archive\n---\n# Beacon\n"))
	}
	commitFixture(t, s, archived...)
	for _, note := range []struct{ path, text string }{{"z.md", "# Current beacon\n"}, {"drafts/work.md", "---\nfmn-container: draft\n---\n# Shared beacon\n"}, {"private/hidden.md", "# Hidden beacon\n"}} {
		if _, err := s.Save(ctx, note.path, []byte(note.text), "", "test"); err != nil {
			t.Fatal(err)
		}
	}
	scope := ContentScope{Policy: ContainerPolicy{Version: 1, Archives: "exclude", Drafts: "include"}, Collection: "all"}
	snapshot, err := s.SnapshotScoped(ctx, "", scope)
	if err != nil || len(snapshot.Files) != 2 || snapshot.ScopeID != scope.ID() || snapshot.Visibility == "" {
		t.Fatalf("scoped snapshot: %+v %v", snapshot, err)
	}
	entries, err := s.FindScoped(ctx, "beacon", "", "", "", false, 100, scope)
	if err != nil || len(entries.Entries) != 2 || entries.ScopeID != scope.ID() {
		t.Fatalf("filtered before literal candidates: %+v %v", entries, err)
	}
	browse, err := s.BrowseScoped(ctx, "", scope)
	if err != nil || len(browse.Items) != 2 || browse.Items[0].Path != "drafts" || browse.Items[0].Count != 1 || browse.ScopeID != scope.ID() {
		t.Fatalf("browse counts: %+v %v", browse, err)
	}
	for _, bad := range []ContentScope{{}, {Policy: ContainerPolicy{Version: 2, Archives: "exclude", Drafts: "exclude"}}, {Policy: ContainerPolicy{Version: 1, Archives: "unknown", Drafts: "exclude"}}, {Policy: scope.Policy, Collection: "anything"}} {
		if _, err := s.SnapshotScoped(ctx, "", bad); err == nil {
			t.Fatal("unknown required scope accepted")
		}
	}
}

func TestScopedSnapshotBudgetCountsEligibleNotes(t *testing.T) {
	t.Setenv("FMN_LIMIT_INDEX_BYTES", "1048576")
	store := newTestStore(t)
	ctx := context.Background()
	archived := "---\nfmn-container: archive\n---\n" + strings.Repeat("x", 400<<10)
	var err error
	for i := 0; i < 3; i++ {
		if _, err = store.Save(ctx, fmt.Sprintf("large-%d.md", i), []byte(archived), "", "test"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = store.Save(ctx, "z-current.md", []byte("# Included\n"), "", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Snapshot(ctx, ""); err == nil {
		t.Fatal("fixture did not exceed configured snapshot budget")
	}
	scope := ContentScope{Policy: ContainerPolicy{Version: 1, Archives: "exclude", Drafts: "exclude"}, Collection: "all"}
	got, err := store.SnapshotScoped(ctx, "", scope)
	if err != nil || len(got.Files) != 1 || got.Files[0].Path != "z-current.md" {
		t.Fatalf("excluded bytes consumed eligible bound: %+v %v", got, err)
	}
}
