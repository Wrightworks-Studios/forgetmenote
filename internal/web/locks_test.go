package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
)

func TestProjectLocksEnforcePermissionsAndSurviveBackup(t *testing.T) {
	app, store := fixture(t)
	lockManuscript(t, store)
	ctx := context.Background()
	doc, err := store.Save(ctx, "notes/chapter.md", []byte("Human text"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	app.catalog.Member(catalog.Bootstrap, "default", "writer", "edit")
	app.catalog.Member(catalog.Bootstrap, "default", "reviewer", "comment")
	body := func(v any) string { raw, _ := json.Marshal(v); return string(raw) }
	change := func(path string, locked bool, revision string) *httptest.ResponseRecorder {
		return call(app, "POST", "/api/locks", body(map[string]any{"path": path, "locked": locked, "revision": revision}), testToken)
	}
	locked := change("notes", true, doc.Revision)
	if locked.Code != 200 {
		t.Fatalf("lock folder: %d %s", locked.Code, locked.Body.String())
	}
	var state struct {
		Revision string                          `json:"revision"`
		Rules    []struct{ Path, Source string } `json:"rules"`
	}
	read := call(app, "GET", "/api/locks?path=notes/chapter.md", "", testToken)
	if err = json.Unmarshal(read.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Rules) != 1 || state.Rules[0].Path != "notes" {
		t.Fatalf("missing inherited lock: %s", read.Body.String())
	}
	for _, path := range []string{"notes/chapter.md", "notes/new.md"} {
		denied := call(app, "POST", "/api/note", body(noteInput{Path: path, Content: "changed", Revision: state.Revision}), testToken)
		if denied.Code != 403 {
			t.Fatalf("locked save %s: %d", path, denied.Code)
		}
	}
	denied := proxyCall(app, "writer", "POST", "/p/default/api/locks", body(map[string]any{"path": "notes", "locked": false, "revision": state.Revision}))
	if denied.Code != 403 {
		t.Fatalf("editor can unlock: %d", denied.Code)
	}
	if change("notes", false, doc.Revision).Code != 409 {
		t.Fatal("stale unlock accepted")
	}
	if change("../escape", true, state.Revision).Code != 403 {
		t.Fatal("unsafe lock path accepted")
	}
	if _, err = store.Save(ctx, ".gitnotes-locks.json", []byte("{}"), state.Revision, "writer"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatalf("direct policy overwrite: %v", err)
	}
	// Comments remain separate from the protected note.
	comment := proxyCall(app, "reviewer", "POST", "/p/default/api/comments", body(map[string]string{"path": doc.Path, "text": "Feedback", "revision": state.Revision}))
	if comment.Code != 200 {
		t.Fatalf("locked note comment: %d %s", comment.Code, comment.Body.String())
	}
	// A notebook restored from its backup keeps its locks.
	bundle := filepath.Join(t.TempDir(), "backup.bundle")
	if err = store.Backup(ctx, bundle); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if w := importRequest(t, app, "restored", raw, catalog.Bootstrap, app.origin); w.Code != 200 {
		t.Fatalf("restore: %d %s", w.Code, w.Body)
	}
	restored, err := app.catalog.Access(catalog.Bootstrap, "restored", "edit")
	if err != nil {
		t.Fatal(err)
	}
	restoredDoc, err := restored.Read(ctx, doc.Path, "")
	if err != nil || string(restoredDoc.Content) != "Human text" {
		t.Fatalf("restore content: %v", err)
	}
	if _, err = restored.Save(ctx, doc.Path, []byte("changed"), restoredDoc.Revision, "test"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatalf("restored lock missing: %v", err)
	}
	if _, err = store.Save(ctx, "notes-other/a.md", []byte("allowed"), "", "test"); err != nil {
		t.Fatal(err)
	}
	status, _ := store.Status(ctx)
	if change("notes", false, status.Revision).Code != 200 {
		t.Fatal("unlock failed")
	}
	status, _ = store.Status(ctx)
	if _, err = store.Save(ctx, doc.Path, []byte("Allowed revision"), status.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	status, _ = store.Status(ctx)
	if change(doc.Path, true, status.Revision).Code != 200 {
		t.Fatal("note lock failed")
	}
	if _, err = store.Save(ctx, "notes/sibling.md", []byte("allowed"), "", "test"); err != nil {
		t.Fatal("note lock affected sibling", err)
	}
	status, _ = store.Status(ctx)
	if change("manuscript", false, status.Revision).Code != 200 {
		t.Fatal("admin cannot manage manuscript lock through UI")
	}
	if err = app.catalog.Create(catalog.Bootstrap, "other", "Other"); err != nil {
		t.Fatal(err)
	}
	other, _ := app.catalog.Access(catalog.Bootstrap, "other", "edit")
	if _, err = other.Save(ctx, doc.Path, []byte("Other project"), "", "test"); err != nil {
		t.Fatal("lock crossed project boundary", err)
	}
}
