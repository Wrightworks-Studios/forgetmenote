package web

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/client"
	"forgetmenote/internal/workspace"
)

func TestCLIEditRetainsTextWhenSaveConditionsChange(t *testing.T) {
	for _, change := range []string{"membership", "handoff", "live edits", "editor error"} {
		t.Run(change, func(t *testing.T) {
			app, store := fixture(t)
			ctx := t.Context()
			first, err := store.Save(ctx, "note.md", []byte("original"), "", "fixture")
			if err != nil {
				t.Fatal(err)
			}
			if err = app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
				t.Fatal(err)
			}
			grant, err := issueCredential(app.catalog, app.oauth, "alice", CredentialSpec{Name: "editor", Projects: []string{"default"}, Scopes: []string{"notes:read", "notes:write"}, Audiences: []string{"api"}, Days: 1})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(app)
			defer server.Close()
			app.origin = server.URL
			c := client.Client{Server: server.URL, Token: grant.Token}
			dir := t.TempDir()
			_, err = c.Edit(ctx, "default", "note.md", dir, func(path string) error {
				if err := os.WriteFile(path, []byte("my retained text"), 0600); err != nil {
					return err
				}
				switch change {
				case "membership":
					return app.catalog.Member(catalog.Bootstrap, "default", "alice", "")
				case "handoff":
					_, err := store.PrepareAuthority(ctx, workspace.AuthorityRequest{ID: "0123456789abcdef0123456789abcdef", Generation: 1, From: "forgetmenote", To: "github", Revision: first.Revision})
					return err
				case "live edits":
					live, err := store.LiveRead(ctx, "note.md")
					if err != nil {
						return err
					}
					_, err = store.LivePatch(ctx, workspace.TargetedPatch{Path: "note.md", Base: live.LiveRevision, OperationID: "editor-concurrent-live", Edits: []workspace.TextEdit{{Start: 0, End: 8, Old: "original", Text: "shared pending text"}}})
					return err
				default:
					return errors.New("editor exited unsuccessfully")
				}
			})
			var draft *client.DraftError
			if !errors.As(err, &draft) {
				t.Fatal("save did not retain a recoverable edit", err)
			}
			text, err := os.ReadFile(draft.Path)
			if err != nil || string(text) != "my retained text" {
				t.Fatal("local text was lost", err)
			}
			current, err := store.Read(ctx, "note.md", "")
			if err != nil || current.Revision != first.Revision || string(current.Content) != "original" {
				t.Fatal("rejected edit changed committed content", err)
			}
		})
	}
}

