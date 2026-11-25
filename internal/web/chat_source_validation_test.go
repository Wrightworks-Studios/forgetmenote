package web

import (
	"errors"
	"fmt"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
)

// Citations of another notebook are reread at their pinned revisions and, for
// a current-only corpus, compared with the notes' current content.
func TestChatSourceValidationRereadsOtherNotebookCitations(t *testing.T) {
	app := chatFixture(t)
	if err := app.catalog.Create(catalog.Bootstrap, "other", "Other notebook"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "other", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	other, err := app.catalog.Access(catalog.Bootstrap, "other", "edit")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	cite := func(path string) chat.Citation {
		doc, err := other.Read(ctx, path, "")
		if err != nil {
			t.Fatal(err)
		}
		return chat.Citation{ID: path, Project: "other", Path: path, URL: "/?project=other&note=" + path + "&revision=" + doc.Revision, Revision: doc.Revision, Hash: workspace.ContentHash(doc.Content), Kind: "notebook", Scope: "notebook"}
	}
	for i := range 6 {
		if _, err = other.Save(ctx, fmt.Sprintf("notes/n%d.md", i), []byte(fmt.Sprintf("# Note %d\nSynthetic evidence %d.", i, i)), "", "operator"); err != nil {
			t.Fatal(err)
		}
	}
	// n0 is pinned at an older revision but unchanged since.
	citations := []chat.Citation{cite("notes/n0.md")}
	if _, err = other.Save(ctx, "notes/later.md", []byte("# Later\nUnrelated."), "", "operator"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 6; i++ {
		citations = append(citations, cite(fmt.Sprintf("notes/n%d.md", i)))
	}
	conversation, err := app.chat.Store.Create("alice", "default", chat.Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	run := chat.Run{ID: chat.NewID(), Options: chat.Options{Mode: "ask", Corpus: &chat.Corpus{Version: 1, Archives: "exclude", Drafts: "exclude", NoteProjects: []string{"other"}}}, Citations: citations}
	conversation.Turns = append(conversation.Turns, chat.Turn{ID: chat.NewID(), RunID: run.ID, Role: "user", Content: "Synthetic question"})
	base, err := app.runContext(chat.RunAuth{User: "alice", Project: "default", Epoch: app.catalog.Epoch("alice")})
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := chat.WithRunScope(base, conversation, run)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.validateChatSources(scoped, run, false); err != nil {
		t.Fatalf("unchanged citations rejected: %v", err)
	}
	revise := func(content string) {
		t.Helper()
		doc, err := other.Read(ctx, "notes/n3.md", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = other.Save(ctx, "notes/n3.md", []byte(content), doc.Revision, "operator"); err != nil {
			t.Fatal(err)
		}
	}
	revise("# Note 3\nRevised evidence.")
	if err = app.validateChatSources(scoped, run, false); !errors.Is(err, workspace.ErrConflict) {
		t.Fatalf("changed cited note accepted: %v", err)
	}
	history := run
	history.Options.Corpus = &chat.Corpus{Version: 1, Archives: "exclude", Drafts: "exclude", History: true, NoteProjects: []string{"other"}}
	if scoped, err = chat.WithRunScope(base, conversation, history); err != nil {
		t.Fatal(err)
	}
	if err = app.validateChatSources(scoped, history, false); err != nil {
		t.Fatalf("history corpus rejected its pinned revisions: %v", err)
	}
	revise("---\nfmn-container: draft\n---\n# Note 3\nDraft.")
	current := run
	current.Citations = []chat.Citation{cite("notes/n3.md")}
	if scoped, err = chat.WithRunScope(base, conversation, current); err != nil {
		t.Fatal(err)
	}
	if err = app.validateChatSources(scoped, current, false); !errors.Is(err, chat.ErrOutsideCorpus) {
		t.Fatalf("note outside the corpus accepted: %v", err)
	}
}
