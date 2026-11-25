package knowledge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNoteMapCacheRebuildAndPagination(t *testing.T) {
	e := retrievalNotes(t, map[string]string{"a.md": "# Original\n[Next](b.md)", "b.md": strings.Repeat("## "+strings.Repeat("long", 800)+"\nbody\n", 12)})
	first, err := e.Map(t.Context(), MapQuery{Limit: 1})
	if err != nil || len(first.Entries) != 1 || first.Next == "" {
		t.Fatalf("first page: %+v %v", first, err)
	}
	second, err := e.Map(t.Context(), MapQuery{Limit: 1, After: first.Next, Revision: first.Revision})
	if err != nil || len(second.Entries) != 1 || second.Next != "" {
		t.Fatalf("oversized entry stalled pagination: %d entries, next=%q: %v", len(second.Entries), second.Next, err)
	}
	if len(second.Entries[0].Backlinks) != 1 {
		t.Fatal("missing backlink")
	}
	cachePath := filepath.Join(e.dir, "note-map.json")
	old := time.Unix(1234, 0)
	if err := os.Chtimes(cachePath, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Map(t.Context(), MapQuery{}); err != nil {
		t.Fatal(err)
	}
	stat, _ := os.Stat(cachePath)
	if !stat.ModTime().Equal(old) {
		t.Fatal("unchanged map rewrote cache")
	}
	raw, _ := os.ReadFile(cachePath)
	var cache noteMapCache
	if err := json.Unmarshal(raw, &cache); err != nil {
		t.Fatal(err)
	}
	entry := cache.Entries["a.md"]
	entry.Title = "Corrupted"
	cache.Entries["a.md"] = entry
	raw, _ = json.Marshal(cache)
	if err := os.WriteFile(cachePath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(database(t, e.Store), e.Store)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Map(t.Context(), MapQuery{Query: "Original"})
	if err != nil || len(got.Entries) != 1 || got.Entries[0].Title != "Original" {
		t.Fatalf("corrupt cache did not rebuild: %+v %v", got, err)
	}
}

func TestNoteMapTracksChangesAndVisibility(t *testing.T) {
	e := retrievalNotes(t, map[string]string{"a.md": "# Start\n[Next](b.md)", "b.md": "# Destination"})
	ctx := t.Context()
	first, err := e.Map(ctx, MapQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	st, err := e.Store.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Store.Save(ctx, "b.md", []byte("# Changed\n## Detail"), st.Revision, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Map(ctx, MapQuery{After: first.Next}); err == nil {
		t.Fatal("stale cursor survived edit")
	}
	changed, err := e.Map(ctx, MapQuery{Query: "Changed"})
	if err != nil || len(changed.Entries) != 1 || len(changed.Entries[0].Backlinks) != 1 {
		t.Fatal(changed, err)
	}
	st, _ = e.Store.Status(ctx)
	if _, err = e.Store.SetContainer(ctx, "a.md", "draft", st.Revision, ""); err != nil {
		t.Fatal(err)
	}
	visible, err := e.Map(ctx, MapQuery{ContainerSelection: ContainerSelection{Drafts: "exclude"}})
	if err != nil || len(visible.Entries) != 1 || len(visible.Entries[0].Backlinks) != 0 {
		t.Fatal("hidden draft leaked into map", visible, err)
	}
	st, _ = e.Store.Status(ctx)
	trashed, err := e.Store.TrashNotes(ctx, []string{"b.md"}, st.Revision, "")
	if err != nil {
		t.Fatal(err)
	}
	gone, err := e.Map(ctx, MapQuery{Query: "Changed"})
	if err != nil || len(gone.Entries) != 0 {
		t.Fatal("trash in map", gone, err)
	}
	listing, err := e.Store.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Store.RestoreTrash(ctx, listing.Entries[0].ID, "restored.md", trashed.Revision, ""); err != nil {
		t.Fatal(err)
	}
	restored, err := e.Map(ctx, MapQuery{Query: "Changed"})
	if err != nil || len(restored.Entries) != 1 || restored.Entries[0].Path != "restored.md" || restored.Entries[0].NoteID != changed.Entries[0].NoteID {
		t.Fatal("restored note identity lost", restored, err)
	}
}
