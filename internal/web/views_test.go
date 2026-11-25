package web

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const viewNote = "# Dashboard\n\nIntro.\n\n```view\n<p id=\"one\">One</p>\n```\n\nBetween.\n\n```js\nnot a view\n```\n\n- item\n\n  ```view\n  <p>Two</p><script>window.two = 2</script>\n  ```\n"

func viewCall(app *Server, method, path, form, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost/p/default"+path, strings.NewReader(form))
	if form != "" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "http://localhost")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	return w
}

func requireViewDocument(t *testing.T, w *httptest.ResponseRecorder, source string) {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("view: %d %s", w.Code, w.Body.String())
	}
	want := "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline' http://localhost/static/views/; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; connect-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'self'"
	if got := w.Header().Get("Content-Security-Policy"); got != want {
		t.Fatalf("view policy:\n got %s\nwant %s", got, want)
	}
	for header, value := range map[string]string{"Content-Type": "text/html; charset=utf-8", "Cache-Control": "private, no-store", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer"} {
		if got := w.Header().Get(header); got != value {
			t.Fatalf("%s = %q, want %q", header, got, value)
		}
	}
	body := w.Body.String()
	bridge := strings.Index(body, `<script src="/static/views/fmn-view.js"></script>`)
	if bridge < 0 || !strings.Contains(body[bridge:], source) {
		t.Fatalf("view document must load the bridge before the block %q:\n%s", source, body)
	}
}

func TestViewBlocksAreServedOnlyInsideTheirSandbox(t *testing.T) {
	app, store := fixture(t)
	if html := app.render("```view\n<b>x</b>\n```"); !strings.Contains(html, `<code class="language-view">`) || strings.Contains(html, "<b>") {
		t.Fatalf("a view block must stay an escaped code block in rendered Markdown: %s", html)
	}
	if policy := call(app, "GET", "/", "", testToken).Header().Get("Content-Security-Policy"); !strings.Contains(policy, "frame-src 'self' https://www.youtube-nocookie.com") || !strings.Contains(policy, "frame-ancestors 'none'") {
		t.Fatalf("the app may frame its own views but nothing may frame the app: %s", policy)
	}
	first, err := store.Save(context.Background(), "notes/dashboard.md", []byte(viewNote), "", "view test")
	if err != nil {
		t.Fatal(err)
	}
	at := func(revision, block string) string {
		return "/api/view?" + url.Values{"path": {"notes/dashboard.md"}, "revision": {revision}, "block": {block}}.Encode()
	}
	requireViewDocument(t, viewCall(app, "GET", at(first.Revision, "0"), "", testToken), `<p id="one">One</p>`)
	requireViewDocument(t, viewCall(app, "GET", at(first.Revision, "1"), "", testToken), "<p>Two</p><script>window.two = 2</script>")
	if _, err := store.Save(context.Background(), "notes/dashboard.md", []byte(strings.Replace(viewNote, "One", "Changed", 1)), first.Revision, "view test"); err != nil {
		t.Fatal(err)
	}
	requireViewDocument(t, viewCall(app, "GET", at(first.Revision, "0"), "", testToken), `<p id="one">One</p>`)
	requireViewDocument(t, viewCall(app, "GET", at("", "0"), "", testToken), `<p id="one">Changed</p>`)

	for _, path := range []string{at(first.Revision, "2"), at(first.Revision, "-1"), at(first.Revision, "x"), "/api/view?path=notes/missing.md&block=0"} {
		if w := viewCall(app, "GET", path, "", testToken); w.Code != 404 || w.Header().Get("Content-Security-Policy") == "" || strings.Contains(w.Body.String(), "<p") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if w := viewCall(app, "GET", "/api/view?path=assets/a.png&block=0", "", testToken); w.Code != 400 {
		t.Fatalf("non-note view path: %d", w.Code)
	}
	if w := viewCall(app, "GET", at(first.Revision, "0"), "", ""); w.Code != 401 || strings.Contains(w.Body.String(), "One") {
		t.Fatalf("view served without a session: %d", w.Code)
	}
}

func TestViewPreviewServesADraftBlockInTheSameSandbox(t *testing.T) {
	app, _ := fixture(t)
	source := `<p>Draft</p><script>window.draft = true</script>`
	form := url.Values{"source": {source}}.Encode()
	requireViewDocument(t, viewCall(app, "POST", "/api/view/preview", form, testToken), source)
	if w := viewCall(app, "POST", "/api/view/preview", form, ""); w.Code != 401 || strings.Contains(w.Body.String(), "Draft") {
		t.Fatalf("preview without a session: %d", w.Code)
	}
	r := httptest.NewRequest("POST", "http://localhost/p/default/api/view/preview", strings.NewReader(form))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://elsewhere.example")
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 403 || strings.Contains(w.Body.String(), "Draft") {
		t.Fatalf("cross-origin preview: %d", w.Code)
	}
}
