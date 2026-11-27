package web

import (
	"context"
	"encoding/json"
	"errors"
	"forgetmenote/internal/actions"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"testing"
)

func TestSharedActionDispatcherAuthorizationAndSchema(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	if _, e := store.Save(ctx, "notes/dispatch.md", []byte("shared evidence"), "", catalog.Bootstrap); e != nil {
		t.Fatal(e)
	}
	for _, u := range []string{"reader", "other"} {
		if e := app.catalog.Member(catalog.Bootstrap, "default", u, "read"); e != nil {
			t.Fatal(e)
		}
	}
	actor := context.WithValue(ctx, identityKey{}, "reader")
	raw, e := app.dispatchAction(actor, "default", "read_note", json.RawMessage(`{"path":"notes/dispatch.md"}`))
	if e != nil {
		t.Fatalf("authorized read failed: %v", e)
	}
	var got noteOutput
	if e = json.Unmarshal(raw, &got); e != nil || got.Content != "shared evidence" {
		t.Fatalf("read %s %v", raw, e)
	}
	for _, in := range []struct{ name, args string }{{"read_note", `{}`}, {"read_note", `{"path":"notes/dispatch.md","unsupported":true}`}, {"read_note", `{"project":"other","path":"notes/dispatch.md"}`}, {"read_note", `{"path":false}`}, {"read_note", `{"path":"notes/dispatch.md"} {}`}, {"save_note", `{"path":"notes/new.md","revision":"","content":"unauthorized"}`}, {"list_projects", `{}`}, {"absent_tool", `{}`}} {
		if _, e = app.dispatchAction(actor, "default", in.name, json.RawMessage(in.args)); e == nil {
			t.Errorf("accepted %s %s", in.name, in.args)
		}
	}
	client := actionClient(t, app, "reader", "notes:read")
	actionCall(t, client, "read_note", map[string]any{"project": "default", "path": "notes/dispatch.md"}, false)
	actionCall(t, client, "read_note", map[string]any{"project": "default", "path": "notes/dispatch.md", "unsupported": true}, true)
	if e = app.catalog.Member(catalog.Bootstrap, "default", "reader", ""); e != nil {
		t.Fatal(e)
	}
	if _, e = app.dispatchAction(actor, "default", "read_note", json.RawMessage(`{"path":"notes/dispatch.md"}`)); e == nil {
		t.Fatal("retained dispatch access after revoke")
	}
	actionCall(t, client, "read_note", map[string]any{"project": "default", "path": "notes/dispatch.md"}, true)
}

func TestRegisteredActionsHaveExplicitChatClassification(t *testing.T) {
	app, _ := fixture(t)
	for name, action := range app.actions {
		metadata, classified := actions.Policies()[name]
		if !classified {
			t.Errorf("registered action %q needs an explicit chat classification or exclusion", name)
		}
		if action.Policy != metadata {
			t.Errorf("registered action %q lost its policy", name)
		}
		if metadata.Chat && !metadata.ChatAvailable() {
			t.Errorf("chat action %q has no consent classification", name)
		}
	}
	for name := range actions.Policies() {
		if _, exists := app.actions[name]; !exists {
			t.Errorf("classification has no registered action: %s", name)
		}
	}
	// Broad MCP reads must not become a chat corpus bypass through discovery.
	for _, name := range []string{"get_indexing_passages", "list_trash", "read_attachment", "export_project_backup", "read_source_original", "verify_source", "list_attachments", "ai_settings", "set_ai_settings", "chat_clients", "set_chat_clients", "chat_client_directory", "provision_work_notebook"} {
		if _, exposed := app.actionTool(name); exposed {
			t.Errorf("unscoped action exposed: %s", name)
		}
	}
}

func TestUnclassifiedRegisteredActionCannotEnterChat(t *testing.T) {
	app, _ := fixture(t)
	called := false
	addAction(app, mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil), &mcp.Tool{Name: "future_action", Description: "Unclassified action"}, func(ctx context.Context, in projectArg) (actionResult, error) {
		called = true
		return actionResult{OK: true}, nil
	})
	if _, exposed := app.actionTool("future_action"); exposed {
		t.Fatal("unclassified action exposed")
	}
	for _, action := range app.actionCatalog() {
		if action["name"] == "future_action" {
			t.Fatal("unclassified action entered catalog")
		}
	}
	ctx := context.WithValue(t.Context(), identityKey{}, catalog.Bootstrap)
	ctx = chat.WithScope(ctx, &chat.RunScope{Run: chat.Run{Options: chat.Options{Corpus: &chat.Corpus{Version: 1, Archives: "exclude", Drafts: "exclude"}}}})
	_, err := app.dispatchAction(ctx, "default", "future_action", json.RawMessage(`{}`))
	if !errors.Is(err, chat.ErrOutsideCorpus) || called {
		t.Fatalf("unclassified action dispatched: called=%v err=%v", called, err)
	}
}