func TestCLIEditRefusesAlreadyPendingLiveWork(t *testing.T) {
	app, store := fixture(t)
	ctx := t.Context()
	if _, err := store.Save(ctx, "note.md", []byte("original"), "", "fixture"); err != nil {
		t.Fatal(err)
	}
	live, err := store.LiveRead(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.LivePatch(ctx, workspace.TargetedPatch{Path: "note.md", Base: live.LiveRevision, OperationID: "editor-existing-live", Edits: []workspace.TextEdit{{Start: 0, End: 8, Old: "original", Text: "shared pending text"}}}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app)
	defer server.Close()
	app.origin = server.URL
	c := client.Client{Server: server.URL, Token: testToken}
	_, err = c.Edit(ctx, "default", "note.md", t.TempDir(), func(string) error { t.Fatal("editor opened stale committed content"); return nil })
	var apiErr *client.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "live_pending" {
		t.Fatal("pending collaboration was not explained", err)
	}
}

func TestCLICommandsUseCentralNotebook(t *testing.T) {
	app, store := fixture(t)
	server := httptest.NewServer(app)
	defer server.Close()
	app.origin = server.URL
	t.Setenv("FORGETMENOTE_SERVER", server.URL)
	t.Setenv("FORGETMENOTE_TOKEN", testToken)
	t.Setenv("FORGETMENOTE_CONFIG_DIR", t.TempDir())
	var output, diagnostics bytes.Buffer
	err := client.Run(context.Background(), []string{"notebook", "list", "--json"}, strings.NewReader(""), &output, &diagnostics)
	if err != nil || !strings.Contains(output.String(), `"authority"`) {
		t.Fatalf("CLI list: %s %v", output.String(), err)
	}
	output.Reset()
	err = client.Run(context.Background(), []string{"note", "write", "default", "notes/CLI name.md", "--create", "--file", "-", "--json"}, strings.NewReader("# Local file"), &output, &diagnostics)
	if err != nil {
		t.Fatalf("CLI create: %v", err)
	}
	doc, err := store.Read(context.Background(), "notes/CLI name.md", "")
	if err != nil || !strings.Contains(string(doc.Content), "# Local file") {
		t.Fatalf("CLI content: %s %v", doc.Content, err)
	}
	err = client.Run(context.Background(), []string{"note", "write", "default", "notes/CLI name.md", "--file", "-"}, strings.NewReader("unsafe"), &output, &diagnostics)
	if err == nil {
		t.Fatal("unconditional update accepted")
	}
}

func TestCLIEditNoopOtherNoteAndIndependentExport(t *testing.T) {
	app, store := fixture(t)
	server := httptest.NewServer(app)
	defer server.Close()
	app.origin = server.URL
	c := client.Client{Server: server.URL, Token: testToken}
	ctx := t.Context()
	first, err := store.Save(ctx, "note.md", []byte("original"), "", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	result, err := c.Edit(ctx, "default", "note.md", dir, func(string) error { return nil })
	if err != nil || !result.Unchanged || result.Revision != first.Revision {
		t.Fatal("no-op committed", result, err)
	}
	result, err = c.Edit(ctx, "default", "note.md", dir, func(p string) error {
		if _, e := store.Save(ctx, "other.md", []byte("other client"), "", "fixture"); e != nil {
			return e
		}
		return os.WriteFile(p, []byte("edited centrally"), 0600)
	})
	if err != nil || result.Revision == first.Revision {
		t.Fatal("unrelated note blocked save", result, err)
	}
	bundle := filepath.Join(dir, "notebook.bundle")
	if err = c.Export(ctx, "default", bundle); err != nil {
		t.Fatal(err)
	}
	if err = c.Export(ctx, "default", bundle); err == nil {
		t.Fatal("export overwrote an existing backup")
	}
	restored := filepath.Join(dir, "restored")
	if raw, e := exec.Command("git", "clone", bundle, restored).CombinedOutput(); e != nil {
		t.Fatal(e, string(raw))
	}
	b, e := os.ReadFile(filepath.Join(restored, "note.md"))
	if e != nil || !strings.Contains(string(b), "edited centrally") {
		t.Fatal("export restore", e)
	}
	if raw, e := exec.Command("git", "-C", restored, "show", first.Revision+":note.md").Output(); e != nil || string(raw) != "original" {
		t.Fatal("export lost history", e)
	}
}

func TestCLIEditCommitsAndPreservesConflictingDraft(t *testing.T) {
	app, store := fixture(t)
	server := httptest.NewServer(app)
	defer server.Close()
	app.origin = server.URL
	c := client.Client{Server: server.URL, Token: testToken}
	ctx := context.Background()
	var initial noteOutput
	if err := c.Call(ctx, "save_note", noteInput{Project: "default", Path: "notes/editor.md", Content: "original"}, &initial); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	result, err := c.Edit(ctx, "default", "notes/editor.md", dir, func(path string) error { return os.WriteFile(path, []byte("accepted from editor"), 0600) })
	if err != nil || result.Revision == initial.Revision {
		t.Fatalf("edit result: %+v %v", result, err)
	}
	doc, err := store.Read(ctx, "notes/editor.md", "")
	if err != nil || !strings.Contains(string(doc.Content), "accepted from editor") {
		t.Fatalf("missing accepted edit: %s %v", doc.Content, err)
	}
	_, err = c.Edit(ctx, "default", "notes/editor.md", dir, func(path string) error {
		if e := os.WriteFile(path, []byte("my unsaved draft"), 0600); e != nil {
			return e
		}
		return c.Call(ctx, "save_note", noteInput{Project: "default", Path: "notes/editor.md", Content: "other client won", Revision: doc.Revision}, new(noteOutput))
	})
	var draft *client.DraftError
	if !errors.As(err, &draft) {
		t.Fatalf("missing recoverable conflict: %v", err)
	}
	text, e := os.ReadFile(draft.Path)
	if e != nil || string(text) != "my unsaved draft" {
		t.Fatalf("draft lost: %s %v", text, e)
	}
	if filepath.Dir(draft.Path) == dir {
		t.Fatal("draft is not isolated in an edit session")
	}
	current, _ := store.Read(ctx, "notes/editor.md", "")
	if !strings.Contains(string(current.Content), "other client won") {
		t.Fatalf("conflicting edit overwritten: %s", current.Content)
	}
}

func TestCLIEditReconcilesLostResponseWithoutRepeatingSave(t *testing.T) {
	app, store := fixture(t)
	ctx := t.Context()
	if _, err := store.Save(ctx, "note.md", []byte("original"), "", "fixture"); err != nil {
		t.Fatal(err)
	}
	saves := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/actions/save_note" {
			saves++
			record := httptest.NewRecorder()
			app.ServeHTTP(record, r)
			if record.Code != 200 {
				t.Error(record.Code, record.Body.String())
			}
			w.Write([]byte(`{"revision":`))
			return
		}
		app.ServeHTTP(w, r)
	}))
	defer server.Close()
	app.origin = server.URL
	c := client.Client{Server: server.URL, Token: testToken}
	dir := t.TempDir()
	result, err := c.Edit(ctx, "default", "note.md", dir, func(p string) error { return os.WriteFile(p, []byte("accepted despite lost response"), 0600) })
	if err != nil || result.Revision == "" || saves != 1 {
		t.Fatalf("ambiguous save not reconciled: %+v %v saves=%d", result, err, saves)
	}
	drafts, err := client.ListEdits(dir)
	if err != nil || len(drafts) != 0 {
		t.Fatal("accepted edit left an unresolved draft", err)
	}
}
