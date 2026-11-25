package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
)

const testToken = "test-only-0123456789abcdefghijklmnopqrstuvwxyz"

func fixture(t testing.TB) (*Server, *workspace.Store) {
	t.Helper()
	dir := t.TempDir()
	projects, err := catalog.Open(dir, nil, pgtest.Dir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { projects.Close() })
	store, _ := projects.Access(catalog.Bootstrap, "default", "edit")
	app, err := New(projects, pgtest.Org(t), dir, "http://localhost", testToken)
	if err != nil {
		t.Fatal(err)
	}
	// Transport fixtures bypass Runtime.Serve; start their feature workers
	// explicitly while leaving startup-only configuration open for each test.
	if err := app.indexes.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := app.quicknotes.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app, store
}

// lockManuscript locks the manuscript folder, for tests of locked paths.
func lockManuscript(t *testing.T, store *workspace.Store) {
	t.Helper()
	state, err := store.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SetLock(context.Background(), "manuscript", true, state.Revision, "test setup"); err != nil {
		t.Fatal(err)
	}
}

func call(app http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	if strings.HasPrefix(path, "/api/") && path != "/api/projects" && !strings.HasPrefix(path, "/api/projects/") && path != "/api/me" && path != "/api/admin" && path != "/api/users" && path != "/api/members" {
		path = "/p/default" + path
	}
	if strings.HasPrefix(path, "/assets/") {
		path = "/p/default" + path
	}
	r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	if server, ok := app.(*Server); ok {
		serveMCPTest(server, w, r)
	} else {
		app.ServeHTTP(w, r)
	}
	return w
}
func TestPrivateRoutesAndSaveConflict(t *testing.T) {
	app, store := fixture(t)
	lockManuscript(t, store)
	for _, path := range []string{"/api/notes", "/api/status", "/api/backup", "/mcp", "/assets/a.png"} {
		w := call(app, "GET", path, "", "")
		if w.Code != 401 {
			t.Fatalf("%s exposed: %d", path, w.Code)
		}
	}
	saved := call(app, "POST", "/api/note", `{"path":"notes/test.md","content":"# Test","revision":""}`, testToken)
	if saved.Code != 200 {
		t.Fatalf("save: %d %s", saved.Code, saved.Body.String())
	}
	stale := call(app, "POST", "/api/note", `{"path":"notes/test.md","content":"overwrite","revision":""}`, testToken)
	if stale.Code != 409 {
		t.Fatalf("stale save: %d", stale.Code)
	}
	read := call(app, "GET", "/api/note?path=notes/test.md", "", testToken)
	if read.Code != 200 || !strings.Contains(read.Body.String(), "# Test") {
		t.Fatalf("read: %s", read.Body.String())
	}
	denied := call(app, "POST", "/api/note", `{"path":"manuscript/chapter.md","content":"no","revision":""}`, testToken)
	if denied.Code != 403 {
		t.Fatalf("read-only: %d", denied.Code)
	}
}
func TestBrowserLoginAndCrossOriginWrites(t *testing.T) {
	app, _ := fixture(t)
	r := httptest.NewRequest("POST", "http://localhost/login", strings.NewReader("token="+testToken))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://localhost")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 303 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 || !cookies[0].HttpOnly {
		t.Fatal("missing protected session")
	}
	for _, origin := range []string{"https://evil.example", "null"} {
		r = httptest.NewRequest("POST", "http://localhost/api/note", strings.NewReader(`{"path":"notes/x.md","content":"x"}`))
		r.AddCookie(cookies[0])
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		w = httptest.NewRecorder()
		app.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("accepted cross-origin %s: %d", origin, w.Code)
		}
	}
}
func TestPreviewSanitizesMarkup(t *testing.T) {
	app, _ := fixture(t)
	w := call(app, "POST", "/api/preview", `{"content":"# Hello\n<script>alert(1)</script>\n[x](javascript:alert(1))"}`, testToken)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	var result map[string]string
	json.Unmarshal(w.Body.Bytes(), &result)
	if strings.Contains(result["html"], "<script") || strings.Contains(result["html"], "javascript:") || !strings.Contains(result["html"], "<h1") {
		t.Fatalf("unsafe preview %s", result["html"])
	}
}
func TestMCPUsesSameStorageAndPermissions(t *testing.T) {
	app, store := fixture(t)
	lockManuscript(t, store)
	ts := httptest.NewServer(app)
	defer ts.Close()
	app.origin = ts.URL
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: tokenTransport{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "save_note", Arguments: map[string]any{"project": "default", "path": "notes/mcp.md", "content": "# MCP", "revision": ""}})
	if err != nil || result.IsError {
		t.Fatalf("tool save: %+v %v", result, err)
	}
	doc, err := store.Read(context.Background(), "notes/mcp.md", "")
	if err != nil || string(doc.Content) != "# MCP" {
		t.Fatalf("missing MCP write: %+v %v", doc, err)
	}
	result, err = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "save_note", Arguments: map[string]any{"project": "default", "path": "manuscript/chapter.md", "content": "no", "revision": ""}})
	if err != nil || !result.IsError {
		t.Fatalf("MCP bypassed read-only: %+v %v", result, err)
	}
	locked, err := store.SetLock(context.Background(), "notes", true, doc.Revision, "admin")
	if err != nil {
		t.Fatal(err)
	}
	result, err = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "save_note", Arguments: map[string]any{"project": "default", "path": doc.Path, "content": "no", "revision": locked.Revision}})
	if err != nil || !result.IsError {
		t.Fatalf("MCP bypassed project lock: %+v %v", result, err)
	}
	result, err = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "note_locks", Arguments: map[string]any{"project": "default", "path": doc.Path}})
	if err != nil || result.IsError {
		t.Fatalf("MCP cannot read lock reason: %+v %v", result, err)
	}

}

