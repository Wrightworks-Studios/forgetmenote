package web

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrowserRequestBodiesAreBoundedByRoute(t *testing.T) {
	app, _ := fixture(t)
	// Larger than an ordinary browser request, as a novel-size manuscript is.
	manuscript := "# Manuscript\n\n" + strings.Repeat("Call me Ishmael. ", 100000)
	body := func(fields map[string]any) string {
		raw, _ := json.Marshal(fields)
		return string(raw)
	}
	tooLarge := func(name string, w *httptest.ResponseRecorder) {
		t.Helper()
		var detail struct{ Code, Message string }
		if w.Code != 413 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.Code != "too_large" || !strings.Contains(detail.Message, "limit") {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}

	tooLarge("comment", call(app, "POST", "/api/comments", body(map[string]any{"path": "note.md", "text": manuscript}), testToken))
	tooLarge("metadata", call(app, "POST", "/api/metadata", body(map[string]any{"path": "note.md", "metadata": map[string]any{"title": manuscript}}), testToken))
	if w := call(app, "POST", "/api/comments", `{"path":`, testToken); w.Code != 400 {
		t.Fatalf("malformed JSON: %d %s", w.Code, w.Body.String())
	}

	saved := call(app, "POST", "/api/note", body(map[string]any{"path": "manuscript.md", "content": manuscript, "revision": ""}), testToken)
	if saved.Code != 200 {
		t.Fatalf("manuscript save: %d %.200s", saved.Code, saved.Body.String())
	}
	if w := call(app, "POST", "/api/preview", body(map[string]any{"content": manuscript}), testToken); w.Code != 200 {
		t.Fatalf("manuscript preview: %d %.200s", w.Code, w.Body.String())
	}

	// Content routes keep their own budget, and exceeding it is not malformed JSON.
	t.Setenv("FMN_LIMIT_FILE_BYTES", "65536")
	t.Setenv("FMN_LIMIT_BUNDLE_BYTES", "65536")
	t.Setenv("FMN_LIMIT_SOURCE_BYTES", "65536")
	tooLarge("oversized save", call(app, "POST", "/api/note", body(map[string]any{"path": "manuscript.md", "content": manuscript + manuscript, "revision": ""}), testToken))

	var upload bytes.Buffer
	form := multipart.NewWriter(&upload)
	part, _ := form.CreateFormFile("file", "cover.png")
	part.Write(bytes.Repeat([]byte("p"), 2<<20))
	form.Close()
	r := httptest.NewRequest("POST", "http://localhost/p/default/api/upload", &upload)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	tooLarge("oversized upload", w)
}

func TestRequestBodyLimitsFollowTheRouteNotTheNotebook(t *testing.T) {
	limit := func(path string) int64 { return requestBodyLimit(parseRequestPath(path)) }
	small, content := int64(browserBodyBytes), limit("/p/default/api/note")
	if content <= small || encodedUploadRequestLimit() <= small {
		t.Fatalf("content limit %d, MCP limit %d", content, encodedUploadRequestLimit())
	}
	for path, want := range map[string]int64{
		"/p/any/api/note": content, "/p/any/api/live": content, "/p/any/api/live/patch": content, "/p/any/api/upload": content,
		"/p/any/api/preview": content, "/p/any/api/view/preview": content, "/p/any/api/chat/c1/turns": content,
		"/p/any/api/knowledge/groups": content, "/p/any/api/knowledge/proposals": content, "/api/projects/import": content,
		"/api/quicknotes": content, "/integrations/slack/events": content, "/api/organization/action/set_instance_branding": content,
		"/api/organization/action/set_ai_settings": small, "/p/any/api/live/checkpoint": small, "/p/any/api/comments": small, "/p/any/api/chat": small, "/p/any/api/chat/c1/save-note": small,
		"/api/quicknotes/settings": small, "/api/projects": small, "/account/password": small, "/integrations/actions/call_data_operation": small,
		"/api/note": content, "/p/api/note": small,
	} {
		if got := limit(path); got != want {
			t.Errorf("%s: limit %d, want %d", path, got, want)
		}
	}
	if limit("/p/any/api/sources/jobs") <= content || limit("/mcp") != encodedUploadRequestLimit() || limit("/api/v1/actions/save_note") != encodedUploadRequestLimit() {
		t.Fatal("source and MCP limits changed")
	}
}
