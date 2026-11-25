package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"forgetmenote/internal/pg/pgtest"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
)

func importRequest(t *testing.T, app *Server, id string, bundle []byte, user, origin string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	form.WriteField("id", id)
	form.WriteField("name", "Imported notebook")
	file, err := form.CreateFormFile("bundle", "notes.bundle")
	if err != nil {
		t.Fatal(err)
	}
	file.Write(bundle)
	form.Close()
	r := httptest.NewRequest("POST", app.origin+"/api/projects/import", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r.Header.Set("Origin", origin)
	if user == catalog.Bootstrap {
		r.Header.Set("Authorization", "Bearer "+testToken)
	} else if user != "" {
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Remote-User", user)
		r.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	return w
}

func TestImportNotebookPreservesHistoryAndOwnership(t *testing.T) {
	app, source := fixture(t)
	ctx := context.Background()
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "alice", CanCreateProjects: true}); err != nil {
		t.Fatal(err)
	}
	first, err := source.Save(ctx, "notes/proof.md", []byte("# Original\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.Save(ctx, "notes/proof.md", []byte("# Restored\n"), first.Revision, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = source.Save(ctx, "assets/proof.png", []byte("synthetic image"), "", ""); err != nil {
		t.Fatal(err)
	}
	before, _ := source.Status(ctx)
	// A Git repository may carry branches and tags beside HEAD; a notebook
	// is the history of HEAD, so they are checked and left out.
	path := filepath.Join(t.TempDir(), "source.bundle")
	if err := source.Backup(ctx, path); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(t.TempDir(), "source")
	git(t, filepath.Dir(repository), "clone", "-q", path, repository)
	git(t, repository, "branch", "archived", first.Revision)
	git(t, repository, "tag", "before-import", first.Revision)
	path = filepath.Join(t.TempDir(), "refs.bundle")
	git(t, repository, "bundle", "create", path, "--all")
	bundle, _ := os.ReadFile(path)
	w := importRequest(t, app, "restored", bundle, "alice", app.origin)
	if w.Code != 200 {
		t.Fatalf("import: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.ID != "restored" {
		t.Fatalf("result: %s", w.Body.String())
	}
	restored, err := app.catalog.Access("alice", "restored", "owner")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := restored.Status(ctx)
	if after.Dirty || after.Revision != before.Revision {
		t.Fatalf("history changed: %#v %#v", before, after)
	}
	exported := filepath.Join(t.TempDir(), "restored.bundle")
	if err := restored.Backup(ctx, exported); err != nil {
		t.Fatal(err)
	}
	if heads := git(t, t.TempDir(), "bundle", "list-heads", exported); !strings.Contains(heads, before.Revision+" refs/heads/main") || strings.Contains(heads, "archived") || strings.Contains(heads, "before-import") {
		t.Fatalf("the restored notebook is not the history of HEAD alone:\n%s", heads)
	}
	for path, want := range map[string]string{"notes/proof.md": "# Restored\n", "assets/proof.png": "synthetic image"} {
		doc, err := restored.Read(ctx, path, "")
		if err != nil || string(doc.Content) != want {
			t.Fatalf("restore %s: %s %v", path, doc.Content, err)
		}
	}
	old, err := restored.Read(ctx, "notes/proof.md", first.Revision)
	if err != nil || string(old.Content) != "# Original\n" {
		t.Fatal("old revision lost", err)
	}
	if _, err := app.catalog.Access("outsider", "restored", "read"); err == nil {
		t.Fatal("import granted unrelated user access")
	}
	if w := importRequest(t, app, "restored", bundle, "alice", app.origin); w.Code != 409 {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body.String())
	}
	app.Close()
	app.catalog.Close()
	reopened, err := catalog.Open(app.data, nil, pgtest.Dir(t, app.data))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.Access("alice", "restored", "owner")
	if err != nil {
		t.Fatal(err)
	}
	status, _ := persisted.Status(ctx)
	if status.Revision != before.Revision {
		t.Fatal("import did not persist")
	}
}

func TestImportNotebookRejectsInvalidAndUnauthorizedUploads(t *testing.T) {
	app, source := fixture(t)
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "source.bundle")
	if err := source.Backup(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	bundle, _ := os.ReadFile(path)
	for _, tc := range []struct {
		name, id, user, origin string
		data                   []byte
		code                   int
	}{
		{"anonymous", "anon", "", app.origin, bundle, 401},
		{"unprivileged", "denied", "outsider", app.origin, bundle, 403},
		{"cross-origin", "cross", catalog.Bootstrap, "https://other.example", bundle, 403},
		{"invalid-id", "../escape", catalog.Bootstrap, app.origin, bundle, 400},
		{"invalid-bundle", "invalid", catalog.Bootstrap, app.origin, []byte("not a bundle"), 400},
		{"truncated-bundle", "truncated", catalog.Bootstrap, app.origin, bundle[:len(bundle)/2], 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := importRequest(t, app, tc.id, tc.data, tc.user, tc.origin)
			if w.Code != tc.code {
				t.Fatalf("got %d want %d: %s", w.Code, tc.code, w.Body.String())
			}
		})
	}
	if len(app.catalog.List(catalog.Bootstrap)) != 1 {
		t.Fatal("failed upload published a notebook")
	}
	entries, _ := os.ReadDir(filepath.Join(app.data, "projects"))
	if len(entries) != 1 || entries[0].Name() != "default" {
		t.Fatal("failed upload leaked project directories", entries)
	}
	// A bundle can record symlinks even though the app itself cannot create them.
	repository := filepath.Join(t.TempDir(), "unsafe")
	git(t, filepath.Dir(repository), "clone", "-q", path, repository)
	if err := os.Symlink("/etc/passwd", filepath.Join(repository, "escape.md")); err != nil {
		t.Fatal(err)
	}
	git(t, repository, "add", "escape.md")
	git(t, repository, "commit", "-q", "-m", "unsafe fixture")
	path = filepath.Join(t.TempDir(), "unsafe.bundle")
	git(t, repository, "bundle", "create", path, "--all")
	bundle, _ = os.ReadFile(path)
	w := importRequest(t, app, "unsafe", bundle, catalog.Bootstrap, app.origin)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "symlink") {
		t.Fatalf("unsafe tree: %d %s", w.Code, w.Body.String())
	}
}

