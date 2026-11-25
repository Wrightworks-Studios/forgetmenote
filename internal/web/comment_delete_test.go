package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
)

func TestDeleteOwnCommentHTTP(t *testing.T) {
	app, store := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "author", "comment"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	note, err := store.Save(ctx, "comment.md", []byte("Original note"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	discussion, err := store.AddComment(ctx, note.Path, "Delete my comment", note.Revision, "author")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]string{"path": note.Path, "id": discussion.Comments[0].ID, "revision": discussion.Revision})
	if err != nil {
		t.Fatal(err)
	}
	response := proxyCall(app, "author", "POST", "/p/default/api/comments/delete", string(raw))
	if response.Code != 200 {
		t.Fatalf("delete own comment: %d %s", response.Code, response.Body)
	}
	var deleted workspace.Discussion
	if err := json.Unmarshal(response.Body.Bytes(), &deleted); err != nil {
		t.Fatal(err)
	}
	if len(deleted.Comments) != 0 || deleted.Revision == discussion.Revision {
		t.Fatalf("comment was not deleted in a new revision: %+v", deleted)
	}
}

func TestCommentDeletionBrowserPermissionsAndReplyPreservation(t *testing.T) {
	app, store := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	for user, role := range map[string]string{"author": "comment", "other": "comment", "editor": "edit", "reader": "read", "owner": "owner"} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", user, role); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "admin", Admin: true}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	note, err := store.Save(ctx, "comment.md", []byte("A bright star shines."), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	locked, err := store.SetLock(ctx, note.Path, true, note.Revision, "owner")
	if err != nil {
		t.Fatal(err)
	}
	d, err := store.AddComment(ctx, note.Path, "Parent content", locked.Revision, "author", workspace.CommentOptions{Anchor: &workspace.CommentAnchor{Quote: "bright star"}})
	if err != nil {
		t.Fatal(err)
	}
	parent := d.Comments[0]
	d, err = store.AddComment(ctx, note.Path, "Other person's reply", d.Revision, "other", workspace.CommentOptions{ReplyTo: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	otherReply := d.Comments[0].Replies[0]
	d, err = store.AddComment(ctx, note.Path, "Author's reply", d.Revision, "author", workspace.CommentOptions{ReplyTo: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	ownReply := d.Comments[0].Replies[1]
	remove := func(user, id, reply, revision string, want int) workspace.Discussion {
		t.Helper()
		raw, err := json.Marshal(map[string]string{"path": note.Path, "id": id, "replyId": reply, "revision": revision})
		if err != nil {
			t.Fatal(err)
		}
		response := proxyCall(app, user, "POST", "/p/default/api/comments/delete", string(raw))
		if response.Code != want {
			t.Fatalf("delete as %s: want %d, got %d %s", user, want, response.Code, response.Body)
		}
		var out workspace.Discussion
		if want == 200 {
			if err := json.Unmarshal(response.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	for _, user := range []string{"other", "editor", "reader"} {
		remove(user, parent.ID, "", d.Revision, 403)
	}
	remove("author", parent.ID, otherReply.ID, d.Revision, 403)
	remove("author", parent.ID, "", note.Revision, 409)
	remove("author", "missing", "", d.Revision, 404)
	remove("owner", parent.ID, "missing", d.Revision, 404)
	for _, role := range []string{"read", ""} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", "author", role); err != nil {
			t.Fatal(err)
		}
		remove("author", parent.ID, "", d.Revision, 403)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "author", "comment"); err != nil {
		t.Fatal(err)
	}
	before, err := store.Comments(ctx, note.Path)
	if err != nil || !reflect.DeepEqual(before, d) {
		t.Fatalf("rejected delete changed discussion: %+v %v", before, err)
	}
	d = remove("author", parent.ID, ownReply.ID, d.Revision, 200)
	if len(d.Comments) != 1 || d.Comments[0].Deleted || d.Comments[0].Text != parent.Text || !reflect.DeepEqual(d.Comments[0].Replies, []workspace.CommentReply{otherReply}) {
		t.Fatalf("reply delete changed another author's content: %+v", d)
	}
	d = remove("author", parent.ID, "", d.Revision, 200)
	if len(d.Comments) != 1 || !d.Comments[0].Deleted || d.Comments[0].Text != "" || d.Comments[0].Resolved || !reflect.DeepEqual(d.Comments[0].Anchor, parent.Anchor) || !reflect.DeepEqual(d.Comments[0].Replies, []workspace.CommentReply{otherReply}) {
		t.Fatalf("deleted parent failed to preserve thread: %+v", d)
	}
	remove("author", parent.ID, "", d.Revision, 404)
	d = remove("other", parent.ID, otherReply.ID, d.Revision, 200)
	if len(d.Comments) != 0 {
		t.Fatalf("empty deleted thread survived: %+v", d)
	}
	for _, user := range []string{"owner", "admin"} {
		d, err = store.AddComment(ctx, note.Path, "Moderate this comment", d.Revision, "other")
		if err != nil {
			t.Fatal(err)
		}
		d = remove(user, d.Comments[0].ID, "", d.Revision, 200)
		if len(d.Comments) != 0 {
			t.Fatalf("%s moderation failed: %+v", user, d)
		}
	}
	got, err := store.Read(ctx, note.Path, "")
	if err != nil || string(got.Content) != string(note.Content) {
		t.Fatalf("deletion changed note: %+v %v", got, err)
	}
	admin, err := app.catalog.Account("admin")
	if err != nil {
		t.Fatal(err)
	}
	adminIdentity := admin.Identity()
	history, err := store.History(ctx, "comments")
	if err != nil || len(history) != 10 || history[0].Source != "browser" || history[0].UserID != adminIdentity.ID {
		t.Fatalf("comment deletion Git history/identity: %+v %v", history, err)
	}
}

func TestCommentDeletionMCPRechecksScopeAndMembership(t *testing.T) {
	app, store := fixture(t)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "author", "comment"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	note, err := store.Save(ctx, "comment.md", []byte("Shared note"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	d, err := store.AddComment(ctx, note.Path, "Parent", note.Revision, "author")
	if err != nil {
		t.Fatal(err)
	}
	d, err = store.AddComment(ctx, note.Path, "Other reply", d.Revision, "other", workspace.CommentOptions{ReplyTo: d.Comments[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"project": "default", "path": note.Path, "id": d.Comments[0].ID, "revision": d.Revision}
	readonly := actionClient(t, app, "author", "notes:read")
	actionCall(t, readonly, "delete_comment", args, true)
	writable := actionClient(t, app, "author", "notes:read notes:write")
	args["replyId"] = d.Comments[0].Replies[0].ID
	actionCall(t, writable, "delete_comment", args, true)
	delete(args, "replyId")
	args["revision"] = note.Revision
	actionCall(t, writable, "delete_comment", args, true)
	args["revision"] = d.Revision
	for _, role := range []string{"read", ""} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", "author", role); err != nil {
			t.Fatal(err)
		}
		actionCall(t, writable, "delete_comment", args, true)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "author", "comment"); err != nil {
		t.Fatal(err)
	}
	raw := actionCall(t, writable, "delete_comment", args, false)
	var deleted workspace.Discussion
	if err := json.Unmarshal(raw, &deleted); err != nil {
		t.Fatal(err)
	}
	if len(deleted.Comments) != 1 || !deleted.Comments[0].Deleted || !reflect.DeepEqual(deleted.Comments[0].Replies, d.Comments[0].Replies) {
		t.Fatalf("MCP lost remaining reply: %s", raw)
	}
	author, err := app.catalog.Account("author")
	if err != nil {
		t.Fatal(err)
	}
	authorIdentity := author.Identity()
	history, err := store.History(ctx, "comments")
	if err != nil || len(history) != 3 || history[0].Source != "mcp" || history[0].UserID != authorIdentity.ID || history[0].Client != "actions-client" {
		t.Fatalf("MCP deletion attribution: %+v %v", history, err)
	}
}

func TestCommentDeletionChatRequiresExplicitApproval(t *testing.T) {
	for _, mode := range []string{"ask", "suggest", "edit"} {
		if allowedAction(mode, "delete_comment", json.RawMessage(`{}`), []string{"comments", "notes"}, false) {
			t.Fatalf("%s auto-approved deletion", mode)
		}
		if !allowedAction(mode, "delete_comment", json.RawMessage(`{}`), nil, true) {
			t.Fatalf("%s rejected explicit approval", mode)
		}
	}
}

// The callback runs only when the handler starts decoding, after route admission.
type commentDeleteRequestBody struct {
	io.Reader
	revoke func()
}

func (b *commentDeleteRequestBody) Read(p []byte) (int, error) {
	if b.revoke != nil {
		b.revoke()
		b.revoke = nil
	}
	return b.Reader.Read(p)
}
func TestCommentDeletionRechecksAccessAfterReadingBody(t *testing.T) {
	app, store := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "author", "comment"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	note, err := store.Save(ctx, "note.md", []byte("Shared note"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	d, err := store.AddComment(ctx, note.Path, "Retain after revocation", note.Revision, "author")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]string{"path": note.Path, "id": d.Comments[0].ID, "revision": d.Revision})
	if err != nil {
		t.Fatal(err)
	}
	body := &commentDeleteRequestBody{Reader: strings.NewReader(string(raw)), revoke: func() {
		if err := app.catalog.Member(catalog.Bootstrap, "default", "author", "read"); err != nil {
			t.Fatal(err)
		}
	}}
	request := httptest.NewRequest("POST", app.origin+"/p/default/api/comments/delete", body)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Remote-User", "author")
	request.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
	request.Header.Set("Origin", app.origin)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatalf("permission revoked during body read: %d %s", response.Code, response.Body)
	}
	latest, err := store.Comments(ctx, note.Path)
	if err != nil || !reflect.DeepEqual(latest, d) {
		t.Fatalf("revoked request changed content: %+v %v", latest, err)
	}
}
