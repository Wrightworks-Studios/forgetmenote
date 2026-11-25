package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"forgetmenote/internal/capacity"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
)

// outcome is one call through one transport: the HTTP status, or for MCP 200
// on success and otherwise the status its typed error carries (0 if untyped).
type outcome struct {
	status int
	code   string
	body   json.RawMessage
}

// transports calls the same operation through the browser API and MCP as
// one user: "" is the bootstrap Admin (testToken), any other user signs in to
// the browser through the trusted proxy. Every caller of one test shares one
// server, so the instance keeps a single origin.
type transports struct {
	t    *testing.T
	app  *Server
	user string
	mcp  *mcp.ClientSession
}

func transportServer(t *testing.T, app *Server) *httptest.Server {
	t.Helper()
	if err := app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app)
	t.Cleanup(ts.Close)
	app.origin = ts.URL
	return ts
}

func newTransports(t *testing.T, app *Server, ts *httptest.Server, user string) transports {
	t.Helper()
	actor := user
	if user == "" {
		actor = catalog.Bootstrap
	}
	token := "transports-" + digest(actor)
	app.oauth.Mu.Lock()
	app.oauth.Access[digest(token)] = withOAuthFamily(accessGrant{Subject: actor, Client: "transports-client", Scope: "notes:read notes:write notes:manage", Resource: ts.URL + "/mcp", Expires: time.Now().Add(time.Hour).Unix(), Epoch: app.catalog.Epoch(actor)})
	app.oauth.Mu.Unlock()
	client := mcp.NewClient(&mcp.Implementation{Name: "transports", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return transports{t, app, user, session}
}

func (c transports) http(method, path string, body any) outcome {
	c.t.Helper()
	raw := ""
	if body != nil {
		raw = string(mustJSON(c.t, body))
	}
	var w *httptest.ResponseRecorder
	if c.user == "" {
		w = apiCall(c.app, method, "/p/default"+path, raw, testToken)
	} else {
		w = proxyCall(c.app, c.user, method, "/p/default"+path, raw)
	}
	result := outcome{status: w.Code, body: w.Body.Bytes()}
	if w.Code >= 400 {
		var detail struct {
			Code string `json:"code"`
		}
		json.Unmarshal(result.body, &detail)
		result.code = detail.Code
	}
	return result
}

func (c transports) tool(name string, args map[string]any) outcome {
	c.t.Helper()
	if _, ok := args["project"]; !ok {
		args["project"] = "default"
	}
	result, err := c.mcp.CallTool(c.t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		c.t.Fatal(name, err)
	}
	if !result.IsError {
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			c.t.Fatal(err)
		}
		return outcome{status: 200, body: raw}
	}
	failed := outcome{body: json.RawMessage(contractText(result))}
	if typed, ok := result.Meta[mcpErrorMeta].(map[string]any); ok {
		status, _ := typed["status"].(float64)
		failed.status = int(status)
		failed.code, _ = typed["code"].(string)
	}
	return failed
}

// same requires both transports to end with the same status and error code
// and, on success, the same result after the optional MCP projection of the
// browser's result. An unclassified error is a browser 500 and an untyped MCP
// error; then the messages must match.
func same[T any](t *testing.T, label string, browser, tool outcome, project func(T) any) {
	t.Helper()
	if browser.status == 500 && browser.code == "internal_error" && tool.status == 0 {
		var detail struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(browser.body, &detail) != nil || detail.Message != string(tool.body) {
			t.Fatalf("%s: unclassified errors differ: browser %s; MCP %s", label, browser.body, tool.body)
		}
		return
	}
	if browser.status != tool.status || browser.code != tool.code {
		t.Fatalf("%s: browser %d %s %s; MCP %d %s %s", label, browser.status, browser.code, browser.body, tool.status, tool.code, tool.body)
	}
	if browser.status != 200 {
		return
	}
	var fromBrowser, fromTool T
	if err := json.Unmarshal(browser.body, &fromBrowser); err != nil {
		t.Fatalf("%s: browser result %s: %v", label, browser.body, err)
	}
	var want, got any = fromBrowser, nil
	if project != nil {
		want = project(fromBrowser)
		raw, _ := json.Marshal(want)
		var projected any
		json.Unmarshal(raw, &projected)
		want = projected
		json.Unmarshal(tool.body, &got)
	} else {
		if err := json.Unmarshal(tool.body, &fromTool); err != nil {
			t.Fatalf("%s: MCP result %s: %v", label, tool.body, err)
		}
		got = fromTool
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("%s: results differ\nbrowser: %s\nMCP:     %s", label, browser.body, tool.body)
	}
}

func expect(t *testing.T, label string, got outcome, status int, code string) {
	t.Helper()
	if got.status != status || got.code != code {
		t.Fatalf("%s: got %d %q %s, want %d %q", label, got.status, got.code, got.body, status, code)
	}
}

func decoded[T any](t *testing.T, got outcome) T {
	t.Helper()
	var value T
	if got.status != 200 {
		t.Fatalf("call failed: %d %s", got.status, got.body)
	}
	if err := json.Unmarshal(got.body, &value); err != nil {
		t.Fatalf("%s: %v", got.body, err)
	}
	return value
}

func members(t *testing.T, app *Server, roles map[string]string) {
	t.Helper()
	for user, role := range roles {
		if err := app.catalog.Member(catalog.Bootstrap, "default", user, role); err != nil {
			t.Fatal(err)
		}
	}
}

func headRevision(t *testing.T, store *workspace.Store) string {
	t.Helper()
	state, err := store.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return state.Revision
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func webcoreStatus(err error) int {
	if err == nil {
		return 0
	}
	return webcore.ErrorOf(err, 500).Status
}

// Capacity refusals are 429 over HTTP and a typed, retryable error over MCP.
func TestCapacityRefusalIsTypedOnEveryTransport(t *testing.T) {
	app, _ := fixture(t)
	ts := transportServer(t, app)
	admin := newTransports(t, app, ts, "")
	notebookJSON(app, "GET /api/test-capacity", "read", func(context.Context, *workspace.Store, projectArg) (any, error) {
		return nil, capacity.ErrBusy
	})
	expect(t, "a browser capacity refusal", admin.http("GET", "/api/test-capacity", nil), 429, "capacity")

	for i := 0; i < cap(app.rpcReadWork); i++ {
		app.rpcReadWork <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(app.rpcReadWork); i++ {
			<-app.rpcReadWork
		}
	}()
	if w := apiCall(app, "POST", "/api/v1/actions/list_trash", `{"project":"default"}`, testToken); w.Code != 429 {
		t.Fatalf("action API capacity: %d %s", w.Code, w.Body.String())
	}
	result, err := admin.mcp.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_trash", Arguments: map[string]any{"project": "default"}})
	if err != nil {
		t.Fatal(err)
	}
	typed, _ := result.Meta[mcpErrorMeta].(map[string]any)
	if !result.IsError || typed["status"] != float64(429) || typed["code"] != "capacity" || typed["retryable"] != true || contractText(result) != capacity.ErrBusy.Error() {
		raw, _ := json.Marshal(result)
		t.Fatalf("MCP capacity refusal: %s", raw)
	}
}

func TestDecodeQueryUsesJSONFieldNames(t *testing.T) {
	var in struct {
		projectArg
		reposource.LineRange
		Name   string `json:"name,omitempty"`
		Count  int    `json:"count"`
		Big    uint64 `json:"big"`
		On     bool   `json:"on"`
		Hidden string `json:"-"`
	}
	values := url.Values{"project": {"p"}, "startLine": {"3"}, "cursor": {"c"}, "name": {"n"}, "count": {"-2"}, "big": {"7"}, "on": {"true"}, "-": {"x"}, "unknown": {"ignored"}}
	if err := decodeQuery(values, &in); err != nil {
		t.Fatal(err)
	}
	if in.Project != "p" || in.StartLine != 3 || in.Cursor != "c" || in.Name != "n" || in.Count != -2 || in.Big != 7 || !in.On || in.Hidden != "" {
		t.Fatalf("decoded %+v", in)
	}
	for key, value := range map[string]string{"count": "two", "big": "-1", "on": "maybe"} {
		err := decodeQuery(url.Values{key: {value}}, &in)
		if status := webcoreStatus(err); status != 400 {
			t.Errorf("%s=%s: %v (%d)", key, value, err, status)
		}
	}
	var nested struct {
		Query knowledge.HybridQuery `json:"query"`
	}
	if err := decodeQuery(url.Values{"query": {"x"}}, &nested); webcoreStatus(err) != 400 {
		t.Fatalf("an object in the URL: %v", err)
	}
}

func TestTrashOperationsAgreeAcrossTransports(t *testing.T) {
	app, store := fixture(t)
	ts := transportServer(t, app)
	members(t, app, map[string]string{"editor": "edit"})
	admin, editor := newTransports(t, app, ts, ""), newTransports(t, app, ts, "editor")
	for _, path := range []string{"first.md", "second.md"} {
		if _, err := store.Save(t.Context(), path, []byte("# "+path+"\n"), "", "writer"); err != nil {
			t.Fatal(err)
		}
	}
	same[workspace.TrashListing](t, "empty listing", admin.http("GET", "/api/trash", nil), admin.tool("list_trash", map[string]any{}), nil)

	stale := headRevision(t, store)
	same[any](t, "an editor cannot trash", editor.http("POST", "/api/trash", map[string]any{"paths": []string{"first.md"}, "revision": stale}), editor.tool("trash_notes", map[string]any{"paths": []string{"first.md"}, "revision": stale}), nil)
	expect(t, "trash through the browser", admin.http("POST", "/api/trash", map[string]any{"paths": []string{"first.md"}, "revision": stale}), 200, "")
	same[any](t, "a stale trash revision", admin.http("POST", "/api/trash", map[string]any{"paths": []string{"second.md"}, "revision": stale}), admin.tool("trash_notes", map[string]any{"paths": []string{"second.md"}, "revision": stale}), nil)
	expect(t, "trash through MCP", admin.tool("trash_notes", map[string]any{"paths": []string{"second.md"}, "revision": headRevision(t, store)}), 200, "")

	listing := decoded[workspace.TrashListing](t, admin.tool("list_trash", map[string]any{}))
	same[workspace.TrashListing](t, "listing", admin.http("GET", "/api/trash", nil), admin.tool("list_trash", map[string]any{}), nil)
	if len(listing.Entries) != 2 {
		t.Fatalf("trash listing: %+v", listing)
	}
	same[any](t, "a wrong emptying confirmation", admin.http("POST", "/api/trash/empty", map[string]any{"revision": listing.Revision, "confirmation": "yes"}), admin.tool("empty_trash", map[string]any{"revision": listing.Revision, "confirmation": "yes"}), nil)
	same[any](t, "an unknown Trash item", admin.http("POST", "/api/trash/restore", map[string]any{"id": "missing", "revision": listing.Revision}), admin.tool("restore_trash", map[string]any{"id": "missing", "revision": listing.Revision}), nil)
	expect(t, "restore through the browser", admin.http("POST", "/api/trash/restore", map[string]any{"id": listing.Entries[0].ID, "revision": listing.Revision}), 200, "")
	expect(t, "restore through MCP", admin.tool("restore_trash", map[string]any{"id": listing.Entries[1].ID, "revision": headRevision(t, store)}), 200, "")
	for _, path := range []string{"first.md", "second.md"} {
		if _, err := store.Read(t.Context(), path, ""); err != nil {
			t.Fatalf("%s not restored: %v", path, err)
		}
	}

	// A browser body naming another notebook is refused, not redirected.
	expect(t, "another notebook in the body", admin.http("POST", "/api/trash", map[string]any{"project": "other", "paths": []string{"first.md"}, "revision": headRevision(t, store)}), 400, "invalid_request")
}

// Route selection authorizes before the body arrives; the operation runs only
// if the caller still has the route's role once the body has been read.
func TestBrowserOperationsRecheckAccessAfterReadingBody(t *testing.T) {
	app, store := fixture(t)
	transportServer(t, app)
	members(t, app, map[string]string{"owner": "owner"})
	note, err := store.Save(t.Context(), "kept.md", []byte("# Kept\n"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	body := &commentDeleteRequestBody{Reader: strings.NewReader(string(mustJSON(t, map[string]any{"paths": []string{note.Path}, "revision": note.Revision}))), revoke: func() {
		if err := app.catalog.Member(catalog.Bootstrap, "default", "owner", "read"); err != nil {
			t.Fatal(err)
		}
	}}
	request := httptest.NewRequest("POST", app.origin+"/p/default/api/trash", body)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Remote-User", "owner")
	request.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
	request.Header.Set("Origin", app.origin)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatalf("owner role revoked while the body was read: %d %s", response.Code, response.Body)
	}
	if _, err := store.Read(t.Context(), note.Path, ""); err != nil {
		t.Fatalf("revoked request trashed the note: %v", err)
	}
}

func TestCommentOperationsAgreeAcrossTransports(t *testing.T) {
	app, store := fixture(t)
	ts := transportServer(t, app)
	members(t, app, map[string]string{"reader": "read", "commenter": "comment"})
	admin, reader, commenter := newTransports(t, app, ts, ""), newTransports(t, app, ts, "reader"), newTransports(t, app, ts, "commenter")
	if _, err := store.Save(t.Context(), "discussed.md", []byte("# Discussed\n\nA passage to discuss.\n"), "", "writer"); err != nil {
		t.Fatal(err)
	}
	type discussion struct {
		Revision string `json:"revision"`
		Comments []struct {
			ID       string `json:"id"`
			Resolved bool   `json:"resolved"`
		} `json:"comments"`
	}
	list := func(c transports) discussion {
		t.Helper()
		same[any](t, "list comments", c.http("GET", "/api/comments?path=discussed.md", nil), c.tool("list_comments", map[string]any{"path": "discussed.md"}), nil)
		return decoded[discussion](t, c.tool("list_comments", map[string]any{"path": "discussed.md"}))
	}
	start := list(reader)
	add := map[string]any{"path": "discussed.md", "text": "Reader remark", "revision": start.Revision}
	same[any](t, "a reader cannot comment", reader.http("POST", "/api/comments", add), reader.tool("add_comment", add), nil)

	expect(t, "comment through the browser", commenter.http("POST", "/api/comments", map[string]any{"path": "discussed.md", "text": "From the browser", "revision": start.Revision}), 200, "")
	stale := map[string]any{"path": "discussed.md", "text": "Late", "revision": start.Revision}
	same[any](t, "a stale discussion revision", commenter.http("POST", "/api/comments", stale), commenter.tool("add_comment", stale), nil)
	current := list(commenter)
	expect(t, "comment through MCP", commenter.tool("add_comment", map[string]any{"path": "discussed.md", "text": "From MCP", "revision": current.Revision}), 200, "")
	current = list(admin)
	if len(current.Comments) != 2 {
		t.Fatalf("comments: %+v", current)
	}

	// Resolving and deleting someone else's comment takes moderation authority
	// on both transports.
	theirs := map[string]any{"path": "discussed.md", "id": current.Comments[1].ID, "revision": current.Revision}
	same[any](t, "a reader cannot resolve", reader.http("POST", "/api/comments/resolve", theirs), reader.tool("resolve_comment", theirs), nil)
	expect(t, "resolve through the browser", admin.http("POST", "/api/comments/resolve", theirs), 200, "")
	current = list(admin)
	expect(t, "resolve through MCP", admin.tool("resolve_comment", map[string]any{"path": "discussed.md", "id": current.Comments[0].ID, "revision": current.Revision}), 200, "")
	current = list(admin)
	if !current.Comments[0].Resolved || !current.Comments[1].Resolved {
		t.Fatalf("resolved comments: %+v", current)
	}
	missing := map[string]any{"path": "discussed.md", "id": "missing", "revision": current.Revision}
	same[any](t, "delete a missing comment", admin.http("POST", "/api/comments/delete", missing), admin.tool("delete_comment", missing), nil)
	expect(t, "delete through the browser", admin.http("POST", "/api/comments/delete", map[string]any{"path": "discussed.md", "id": current.Comments[0].ID, "revision": current.Revision}), 200, "")
	current = list(admin)
	expect(t, "delete through MCP", admin.tool("delete_comment", map[string]any{"path": "discussed.md", "id": current.Comments[0].ID, "revision": current.Revision}), 200, "")
	if current = list(admin); len(current.Comments) != 0 {
		t.Fatalf("deleted comments remain: %+v", current)
	}
}

func TestLiveOperationsAgreeAcrossTransports(t *testing.T) {
	app, store := fixture(t)
	ts := transportServer(t, app)
	members(t, app, map[string]string{"reader": "read"})
	admin, reader := newTransports(t, app, ts, ""), newTransports(t, app, ts, "reader")
	if _, err := store.Save(t.Context(), "live.md", []byte("alpha beta\n"), "", "writer"); err != nil {
		t.Fatal(err)
	}
	asMCP := func(v workspace.LiveDocument) any { return readableLive(v) }
	read := func() workspace.LiveDocument {
		t.Helper()
		same(t, "read live", admin.http("GET", "/api/live?path=live.md", nil), admin.tool("read_live_note", map[string]any{"path": "live.md"}), asMCP)
		return decoded[workspace.LiveDocument](t, admin.http("GET", "/api/live?path=live.md", nil))
	}
	base := read()
	same[any](t, "read a missing live note", admin.http("GET", "/api/live?path=missing.md", nil), admin.tool("read_live_note", map[string]any{"path": "missing.md"}), nil)
	// An out-of-range or malformed wait is invalid input, not a server error.
	expect(t, "an out-of-range live wait", admin.http("GET", "/api/live?path=live.md&wait=25001", nil), 400, "invalid_request")
	expect(t, "a malformed live wait", admin.http("GET", "/api/live?path=live.md&wait=soon", nil), 400, "invalid_request")

	patch := func(id, old, text string, start int) map[string]any {
		return map[string]any{"path": "live.md", "base": base.LiveRevision, "operationId": id, "edits": []map[string]any{{"start": start, "end": start + len(old), "old": old, "text": text}}}
	}
	same[any](t, "a reader cannot patch", reader.http("POST", "/api/live/patch", patch("reader", "alpha", "x", 0)), reader.tool("patch_live_note", patch("reader", "alpha", "x", 0)), nil)
	same[any](t, "mismatched old text", admin.http("POST", "/api/live/patch", patch("wrong", "zzzzz", "x", 0)), admin.tool("patch_live_note", patch("wrong", "zzzzz", "x", 0)), nil)
	browser := decoded[workspace.LiveDocument](t, admin.http("POST", "/api/live/patch", patch("browser", "alpha", "ALPHA", 0)))
	viaMCP := decoded[liveNoteOutput](t, admin.tool("patch_live_note", patch("mcp", "beta", "BETA", 6)))
	if !browser.Pending || viaMCP.Content != "ALPHA BETA\n" || !viaMCP.Pending {
		t.Fatalf("patches: browser %+v, MCP %+v", browser, viaMCP)
	}
	read()
	same[any](t, "a reader cannot checkpoint", reader.http("POST", "/api/live/checkpoint", map[string]any{"path": "live.md"}), reader.tool("checkpoint_live_note", map[string]any{"path": "live.md"}), nil)
	checkpoint := decoded[workspace.LiveDocument](t, admin.http("POST", "/api/live/checkpoint", map[string]any{"path": "live.md"}))
	// An unchanged retry through MCP returns the same checkpoint.
	same(t, "checkpoint", outcome{status: 200, body: mustJSON(t, checkpoint)}, admin.tool("checkpoint_live_note", map[string]any{"path": "live.md"}), asMCP)
	if checkpoint.Pending {
		t.Fatalf("checkpoint left edits pending: %+v", checkpoint)
	}
}

func TestRepositoryOperationsAgreeAcrossTransports(t *testing.T) {
	app, _ := fixture(t)
	ts := transportServer(t, app)
	members(t, app, map[string]string{"reader": "read"})
	admin, reader := newTransports(t, app, ts, ""), newTransports(t, app, ts, "reader")

	// Unconfigured sources answer the same way on both transports.
	same[any](t, "unconfigured status", admin.http("GET", "/api/repository", nil), admin.tool("repository_source_status", map[string]any{}), nil)
	same[any](t, "unconfigured tree", admin.http("GET", "/api/repository/tree", nil), admin.tool("repository_source_tree", map[string]any{}), nil)
	expect(t, "unconfigured tree", admin.http("GET", "/api/repository/tree", nil), 503, "unavailable")

	configureSearchSource(t, app)
	status := decoded[repositoryStatus](t, admin.tool("repository_source_status", map[string]any{}))
	if !status.Configured || !status.Connected || status.Downloaded == "" || status.InstallURL == "" {
		t.Fatalf("repository status: %+v", status)
	}
	same[repositoryStatus](t, "status", admin.http("GET", "/api/repository", nil), admin.tool("repository_source_status", map[string]any{}), nil)
	same[[]reposource.Entry](t, "tree", admin.http("GET", "/api/repository/tree", nil), admin.tool("repository_source_tree", map[string]any{}), nil)
	same[reposource.MapResult](t, "map", admin.http("GET", "/api/repository/map?query=Health&limit=5", nil), admin.tool("repository_source_map", map[string]any{"query": "Health", "limit": 5}), nil)
	expect(t, "a malformed map limit", admin.http("GET", "/api/repository/map?limit=many", nil), 400, "invalid_request")
	revision := status.Downloaded
	same[reposource.File](t, "file", admin.http("GET", "/api/repository/file?path=health.go&revision="+revision, nil), admin.tool("repository_source_file", map[string]any{"path": "health.go", "revision": revision}), nil)
	same[reposource.File](t, "file range", admin.http("GET", "/api/repository/file?path=health.go&startLine=1&endLine=2&revision="+revision, nil), admin.tool("repository_source_file", map[string]any{"path": "health.go", "revision": revision, "startLine": 1, "endLine": 2}), nil)
	// A partial or oversized line range is invalid input on both transports.
	for _, bounds := range []map[string]any{{"startLine": 2}, {"cursor": "next"}, {"startLine": 1, "endLine": 500}} {
		query := url.Values{"path": {"health.go"}, "revision": {revision}}
		args := map[string]any{"path": "health.go", "revision": revision}
		for key, value := range bounds {
			query.Set(key, fmt.Sprint(value))
			args[key] = value
		}
		browser := admin.http("GET", "/api/repository/file?"+query.Encode(), nil)
		expect(t, fmt.Sprintf("line range %v", bounds), browser, 400, "invalid_request")
		same[any](t, fmt.Sprintf("line range %v", bounds), browser, admin.tool("repository_source_file", args), nil)
	}
	same[reposource.Changes](t, "changes", admin.http("GET", "/api/repository/changes?from="+revision, nil), admin.tool("repository_source_changes", map[string]any{"from": revision}), nil)
	capture := map[string]any{"source": status.ID, "revision": revision, "path": "health.go"}
	same[any](t, "a reader cannot retain a citation", reader.http("POST", "/api/repository/capture", capture), reader.tool("retain_repository_citation", capture), nil)
	same[[]reposource.File](t, "capture", admin.http("POST", "/api/repository/capture", capture), admin.tool("retain_repository_citation", capture), nil)
	index := map[string]any{"generation": status.Generation + 7, "config": status.IndexConfig}
	same[any](t, "a reader cannot configure the index", reader.http("POST", "/api/repository/index-config", index), reader.tool("configure_repository_index", index), nil)
	same[any](t, "a stale index generation", admin.http("POST", "/api/repository/index-config", index), admin.tool("configure_repository_index", index), nil)
	same[any](t, "a reader cannot refresh", reader.http("POST", "/api/repository/refresh", map[string]any{}), reader.tool("refresh_repository_source", map[string]any{}), nil)
}

func TestKnowledgeOperationsAgreeAcrossTransports(t *testing.T) {
	app, store := fixture(t)
	ts := transportServer(t, app)
	members(t, app, map[string]string{"reader": "read", "commenter": "comment", "editor": "edit"})
	admin, reader, commenter, editor := newTransports(t, app, ts, ""), newTransports(t, app, ts, "reader"), newTransports(t, app, ts, "commenter"), newTransports(t, app, ts, "editor")
	note, err := store.Save(t.Context(), "whales.md", []byte("# Whales\n\nThe whale returns at dawn. See [the harbour](harbour.md).\n"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(t.Context(), "harbour.md", []byte("# Harbour\n\nBoats wait in the harbour.\n"), "", "writer"); err != nil {
		t.Fatal(err)
	}
	note, err = store.Read(t.Context(), "whales.md", "")
	if err != nil {
		t.Fatal(err)
	}

	settings := decoded[knowledge.RetrievalSettingsState](t, admin.tool("get_retrieval_settings", map[string]any{}))
	same[knowledge.RetrievalSettingsState](t, "retrieval settings", admin.http("GET", "/api/knowledge/retrieval-settings", nil), admin.tool("get_retrieval_settings", map[string]any{}), nil)
	change := map[string]any{"settings": settings.Settings, "expectedId": "stale"}
	same[any](t, "an editor cannot change retrieval settings", editor.http("POST", "/api/knowledge/retrieval-settings", change), editor.tool("set_retrieval_settings", change), nil)
	same[any](t, "stale retrieval settings", admin.http("POST", "/api/knowledge/retrieval-settings", change), admin.tool("set_retrieval_settings", change), nil)

	// knowledge_status adds tool guidance around the same semantic status.
	status := decoded[map[string]json.RawMessage](t, admin.tool("knowledge_status", map[string]any{}))
	same[knowledge.IndexStatus](t, "semantic status", admin.http("GET", "/api/knowledge/status", nil), outcome{status: 200, body: status["semantic"]}, nil)
	same[any](t, "snapshot", admin.http("GET", "/api/knowledge/snapshot", nil), admin.tool("read_project_changes", map[string]any{"snapshot": true}), nil)
	same[any](t, "changes", admin.http("GET", "/api/knowledge/changes?since="+note.Revision, nil), admin.tool("read_project_changes", map[string]any{"since": note.Revision}), nil)
	same[knowledge.Pending](t, "indexing passages", admin.http("GET", "/api/knowledge/passages", nil), admin.tool("get_indexing_passages", map[string]any{}), nil)
	submission := map[string]any{"configId": "none", "revision": note.Revision, "vectors": []any{}}
	same[any](t, "a reader cannot submit embeddings", reader.http("POST", "/api/knowledge/embeddings", submission), reader.tool("submit_embeddings", map[string]any{"submission": submission}), nil)
	same[any](t, "embeddings without a configuration", admin.http("POST", "/api/knowledge/embeddings", submission), admin.tool("submit_embeddings", map[string]any{"submission": submission}), nil)
	same[any](t, "semantic search without embeddings", admin.http("POST", "/api/knowledge/search", map[string]any{"text": "whale"}), admin.tool("semantic_search", map[string]any{"query": map[string]any{"text": "whale"}}), nil)
	same[knowledge.HybridResult](t, "hybrid search", admin.http("POST", "/api/knowledge/hybrid-search", map[string]any{"text": "whale", "mode": "text"}), admin.tool("hybrid_search", map[string]any{"query": map[string]any{"text": "whale", "mode": "text"}}), nil)
	same[any](t, "hybrid search without text", admin.http("POST", "/api/knowledge/hybrid-search", map[string]any{}), admin.tool("hybrid_search", map[string]any{"query": map[string]any{}}), nil)
	same[knowledge.Evidence](t, "note sources", admin.http("GET", "/api/knowledge/evidence?path=whales.md", nil), admin.tool("get_note_sources", map[string]any{"path": "whales.md"}), nil)
	same[any](t, "sources of a missing note", admin.http("GET", "/api/knowledge/evidence?path=missing.md", nil), admin.tool("get_note_sources", map[string]any{"path": "missing.md"}), nil)

	config := map[string]any{"config": map[string]any{"enabled": false, "model": "", "dimensions": 0, "passageRunes": 0, "splitter": "", "endpoint": false}}
	same[any](t, "an editor cannot configure the index", editor.http("POST", "/api/knowledge/config", config), editor.tool("set_semantic_config", config), nil)
	same[knowledge.IndexStatus](t, "configure the index", admin.http("POST", "/api/knowledge/config", config), admin.tool("set_semantic_config", config), nil)

	instructions := decoded[map[string]any](t, admin.tool("project_instructions", map[string]any{}))
	same[map[string]any](t, "instructions", admin.http("GET", "/api/knowledge/instructions", nil), admin.tool("project_instructions", map[string]any{}), nil)
	write := map[string]any{"content": "Write plainly.\n", "revision": instructions["revision"]}
	same[any](t, "an editor cannot set instructions", editor.http("POST", "/api/knowledge/instructions", write), editor.tool("set_project_instructions", write), nil)
	expect(t, "set instructions through the browser", admin.http("POST", "/api/knowledge/instructions", write), 200, "")
	same[any](t, "stale instructions", admin.http("POST", "/api/knowledge/instructions", write), admin.tool("set_project_instructions", write), nil)
	note, err = store.Read(t.Context(), "whales.md", "")
	if err != nil {
		t.Fatal(err)
	}

	// Findings and proposals: the browser fixes the kind or mode in its path.
	finding := func(key string) map[string]any {
		return map[string]any{"path": "whales.md", "revision": note.Revision, "hash": workspace.ContentHash(note.Content), "key": key, "title": "Dawn or dusk?", "text": "The harbour note disagrees.", "evidence": []map[string]any{{"path": "harbour.md", "explanation": "Boats wait."}}}
	}
	same[any](t, "a reader cannot file a finding", reader.http("POST", "/api/knowledge/findings", finding("reader")), reader.tool("file_review_finding", map[string]any{"finding": finding("reader")}), nil)
	fromBrowser := decoded[workspace.ReviewFinding](t, commenter.http("POST", "/api/knowledge/findings", finding("browser")))
	note, _ = store.Read(t.Context(), "whales.md", "")
	fromMCP := decoded[workspace.ReviewFinding](t, commenter.tool("file_review_finding", map[string]any{"finding": finding("mcp")}))
	same[map[string]any](t, "review inbox", admin.http("GET", "/api/knowledge/review", nil), admin.tool("review_inbox", map[string]any{}), nil)
	resolve := func(f workspace.ReviewFinding) map[string]any {
		discussion := decoded[struct {
			Revision string `json:"revision"`
		}](t, admin.tool("list_comments", map[string]any{"path": "whales.md"}))
		return map[string]any{"path": "whales.md", "id": f.Comment.ID, "revision": discussion.Revision, "decision": "resolve"}
	}
	// A commenter cannot resolve another person's finding on either transport.
	theirs := resolve(fromMCP)
	browserDenied := reader.http("POST", "/api/knowledge/findings/decide", theirs)
	expect(t, "a reader cannot decide a finding", browserDenied, 403, "forbidden")
	expect(t, "resolve a finding through the browser", editor.http("POST", "/api/knowledge/findings/decide", resolve(fromBrowser)), 200, "")
	decision := resolve(fromMCP)
	decision["kind"] = "finding"
	expect(t, "resolve a finding through MCP", editor.tool("review_decision", decision), 200, "")
	decision["kind"] = "verdict"
	expect(t, "an unknown decision kind", admin.tool("review_decision", decision), 400, "invalid_request")

	note, _ = store.Read(t.Context(), "whales.md", "")
	group := func(id, text string) map[string]any {
		return map[string]any{"id": id, "summary": "Dusk", "edits": []map[string]any{{"path": "whales.md", "revision": note.Revision, "hash": workspace.ContentHash(note.Content), "content": strings.Replace(string(note.Content), "dawn", text, 1)}}}
	}
	same[any](t, "a reader cannot propose", reader.http("POST", "/api/knowledge/proposals", group("reader", "dusk")), reader.tool("submit_change_group", map[string]any{"mode": "proposal", "group": group("reader", "dusk")}), nil)
	expect(t, "an unknown group mode", admin.tool("submit_change_group", map[string]any{"mode": "maybe", "group": group("maybe", "dusk")}), 400, "invalid_request")
	proposed := decoded[knowledge.Proposal](t, commenter.http("POST", "/api/knowledge/proposals", group("browser-proposal", "dusk")))
	expect(t, "propose through MCP", commenter.tool("submit_change_group", map[string]any{"mode": "proposal", "group": group("mcp-proposal", "noon")}), 200, "")
	same[any](t, "a commenter cannot apply", commenter.http("POST", "/api/knowledge/groups", group("apply", "dusk")), commenter.tool("submit_change_group", map[string]any{"mode": "apply", "group": group("apply", "dusk")}), nil)
	accept := map[string]any{"id": proposed.ID, "decision": "accept"}
	expect(t, "a commenter cannot accept", commenter.http("POST", "/api/knowledge/proposals/decide", accept), 403, "forbidden")
	same[any](t, "a commenter cannot accept", commenter.http("POST", "/api/knowledge/proposals/decide", accept), commenter.tool("review_decision", map[string]any{"kind": "proposal", "id": proposed.ID, "decision": "accept"}), nil)
	expect(t, "accept through the browser", editor.http("POST", "/api/knowledge/proposals/decide", accept), 200, "")
	note, _ = store.Read(t.Context(), "whales.md", "")
	if !strings.Contains(string(note.Content), "dusk") {
		t.Fatalf("accepted proposal not applied: %s", note.Content)
	}
	expect(t, "apply through MCP", editor.tool("submit_change_group", map[string]any{"mode": "apply", "group": group("mcp-apply", "night")}), 200, "")
	note, _ = store.Read(t.Context(), "whales.md", "")
	expect(t, "apply through the browser", editor.http("POST", "/api/knowledge/groups", group("browser-apply", "morning")), 200, "")

	same[map[string]any](t, "consumer checkpoint", admin.http("GET", "/api/knowledge/checkpoint?consumer=review", nil), admin.tool("consumer_checkpoint", map[string]any{"consumer": "review"}), nil)
	completion := map[string]any{"consumer": "review", "previous": "not-the-checkpoint", "revision": headRevision(t, store), "summary": "Reviewed.", "reset": false}
	same[any](t, "a reader cannot complete", reader.http("POST", "/api/knowledge/complete", completion), reader.tool("complete_workflow", map[string]any{"completion": completion}), nil)
	same[any](t, "a stale completion", admin.http("POST", "/api/knowledge/complete", completion), admin.tool("complete_workflow", map[string]any{"completion": completion}), nil)
	completion["previous"] = ""
	expect(t, "complete through the browser", admin.http("POST", "/api/knowledge/complete", completion), 200, "")
	checkpoint := decoded[map[string]any](t, admin.tool("consumer_checkpoint", map[string]any{"consumer": "review"}))
	if checkpoint["completionMeaning"] != checkpointMeaning {
		t.Fatalf("checkpoint: %v", checkpoint)
	}
}

// Every transport authorizes before validating, fencing or reading a note,
// and refuses a non-Markdown path as invalid input.
func TestNoteReadAndSaveAgreeAcrossTransports(t *testing.T) {
	app, store := fixture(t)
	ts := transportServer(t, app)
	members(t, app, map[string]string{"reader": "read"})
	admin, reader, outsider := newTransports(t, app, ts, ""), newTransports(t, app, ts, "reader"), newTransports(t, app, ts, "outsider")
	note, err := store.Save(t.Context(), "note.md", []byte("# Note\n"), "", "writer")
	if err != nil {
		t.Fatal(err)
	}
	same[any](t, "read", admin.http("GET", "/api/note?path=note.md", nil), admin.tool("read_note", map[string]any{"path": "note.md"}), nil)
	same[any](t, "read a missing note", admin.http("GET", "/api/note?path=missing.md", nil), admin.tool("read_note", map[string]any{"path": "missing.md"}), nil)
	media := admin.http("GET", "/api/note?path=assets/cover.png", nil)
	expect(t, "read media as a note", media, 400, "invalid_request")
	same[any](t, "read media as a note", media, admin.tool("read_note", map[string]any{"path": "assets/cover.png"}), nil)
	text := map[string]any{"path": "notes/data.txt", "content": "plain", "revision": ""}
	same[any](t, "save a non-Markdown path", admin.http("POST", "/api/note", text), admin.tool("save_note", text), nil)
	expect(t, "save a non-Markdown path", admin.http("POST", "/api/note", text), 400, "invalid_request")

	// A wrong authority generation is a conflict only for a caller who may
	// write; anyone else learns nothing about the notebook's authority.
	fenced := map[string]any{"path": "note.md", "content": "# Fenced\n", "revision": note.Revision, "authorityGeneration": 999}
	same[any](t, "a stale authority generation", admin.http("POST", "/api/note", fenced), admin.tool("save_note", fenced), nil)
	expect(t, "a stale authority generation", admin.tool("save_note", fenced), 409, "conflict")
	same[any](t, "a reader with a stale generation", reader.http("POST", "/api/note", fenced), reader.tool("save_note", fenced), nil)
	expect(t, "a reader with a stale generation", reader.tool("save_note", fenced), 403, "forbidden")
	expect(t, "an outsider with a stale generation", outsider.tool("save_note", fenced), 403, "forbidden")
	expect(t, "an outsider with a stale generation", outsider.http("POST", "/api/note", fenced), 403, "forbidden")
	fenced["authorityGeneration"] = app.catalog.Authority("default").Generation
	expect(t, "an outsider with the current generation", outsider.tool("save_note", fenced), 403, "forbidden")
	expect(t, "save through MCP", admin.tool("save_note", fenced), 200, "")
}
