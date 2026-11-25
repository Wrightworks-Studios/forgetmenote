package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"net/http"
	"strings"
	"testing"
)

func TestReleaseReviewAApprovalEpoch(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	store, err := app.catalog.Access("alice", "default", "owner")
	if err != nil {
		t.Fatal(err)
	}
	note, err := store.Save(t.Context(), "notes/epoch.md", []byte("before"), "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	status, err := store.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	run := chat.Run{ID: chat.NewID(), State: "running", Rounds: 1, Epoch: app.catalog.Epoch("alice"), Options: chat.Options{Corpus: c.Options.Corpus, Connection: "local", Model: "synthetic", Mode: "edit"}}
	if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Turns = append(c.Turns, chat.Turn{ID: "origin", RunID: run.ID, Role: "user", Content: "Lock the requested note"})
		c.Runs = append(c.Runs, run)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	auth := chat.RunAuth{User: "alice", Project: "default", Epoch: run.Epoch}
	ctx, err := app.runContext(auth)
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"path": note.Path, "locked": true, "revision": status.Revision})
	err = app.executeChatTool(ctx, auth, c.ID, run.ID, chat.ToolCall{ID: "lock", Name: "set_note_lock", Arguments: args}, app.chat.Config.Connections[0])
	if err != errChatConfirmation {
		t.Fatalf("prepare: %v", err)
	}
	prepared, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	op := prepared.Runs[0].Operations[0]
	if err = app.catalog.Revoke(catalog.Bootstrap, "alice"); err != nil {
		t.Fatal(err)
	}
	if app.checkRun(auth) == nil {
		t.Fatal("old epoch remained valid")
	}
	raw, _ := json.Marshal(map[string]string{"decision": "approve", "confirmation": op.Confirmation})
	response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/actions/"+op.ID, string(raw))
	t.Logf("old run epoch=%d current=%d response=%d %s", run.Epoch, app.catalog.Epoch("alice"), response.Code, response.Body)
	if response.Code != http.StatusForbidden {
		t.Fatalf("old confirmation accepted: %d %s", response.Code, response.Body)
	}
	locks, err := store.Locks(t.Context(), note.Path)
	if err != nil || len(locks.Rules) != 0 {
		t.Fatalf("revoked approval mutated locks: %+v %v", locks, err)
	}
	app.chat.Store.Close()
	reopened, err := chat.OpenStore(app.db)
	if err != nil {
		t.Fatal(err)
	}
	app.chat.Store = reopened
	retained, err := reopened.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	invalidated := retained.Run(run.ID)
	if invalidated.State != "cancelled" || invalidated.Usage.Reserved != 0 || invalidated.Operations[0].Confirmation != "" || invalidated.Operations[0].State != "declined" {
		t.Fatalf("revoked confirmation not durably invalidated: %+v", invalidated)
	}
	response = proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/actions/"+op.ID, string(raw))
	if response.Code != http.StatusConflict {
		t.Fatalf("old confirmation replay after reload: %d %s", response.Code, response.Body)
	}
}

func TestReleaseReviewAInstructionsInContext(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "owner")
	if err != nil {
		t.Fatal(err)
	}
	marker := "OWNER_POLICY_UNIQUE_MARKER: Every answer must be written in Swedish."
	if _, err = store.WriteInstructions(t.Context(), []byte(marker), "", "operator"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(t.Context(), "notes/zebras.md", []byte("Zebras have stripes."), "", "operator"); err != nil {
		t.Fatal(err)
	}
	run := chat.Run{ID: chat.NewID(), Options: chat.Options{Corpus: c.Options.Corpus, Connection: "local", Model: "synthetic", Mode: "ask"}}
	c.Turns = []chat.Turn{{ID: chat.NewID(), RunID: run.ID, Role: "user", Content: "What markings do zebras have?"}}
	msgs, _, _, err := app.initialChatMessages(t.Context(), c, run, app.chat.Config.Connections[0])
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(chat.Request{System: chatSystem, Messages: msgs, Tools: app.runTools(run, app.chat.Config.Connections[0])})
	if !strings.Contains(string(raw), marker) {
		t.Fatalf("owner instructions absent from provider request: %s", raw)
	}
}
