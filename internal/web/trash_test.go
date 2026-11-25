package web

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/workspace"
	"testing"
)

func TestTrashHTTPAndMCP(t *testing.T) {
	app, s := fixture(t)
	ctx := context.Background()
	doc, err := s.Save(ctx, "discard.md", []byte("# Discard me\n"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"paths": []string{doc.Path}, "revision": doc.Revision})
	if w := call(app, "POST", "/api/trash", string(body), testToken); w.Code != 200 {
		t.Fatalf("Trash action: %d %s", w.Code, w.Body.String())
	}
	client := actionClient(t, app, "@local-admin", "notes:read notes:write notes:manage")
	raw := actionCall(t, client, "list_trash", map[string]any{"project": "default"}, false)
	var listing workspace.TrashListing
	if err = json.Unmarshal(raw, &listing); err != nil || len(listing.Entries) != 1 {
		t.Fatal(string(raw), err)
	}
	actionCall(t, client, "empty_trash", map[string]any{"project": "default", "revision": listing.Revision}, true)
	actionCall(t, client, "restore_trash", map[string]any{"project": "default", "id": listing.Entries[0].ID, "revision": listing.Revision}, false)
	if _, err = s.Read(ctx, doc.Path, ""); err != nil {
		t.Fatal(err)
	}
}
func TestTrashPermissionsAndGroupedDeleteParity(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	for _, role := range []string{"read", "comment", "edit", "owner"} {
		if err := app.catalog.Member("@local-admin", "default", role, role); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	doc, err := store.Save(ctx, "permissions.md", []byte("permissions proof"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"paths": []string{doc.Path}, "revision": doc.Revision})
	for _, role := range []string{"read", "comment", "edit"} {
		if w := proxyCall(app, role, "POST", "/p/default/api/trash", string(body)); w.Code != 403 {
			t.Fatalf("%s can trash: %d", role, w.Code)
		}
		client := actionClient(t, app, role, "notes:read notes:write notes:manage")
		actionCall(t, client, "trash_notes", map[string]any{"project": "default", "paths": []string{doc.Path}, "revision": doc.Revision}, true)
		actionCall(t, client, "submit_change_group", map[string]any{"project": "default", "mode": "apply", "group": workspace.EditGroup{ID: "delete-" + role, Summary: "Delete", Edits: []workspace.NoteEdit{{Path: doc.Path, Revision: doc.Revision, Hash: workspace.ContentHash(doc.Content), Delete: true}}}}, true)
	}
	owner := actionClient(t, app, "owner", "notes:read notes:write notes:manage")
	actionCall(t, owner, "trash_notes", map[string]any{"project": "default", "paths": []string{doc.Path}, "revision": doc.Revision}, false)
	listing, err := store.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reader := actionClient(t, app, "read", "notes:read")
	actionCall(t, reader, "list_trash", map[string]any{"project": "default"}, false)
	actionCall(t, reader, "empty_trash", map[string]any{"project": "default", "revision": listing.Revision, "confirmation": listing.Confirmation}, true)
	actionCall(t, reader, "restore_trash", map[string]any{"project": "default", "id": listing.Entries[0].ID, "revision": listing.Revision}, true)
}
