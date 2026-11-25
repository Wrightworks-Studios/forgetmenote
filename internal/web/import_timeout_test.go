package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// slowGitFetch puts a Git first on PATH whose fetch takes seconds longer, as
// on a slow or stalled disk.
func slowGitFetch(t *testing.T, seconds int) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shim := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = fetch ]; then sleep " + strconv.Itoa(seconds) + "; fi; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// bundleForm is the browser's import form for bundle.
func bundleForm(t *testing.T, id string, bundle []byte) (*bytes.Buffer, string) {
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
	return &body, form.FormDataContentType()
}

// An import that runs out of time is neither a server failure nor a refused
// bundle: the browser gets 503 with the code timeout, and an MCP client a
// typed error it may retry, each with the import's own message.
func TestTimedOutImportIsARetryableTimeout(t *testing.T) {
	app, source := fixture(t)
	path := filepath.Join(t.TempDir(), "source.bundle")
	if err := source.Backup(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	bundle, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "alice", CanCreateProjects: true}); err != nil {
		t.Fatal(err)
	}
	// A fetch that does not finish, so the import runs out of time in its
	// check.
	slowGitFetch(t, 60)

	// The browser, whose request ends before the stalled check does.
	body, contentType := bundleForm(t, "late", bundle)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	r := httptest.NewRequestWithContext(ctx, "POST", app.origin+"/api/projects/import", body)
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Origin", app.origin)
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	var refusal struct{ Code, Message string }
	json.Unmarshal(w.Body.Bytes(), &refusal)
	if w.Code != 503 || refusal.Code != "timeout" || !strings.Contains(refusal.Message, "does not show that the bundle is damaged") {
		t.Fatalf("a browser import that ran out of time: %d %s", w.Code, w.Body)
	}
	if _, err := app.catalog.Access(catalog.Bootstrap, "late", "read"); err == nil {
		t.Fatal("an import that ran out of time published its notebook")
	}

	// MCP. A tool call has no deadline of its request's, so this tool runs
	// the import as import_project_bundle does under a limit of its own.
	addManagementTool(app, app.revisions.server, "import_project_bundle", "Import a bundle within two seconds.", false, func(ctx context.Context, in struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Data string `json:"data"`
	}) (map[string]string, error) {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		upload := base64.NewDecoder(base64.StdEncoding, strings.NewReader(in.Data))
		return nil, app.catalog.Import(ctx, subject(ctx), in.ID, in.Name, upload)
	})
	client := actionClient(t, app, "alice", "notes:read notes:write notes:manage")
	result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "import_project_bundle", Arguments: map[string]any{"id": "late-mcp", "name": "Late", "data": base64.StdEncoding.EncodeToString(bundle)}})
	if err != nil {
		t.Fatal(err)
	}
	typed, _ := result.Meta[mcpErrorMeta].(map[string]any)
	if !result.IsError || typed["status"] != float64(503) || typed["code"] != "timeout" || typed["retryable"] != true || !strings.Contains(contractText(result), "does not show that the bundle is damaged") {
		raw, _ := json.Marshal(result)
		t.Fatalf("an MCP import that ran out of time: %s", raw)
	}
	if _, err := app.catalog.Access("alice", "late-mcp", "read"); err == nil {
		t.Fatal("an MCP import that ran out of time published its notebook")
	}
}

// The browser's import counts its time once the upload is stored: neither an
// upload slower than the server's read timeout nor an import that outlasts
// its write timeout, both counted from the request's start, cuts the import
// short or loses its answer.
func TestImportOutlastsTheServersRequestTimeouts(t *testing.T) {
	app, source := fixture(t)
	path := filepath.Join(t.TempDir(), "source.bundle")
	if err := source.Backup(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	bundle, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	slowGitFetch(t, 2)
	ts := httptest.NewUnstartedServer(app)
	ts.Config.ReadTimeout, ts.Config.WriteTimeout = time.Second, time.Second
	ts.Start()
	defer ts.Close()
	app.origin = ts.URL

	body, contentType := bundleForm(t, "slow", bundle)
	raw := body.Bytes()
	upload, send := io.Pipe()
	go func() {
		send.Write(raw[:len(raw)/2])
		time.Sleep(1500 * time.Millisecond)
		send.Write(raw[len(raw)/2:])
		send.Close()
	}()
	r, err := http.NewRequestWithContext(t.Context(), "POST", ts.URL+"/api/projects/import", upload)
	if err != nil {
		t.Fatal(err)
	}
	r.ContentLength = int64(len(raw))
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Origin", app.origin)
	r.Header.Set("Authorization", "Bearer "+testToken)
	response, err := ts.Client().Do(r)
	if err != nil {
		t.Fatal("the import's answer was lost:", err)
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !strings.Contains(string(answer), `"id":"slow"`) {
		t.Fatalf("a slow upload and import: %d %s", response.StatusCode, answer)
	}
	if _, err := app.catalog.Access(catalog.Bootstrap, "slow", "read"); err != nil {
		t.Fatal("the import was not published:", err)
	}
}