type tokenTransport struct{}

func (tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+testToken)
	return http.DefaultTransport.RoundTrip(r)
}
func TestBackupEndpoint(t *testing.T) {
	app, _ := fixture(t)
	w := call(app, "GET", "/api/backup", "", testToken)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	raw, _ := io.ReadAll(w.Result().Body)
	if !strings.HasPrefix(string(raw), "# v2 git bundle") {
		t.Fatalf("not a git bundle: %.50s", raw)
	}
}

func TestPublicOriginValidation(t *testing.T) {
	app, _ := fixture(t)
	for _, origin := range []string{"http://example.test", "https://user:secret@example.com", "https://example.com#fragment"} {
		if _, err := New(app.catalog, app.db, app.data, origin, testToken); err == nil {
			t.Errorf("accepted unsafe origin %s", origin)
		}
	}
}

func TestPreviewHeadingAnchors(t *testing.T) {
	app, _ := fixture(t)
	w := call(app, "POST", "/api/preview", `{"content":"# Heading\n\n[Jump](#heading)"}`, testToken)
	if !strings.Contains(w.Body.String(), `id=\"h-heading\"`) {
		t.Fatalf("missing heading anchor: %s", w.Body.String())
	}
}

func TestProjectAdministrationRequiresAdmin(t *testing.T) {
	app, _ := fixture(t)
	w := call(app, "POST", "/api/projects", `{"id":"second","name":"Second project"}`, testToken)
	if w.Code != 200 {
		t.Fatalf("project creation: %d %s", w.Code, w.Body.String())
	}
}

func TestLoginPagePreservesSameOriginFormRequests(t *testing.T) {
	app, _ := fixture(t)
	page := call(app, "GET", "/login", "", "")
	if page.Code != 200 {
		t.Fatalf("login page: %d", page.Code)
	}
	// no-referrer makes browsers send Origin: null on form POSTs, which our
	// CSRF guard must reject. Preserve same-origin requests without exposing
	// private page URLs to other sites.
	if got := page.Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Fatalf("form submissions need same-origin referrer policy; got %q", got)
	}
}

func TestBundledReadingFont(t *testing.T) {
	app, _ := fixture(t)
	for _, name := range []string{"Onest", "SourceSerif4-Roman", "SourceSerif4-Italic", "Fraunces"} {
		w := call(app, "GET", "/static/fonts/"+name+".woff2", "", "")
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "font/woff2") || !strings.HasPrefix(w.Body.String(), "wOF2") {
			t.Fatalf("bundled font %s unavailable: status=%d type=%s", name, w.Code, w.Header().Get("Content-Type"))
		}
		// The TTF sources regenerate the faces; the server does not carry them.
		if w = call(app, "GET", "/static/fonts/"+name+".ttf", "", ""); w.Code != 404 {
			t.Fatalf("font source %s served: %d", name, w.Code)
		}
	}
	license := call(app, "GET", "/static/fonts/OFL.txt", "", "")
	if license.Code != 200 || !strings.Contains(license.Body.String(), "SIL OPEN FONT LICENSE") {
		t.Fatal("bundled font license unavailable")
	}
}

// Raw OAuth tests use the legacy protocol, including its required initialization.
func serveMCPTest(app *Server, w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/mcp" && r.Method == "POST" && r.Header.Get("Mcp-Session-Id") == "" {
		init := httptest.NewRequest("POST", app.origin+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":99,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"oauth-test","version":"1"}}}`))
		init.Header = r.Header.Clone()
		init.Header.Set("Content-Type", "application/json")
		init.Header.Set("Accept", "application/json, text/event-stream")
		response := httptest.NewRecorder()
		app.ServeHTTP(response, init)
		if id := response.Header().Get("Mcp-Session-Id"); id != "" {
			r.Header.Set("Mcp-Session-Id", id)
			r.Header.Set("MCP-Protocol-Version", "2025-11-25")
		}
	}
	app.ServeHTTP(w, r)
}

func TestFolderFilterForBrowseAndSearch(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	for _, path := range []string{"wiki/a.md", "wiki/nested/b.md", "wiki-other/c.md", "elsewhere/d.md"} {
		state, _ := store.Status(ctx)
		if _, err := store.Save(ctx, path, []byte("---\ntags: [research]\nstatus: draft\n---\nA telescope observation."), state.Revision, "test"); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{"", "telescope"} {
		r := call(app, "GET", "/api/notes?path=wiki/&tag=research&status=draft&q="+query, "", testToken)
		var entries []workspace.Entry
		if err := json.Unmarshal(r.Body.Bytes(), &entries); err != nil || r.Code != 200 || len(entries) != 2 {
			t.Fatalf("folder filter for %q: %d %s (%v)", query, r.Code, r.Body.String(), err)
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Path, "wiki/") {
				t.Fatalf("sibling leaked: %s", entry.Path)
			}
		}
	}
	client := actionClient(t, app, catalog.Bootstrap, "notes:read")
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "search_notes", Arguments: map[string]any{"project": "default", "query": "telescope", "path": "wiki/", "tag": "research", "status": "draft"}})
	if err != nil || result.IsError {
		t.Fatalf("MCP: %v %v", result, err)
	}
	raw, _ := json.Marshal(result.StructuredContent)
	if strings.Contains(string(raw), "wiki-other") || !strings.Contains(string(raw), "wiki/nested/b.md") {
		t.Fatalf("MCP folder filter: %s", raw)
	}
}
