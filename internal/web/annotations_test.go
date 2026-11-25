package web

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
	"testing"
)

func TestPassageCommentsRespectPermissionsAndPreserveLockedNotes(t *testing.T) {
	app, store := fixture(t)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	for name, role := range map[string]string{"reader": "read", "reviewer": "comment", "other": "comment"} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", name, role); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	doc, err := store.Save(ctx, "a.md", []byte("# Stars\n\nA **bright star** shines.\n"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	call := func(user, path string, body any) (int, workspace.Discussion) {
		t.Helper()
		raw, _ := json.Marshal(body)
		w := proxyCall(app, user, "POST", "/p/default/api/"+path, string(raw))
		var d workspace.Discussion
		json.Unmarshal(w.Body.Bytes(), &d)
		return w.Code, d
	}
	locked, err := store.SetLock(ctx, "a.md", true, doc.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"path": "a.md", "revision": locked.Revision, "text": "Check the star", "anchor": map[string]string{"quote": "bright star", "prefix": "Stars A ", "suffix": " shines."}}
	if code, _ := call("reader", "comments", body); code != 403 {
		t.Fatalf("reader comment: %d", code)
	}
	if code, _ := call("reviewer", "note", map[string]any{"path": "a.md", "content": "Changed", "revision": doc.Revision}); code != 403 {
		t.Fatalf("commenter edit: %d", code)
	}
	code, d := call("reviewer", "comments", body)
	if code != 200 || len(d.Comments) != 1 || d.Comments[0].Anchor == nil {
		t.Fatalf("anchor: %d %+v", code, d)
	}
	id := d.Comments[0].ID
	code, d = call("other", "comments", map[string]any{"path": "a.md", "revision": d.Revision, "text": "Agreed", "replyTo": id})
	if code != 200 || len(d.Comments) != 1 || len(d.Comments[0].Replies) != 1 {
		t.Fatalf("reply: %d %+v", code, d)
	}
	if code, _ = call("other", "comments/resolve", map[string]any{"path": "a.md", "revision": d.Revision, "id": id}); code != 403 {
		t.Fatalf("non-author resolve: %d", code)
	}
	code, d = call("reviewer", "comments/resolve", map[string]any{"path": "a.md", "revision": d.Revision, "id": id})
	if code != 200 || !d.Comments[0].Resolved {
		t.Fatalf("resolve: %d %+v", code, d)
	}
	if code, _ = call("reviewer", "comments", map[string]any{"path": "a.md", "revision": d.Revision, "text": "Late reply", "replyTo": id}); code == 200 {
		t.Fatal("resolved thread accepted reply")
	}
	note, err := store.Read(ctx, "a.md", "")
	if err != nil || string(note.Content) != string(doc.Content) {
		t.Fatal("comments changed the note")
	}
}