func TestImportNotebookLimitsAndAdmission(t *testing.T) {
	app, _ := fixture(t)
	// An import through another gateway holds the instance's import lock.
	gateway, _ := pgtest.Gateway(t, app.db)
	release, ok, err := gateway.TryExclusive(t.Context(), "notebook-import")
	if err != nil || !ok {
		t.Fatal("hold the import lock", ok, err)
	}
	w := importRequest(t, app, "busy", []byte("not parsed"), catalog.Bootstrap, app.origin)
	release()
	if w.Code != 429 {
		t.Fatalf("admission: %d %s", w.Code, w.Body.String())
	}
	w = importRequest(t, app, "oversized", bytes.Repeat([]byte("x"), int(workspace.MaxBundleBytes())+1), catalog.Bootstrap, app.origin)
	if w.Code != 413 {
		t.Fatalf("size: %d %s", w.Code, w.Body.String())
	}
	if len(app.catalog.List(catalog.Bootstrap)) != 1 {
		t.Fatal("oversized upload created a notebook")
	}
}

func TestImportNotebookMCPRequiresCreationAndConsent(t *testing.T) {
	app, source := fixture(t)
	ctx := context.Background()
	first, err := source.Save(ctx, "notes/proof.md", []byte("# Original\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.Save(ctx, "notes/proof.md", []byte("# Restored\n"), first.Revision, ""); err != nil {
		t.Fatal(err)
	}
	before, _ := source.Status(ctx)
	path := filepath.Join(t.TempDir(), "fixture.bundle")
	if err := source.Backup(ctx, path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	args := map[string]any{"id": "mcp-import", "name": "MCP notebook", "data": base64.StdEncoding.EncodeToString(raw)}
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "alice", CanCreateProjects: true}); err != nil {
		t.Fatal(err)
	}
	client := actionClient(t, app, "alice", "notes:read notes:write notes:manage")
	actionCall(t, client, "import_project_bundle", args, false)
	imported, err := app.catalog.Access("alice", "mcp-import", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if after, err := imported.Status(ctx); err != nil || after.Revision != before.Revision {
		t.Fatalf("MCP import changed the history: %+v %v", after, err)
	}
	if old, err := imported.Read(ctx, "notes/proof.md", first.Revision); err != nil || string(old.Content) != "# Original\n" {
		t.Fatalf("MCP import lost an old revision: %v", err)
	}
	actionCall(t, client, "import_project_bundle", args, true)
	args["id"] = "no-write"
	actionCall(t, actionClient(t, app, "alice", "notes:read notes:manage"), "import_project_bundle", args, true)
	args["id"] = "no-manage"
	actionCall(t, actionClient(t, app, "alice", "notes:read notes:write"), "import_project_bundle", args, true)
	args["id"] = "no-create"
	actionCall(t, actionClient(t, app, "outsider", "notes:read notes:write notes:manage"), "import_project_bundle", args, true)
	if len(app.catalog.List(catalog.Bootstrap)) != 2 {
		t.Fatal("unauthorized import created notebook")
	}
}
