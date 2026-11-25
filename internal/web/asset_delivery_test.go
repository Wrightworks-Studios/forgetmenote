package web

import (
	"bytes"
	"encoding/json"
	"html/template"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFingerprintedPublicAssetsAndLegacyAliases(t *testing.T) {
	app, _ := fixture(t)
	raw, err := assets.ReadFile("ui/dist/manifest.json")
	if err != nil {
		t.Fatal("missing asset manifest:", err)
	}
	var manifest struct {
		Aliases map[string]string `json:"aliases"`
		Files   []string          `json:"files"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, legacy := range []string{"/static/app.mjs", "/static/style.css", "/static/dist/source-editor.js", "/static/dist/writing-worker.js"} {
		url := manifest.Aliases[legacy]
		if url == "" || url == legacy {
			t.Fatalf("unfingerprinted entry %q: %q", legacy, url)
		}
		current := call(app, "GET", url, "", "")
		if current.Code != 200 || current.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Fatalf("asset %s: %d %s", url, current.Code, current.Header().Get("Cache-Control"))
		}
		old := call(app, "GET", legacy, "", "")
		if old.Code != 200 || old.Header().Get("Cache-Control") != "no-store" || old.Body.String() != current.Body.String() {
			t.Fatalf("legacy asset %s lost compatibility", legacy)
		}
	}
	page := call(app, "GET", "/", "", testToken)
	if page.Code != 200 || !strings.Contains(page.Body.String(), manifest.Aliases["/static/app.mjs"]) || !strings.Contains(page.Body.String(), manifest.Aliases["/static/style.css"]) {
		t.Fatal("page does not use fingerprinted assets")
	}
	if page.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("private page cache policy changed")
	}
	missing := call(app, "GET", "/static/dist/missing-ABCDEF12.js", "", "")
	if missing.Code != 404 || missing.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("unlisted asset became cacheable")
	}
	// The Visual editor of earlier releases is gone: a tab still running their
	// code gets a 404 for it, and opens the note in Source instead.
	for _, removed := range []string{"/static/dist/milkdown.js", "/static/dist/milkdown.css"} {
		if manifest.Aliases[removed] != "" {
			t.Fatalf("removed entry %s is still built", removed)
		}
		if w := call(app, "GET", removed, "", ""); w.Code != 404 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("removed entry %s: %d %s", removed, w.Code, w.Header().Get("Cache-Control"))
		}
	}
	r := httptest.NewRequest("GET", app.origin+manifest.Aliases["/static/app.mjs"], nil)
	r.Header.Set("Range", "bytes=999999999999-")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 416 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("invalid range cached", w.Code, w.Header())
	}
	app.ConfigureHeadless(true)
	for _, legacy := range []string{"/static/app.mjs", "/static/dist/writing-worker.js", "/static/style.css", "/static/connect-ai.mjs", "/static/invitation.mjs"} {
		want := 404
		if legacy == "/static/style.css" || legacy == "/static/connect-ai.mjs" || legacy == "/static/invitation.mjs" {
			want = 200
		}
		if got := call(app, "GET", manifest.Aliases[legacy], "", ""); got.Code != want {
			t.Fatalf("headless %s: %d", legacy, got.Code)
		}
	}
}

func TestEmbeddedAssetsAreTheBuildAndItsLicenses(t *testing.T) {
	app, _ := fixture(t)
	// Embedding pages import the chat panel by its stable entry URL.
	chat := call(app, "GET", "/static/chat.mjs", "", "")
	if chat.Code != 200 || !strings.Contains(chat.Header().Get("Content-Type"), "javascript") || !strings.Contains(chat.Body.String(), "initChat") {
		t.Fatalf("chat entry: %d %s", chat.Code, chat.Header().Get("Content-Type"))
	}
	for _, path := range []string{"/static/chat-model.mjs", "/static/editor.mjs", "/static/styles/base.css", "/static/fonts/Onest.ttf", "/static/fonts/manifest.json"} {
		if w := call(app, "GET", path, "", ""); w.Code != 404 {
			t.Errorf("unbundled source %s served: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/static/fonts/OFL.txt", "/static/fonts/Fraunces-OFL.txt", "/static/fonts/SourceSerif4-LICENSE.md"} {
		if w := call(app, "GET", path, "", ""); w.Code != 200 || w.Body.Len() == 0 {
			t.Errorf("font license %s: %d", path, w.Code)
		}
	}
}

func TestPagesNamingAnUnbuiltAssetFailToRender(t *testing.T) {
	app, _ := fixture(t)
	page := template.Must(template.New("page").Funcs(pageFuncs(app.catalog, app.browserAssets)).Parse(`<script src="{{asset "/static/app.mjs"}}"></script>{{asset "/static/missing.mjs"}}`))
	var out bytes.Buffer
	if err := page.Execute(&out, nil); err == nil || !strings.Contains(err.Error(), "/static/missing.mjs") {
		t.Fatalf("unknown asset rendered: %v %q", err, out.String())
	}
	if !strings.Contains(out.String(), app.browserAssets.Aliases["/static/app.mjs"]) {
		t.Fatalf("known asset: %q", out.String())
	}
}
