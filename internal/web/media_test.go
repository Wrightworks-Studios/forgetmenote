package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVideoPreviewPreservesPortableLinkAndRestrictsFrames(t *testing.T) {
	app, _ := fixture(t)
	html := app.render("[Video](<https://youtu.be/M7lc1UVf-VE> \"video\")\n\n<iframe src=\"https://evil.example\"></iframe>")
	if !strings.Contains(html, `title="video"`) || !strings.Contains(html, `href="https://youtu.be/M7lc1UVf-VE"`) || strings.Contains(html, "<iframe") {
		t.Fatalf("video link lost or raw iframe accepted: %s", html)
	}
	response := call(app, "GET", "/", "", testToken)
	policy := response.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "frame-src 'self' https://www.youtube-nocookie.com https://player.vimeo.com;") || !strings.Contains(policy, "media-src 'self';") {
		t.Fatalf("unexpected media policy: %s", policy)
	}
	// The editor's toolbar (built by the editor bundle) offers attachments and
	// videos in its Insert menu, through the page's one file input.
	editor := call(app, "GET", "/static/dist/source-editor.js", "", "")
	if strings.Contains(response.Body.String(), `id="upload-open"`) || !strings.Contains(response.Body.String(), `id="upload"`) || editor.Code != 200 || !strings.Contains(editor.Body.String(), "Insert block") || !strings.Contains(editor.Body.String(), "Video embed") {
		t.Fatalf("attachment control must be in the editor: editor bundle %d, %d bytes", editor.Code, editor.Body.Len())
	}
}

func TestProjectVideoRequiresAccessAndSupportsSeeking(t *testing.T) {
	app, store := fixture(t)
	_, err := store.Save(context.Background(), "assets/clip.mp4", []byte("0123456789"), "", "media test")
	if err != nil {
		t.Fatal(err)
	}
	if w := call(app, "GET", "/assets/assets/clip.mp4", "", ""); w.Code != 401 {
		t.Fatalf("private media exposed: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "http://localhost/p/default/assets/assets/clip.mp4", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Range", "bytes=2-5")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != http.StatusPartialContent || w.Body.String() != "2345" || w.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("video seeking: %d %s", w.Code, w.Body.String())
	}
}
