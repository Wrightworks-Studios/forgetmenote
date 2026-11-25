package web

import (
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
	"testing"
)

// Each tool result pins its citations. Citations at the current revision
// reuse the current scoped snapshot instead of traversing the notebook again.
// The notes store serves that traversal from memory, without the Git
// commands this test once counted, so it checks what the snapshot pins.
func TestChatCitationsAtCurrentRevisionPinWithOneSnapshot(t *testing.T) {
	_, store := fixture(t)
	if _, err := store.Save(t.Context(), "notes/dependency.md", []byte("# Dependency\nSynthetic dependency.\n"), "", catalog.Bootstrap); err != nil {
		t.Fatal(err)
	}
	content := "# Cited\nSynthetic claim from [the dependency](dependency.md).\n"
	doc, err := store.Save(t.Context(), "notes/cited.md", []byte(content), "", catalog.Bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	run := chat.Run{ID: "pin-run", Options: chat.Options{Corpus: &chat.Corpus{Version: 1, Archives: "exclude", Drafts: "exclude"}}}
	ctx, err := withChatRunScope(t.Context(), chat.Conversation{Owner: catalog.Bootstrap, Project: "default", ID: "pin-chat", Turns: []chat.Turn{{Role: "user", RunID: run.ID}}}, run)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := chatScopedSnapshot(ctx, store, doc.Revision)
	if err != nil {
		t.Fatal(err)
	}
	want := knowledge.ReviewFingerprints(snapshot)[doc.Path]
	if want == "" {
		t.Fatal("fixture has no review fingerprint")
	}
	citations := []chat.Citation{{ID: "cited", Path: doc.Path, Revision: doc.Revision, Hash: workspace.ContentHash([]byte(content))}}
	if err = pinChatCitationReferences(ctx, store, citations); err != nil {
		t.Fatal(err)
	}
	if citations[0].References != want || citations[0].Scope != "current" {
		t.Fatalf("pinned citation: %+v", citations[0])
	}
}
