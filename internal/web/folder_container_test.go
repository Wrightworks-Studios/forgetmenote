package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
)

func folderContainerFixture(t *testing.T, notes map[string]string) (*Server, *workspace.Store) {
	t.Helper()
	app := chatFixture(t)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	edits := []workspace.NoteEdit{}
	for path, content := range notes {
		edits = append(edits, workspace.NoteEdit{Path: path, Content: content})
	}
	for start := 0; start < len(edits); start += 50 {
		if _, err := store.ApplyGroup(context.Background(), workspace.EditGroup{ID: fmt.Sprint("seed-", start), Summary: "seed", Edits: edits[start:min(len(edits), start+50)]}); err != nil {
			t.Fatal(err)
		}
	}
	return app, store
}

func containerOf(t *testing.T, store *workspace.Store, path string) string {
	t.Helper()
	doc, err := store.Read(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	container, err := workspace.ParseContainer(doc.Content)
	if err != nil {
		t.Fatal(err)
	}
	return container
}

func TestFolderMovesEveryNoteToArchivesAndBack(t *testing.T) {
	app, store := folderContainerFixture(t, map[string]string{
		"Field/a.md":     "# A\nBody.",
		"Field/b.md":     "---\ntags: [x]\n---\n# B\nBody.",
		"Field/sub/c.md": "# C\nBody.",
		"Fieldwork.md":   "# Not in the folder\n",
		"other.md":       "# Other\n",
	})
	status, _ := store.Status(context.Background())
	w := proxyCall(app, "alice", "POST", "/p/default/api/folder-container", `{"folder":"Field","container":"archive","revision":"`+status.Revision+`"}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var out struct {
		Revision string `json:"revision"`
		Changed  int    `json:"changed"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Changed != 3 || out.Revision == status.Revision {
		t.Fatalf("result: %+v", out)
	}
	for path, want := range map[string]string{"Field/a.md": "archive", "Field/b.md": "archive", "Field/sub/c.md": "archive", "Fieldwork.md": "", "other.md": ""} {
		if got := containerOf(t, store, path); got != want {
			t.Fatalf("%s: container %q, want %q", path, got, want)
		}
	}
	if doc, _ := store.Read(context.Background(), "Field/b.md", ""); !strings.Contains(string(doc.Content), "tags: [x]") {
		t.Fatalf("other front matter lost: %s", doc.Content)
	}
	history, err := store.History(context.Background(), "Field/a.md")
	if err != nil || len(history) == 0 || history[0].Revision != out.Revision {
		t.Fatalf("folder change was not one commit: %+v %v", history, err)
	}
	w = proxyCall(app, "alice", "POST", "/p/default/api/folder-container", `{"folder":"Field","container":"","revision":"`+out.Revision+`"}`)
	if w.Code != 200 || containerOf(t, store, "Field/sub/c.md") != "" {
		t.Fatal("return to Notes failed", w.Code, w.Body.String())
	}
}

func TestFolderContainerRejectsStaleTrashAndReaders(t *testing.T) {
	app, store := folderContainerFixture(t, map[string]string{"Field/a.md": "# A\n", "Field/b.md": "# B\n"})
	status, _ := store.Status(context.Background())
	doc, _ := store.Read(context.Background(), "Field/b.md", "")
	if _, err := store.Save(context.Background(), "Field/b.md", []byte("# B\nChanged elsewhere."), doc.Revision, ""); err != nil {
		t.Fatal(err)
	}
	if w := proxyCall(app, "alice", "POST", "/p/default/api/folder-container", `{"folder":"Field","container":"archive","revision":"`+status.Revision+`"}`); w.Code != 409 {
		t.Fatal("stale folder snapshot accepted", w.Code, w.Body.String())
	}
	if containerOf(t, store, "Field/a.md") != "" {
		t.Fatal("conflict left a partial change")
	}
	current, _ := store.Status(context.Background())
	for _, folder := range []string{"", "Trash", "Trash/x", "../Field"} {
		if w := proxyCall(app, "alice", "POST", "/p/default/api/folder-container", `{"folder":"`+folder+`","container":"archive","revision":"`+current.Revision+`"}`); w.Code < 400 || w.Code >= 500 {
			t.Fatalf("folder %q accepted: %d", folder, w.Code)
		}
	}
	if w := proxyCall(app, "bob", "POST", "/p/default/api/folder-container", `{"folder":"Field","container":"archive","revision":"`+current.Revision+`"}`); w.Code != 403 {
		t.Fatal("reader archived a folder", w.Code)
	}
}

func TestLargeFolderMovesInBoundedGroups(t *testing.T) {
	notes := map[string]string{}
	for i := range 51 {
		notes[fmt.Sprintf("Big/n%02d.md", i)] = fmt.Sprintf("# Note %d\n", i)
	}
	app, store := folderContainerFixture(t, notes)
	status, _ := store.Status(context.Background())
	w := proxyCall(app, "alice", "POST", "/p/default/api/folder-container", `{"folder":"Big","container":"archive","revision":"`+status.Revision+`"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"changed":51`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if containerOf(t, store, "Big/n00.md") != "archive" || containerOf(t, store, "Big/n50.md") != "archive" {
		t.Fatal("not every note moved")
	}
}

// A lock blocks the folder move, and the refusal names the lock instead of
// reporting an inaccessible path.
func TestLockedFolderMoveExplainsTheLock(t *testing.T) {
	for _, tc := range []struct{ lock, want string }{
		{"Field", "“Field” is locked"},
		{"Field/sub/c.md", "“Field/sub/c.md” is locked"},
	} {
		t.Run(tc.lock, func(t *testing.T) {
			app, store := folderContainerFixture(t, map[string]string{"Field/a.md": "# A\n", "Field/sub/c.md": "# C\n"})
			status, _ := store.Status(context.Background())
			if _, err := store.SetLock(context.Background(), tc.lock, true, status.Revision, ""); err != nil {
				t.Fatal(err)
			}
			current, _ := store.Status(context.Background())
			w := proxyCall(app, "alice", "POST", "/p/default/api/folder-container", `{"folder":"Field","container":"archive","revision":"`+current.Revision+`"}`)
			if w.Code != 403 || !strings.Contains(w.Body.String(), tc.want) || !strings.Contains(w.Body.String(), `"code":"locked"`) {
				t.Fatalf("locked folder: %d %s", w.Code, w.Body.String())
			}
			if containerOf(t, store, "Field/a.md") != "" {
				t.Fatal("a locked folder move changed a note")
			}
		})
	}
}
