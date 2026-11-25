package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
)

// endOnRewrite runs request, with ctx, once the purge it runs has rewritten
// the notebook's history: it holds the purge while it rebuilds the Git
// export, calls end, and lets it go on.
func endOnRewrite(t *testing.T, request func(), end func()) {
	t.Helper()
	began, release := holdGit(t, "prune")
	done := make(chan struct{})
	go func() {
		defer close(done)
		request()
	}()
	for deadline := time.Now().Add(30 * time.Second); !began(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the purge never rebuilt the export")
		}
	}
	end()
	// The request's end reaches the server before the purge goes on.
	time.Sleep(200 * time.Millisecond)
	release()
	<-done
}

// quotedPurgeSecret saves a note holding a secret that a chat and a Jev
// record quote, and has an agent prepare a purge plan of it.
func quotedPurgeSecret(t *testing.T, app *Server, store *workspace.Store, path string) (string, chat.Conversation) {
	t.Helper()
	secret := webPurgeSecret()
	first := savePurgeNote(t, store, path, "# Keys\n\nThe staging key is "+secret+".\n", "")
	savePurgeNote(t, store, path, "# Keys\n\nThe staging key is "+secret+", rotated soon.\n", first.Revision)
	conversation, err := app.chat.Store.Create(catalog.Bootstrap, "default", chat.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.chat.Store.Update(catalog.Bootstrap, "default", conversation.ID, func(c *chat.Conversation) error {
		c.Title = "Keys"
		c.AddEvent("run-1", "tool", map[string]any{"result": "Read notes/keys.md:\nThe staging key is " + secret + "."})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.jevState.records.Write("jobs/"+strings.Repeat("d", 64)+".json", map[string]any{"version": 2, "project": "default", "message": "Found:\nThe staging key is " + secret + "."}); err != nil {
		t.Fatal(err)
	}
	return secret, conversation
}

// holdingPurgeSecret counts the stored chat transcripts, Jev records and
// prepared purge plans that still hold secret.
func holdingPurgeSecret(t *testing.T, app *Server, secret string) (chats, jev, plans int) {
	t.Helper()
	ctx := context.Background()
	if err := app.db.Pool.QueryRow(ctx, `SELECT count(*) FROM chat_conversations WHERE position(convert_to($1, 'UTF8') IN record) > 0`, secret).Scan(&chats); err != nil {
		t.Fatal(err)
	}
	if err := app.db.Pool.QueryRow(ctx, `SELECT count(*) FROM jev_records WHERE position($1 IN body::text) > 0`, secret).Scan(&jev); err != nil {
		t.Fatal(err)
	}
	if err := app.db.Pool.QueryRow(ctx, `SELECT count(*) FROM pending_flows WHERE position(convert_to($1, 'UTF8') IN body) > 0`, secret).Scan(&plans); err != nil {
		t.Fatal(err)
	}
	return chats, jev, plans
}

// A confirmed purge runs to its end even when its request ends once the
// history is rewritten, as when the person reloads or closes the tab, or
// presses Ctrl-C in the operator command: the chat transcripts, Jev records
// and prepared plans that quoted the content are redacted all the same, and
// the receipt says so without warnings. A note purge could never be planned
// again to finish that.
func TestPurgeWhoseRequestEndsAfterTheRewriteStillCleansUp(t *testing.T) {
	for _, kind := range []string{workspace.PurgeText, workspace.PurgeNote} {
		t.Run(kind, func(t *testing.T) {
			app, store := fixture(t)
			secret, conversation := quotedPurgeSecret(t, app, store, "notes/keys.md")
			scope := workspace.PurgeScope{Kind: kind, Text: secret}
			if kind == workspace.PurgeNote {
				scope = workspace.PurgeScope{Kind: kind, Path: "notes/keys.md"}
			}
			origin := app.origin
			session := actionClient(t, app, catalog.Bootstrap, "notes:read notes:write notes:manage")
			actionCall(t, session, "prepare_purge", map[string]any{"project": "default", "kind": "text", "text": secret}, false)
			app.origin = origin
			plan := preparePurgeHTTP(t, app, scope)
			if plan.Derived.Chats != 1 || plan.Derived.JevRecords != 1 {
				t.Fatalf("plan: %+v", plan.Derived)
			}
			body, _ := json.Marshal(map[string]any{"scope": plan.Scope, "revision": plan.Revision, "confirmation": plan.Confirmation, "reason": "Leaked staging key"})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := httptest.NewRequestWithContext(ctx, "POST", "http://localhost/p/default/api/purge/execute", strings.NewReader(string(body)))
			r.Header.Set("Authorization", "Bearer "+testToken)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			endOnRewrite(t, func() { app.ServeHTTP(w, r) }, cancel)
			var receipt purgeReceipt
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || len(receipt.Warnings) > 0 || receipt.Derived.Chats != 1 || receipt.Derived.JevRecords != 1 {
				t.Fatalf("execute: HTTP %d %s", w.Code, w.Body)
			}
			if chats, jev, plans := holdingPurgeSecret(t, app, secret); chats+jev+plans > 0 {
				t.Fatalf("still holding the secret: %d chats, %d Jev records, %d plans", chats, jev, plans)
			}
			c, err := app.chat.Store.Read(catalog.Bootstrap, "default", conversation.ID)
			raw, _ := json.Marshal(c)
			if err != nil || strings.Contains(string(raw), secret) || !strings.Contains(string(raw), workspace.PurgedText) {
				t.Fatalf("transcript: %s %v", raw, err)
			}
		})
	}
}

// A browser that disconnects once the history is rewritten leaves the purge
// to finish on the server, which keeps a receipt without warnings.
func TestPurgeWhoseClientDisconnectsAfterTheRewriteStillCleansUp(t *testing.T) {
	app, store := fixture(t)
	secret, _ := quotedPurgeSecret(t, app, store, "notes/keys.md")
	plan := preparePurgeHTTP(t, app, workspace.PurgeScope{Kind: workspace.PurgeText, Text: secret})
	server := httptest.NewServer(app)
	defer server.Close()
	body, _ := json.Marshal(map[string]any{"scope": plan.Scope, "revision": plan.Revision, "confirmation": plan.Confirmation, "reason": "Leaked staging key"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/p/default/api/purge/execute", strings.NewReader(string(body)))
	request.Host = "localhost"
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	endOnRewrite(t, func() {
		if response, err := http.DefaultClient.Do(request); err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			t.Error("the client got the answer before it disconnected:", response.StatusCode)
		}
	}, cancel)
	var receipts struct {
		Receipts []purgeReceipt `json:"receipts"`
	}
	for deadline := time.Now().Add(30 * time.Second); len(receipts.Receipts) == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the purge kept no receipt")
		}
		json.Unmarshal(call(app, "GET", "/api/purge", "", testToken).Body.Bytes(), &receipts)
	}
	if r := receipts.Receipts[0]; len(r.Warnings) > 0 || r.Derived.Chats != 1 || r.Derived.JevRecords != 1 {
		t.Fatalf("receipt: %+v", r)
	}
	if chats, jev, _ := holdingPurgeSecret(t, app, secret); chats+jev > 0 {
		t.Fatalf("still holding the secret: %d chats, %d Jev records", chats, jev)
	}
}
