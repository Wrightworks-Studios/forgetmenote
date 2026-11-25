package web

import (
	"net/http/httptest"
	"testing"
)

func TestRequestsMustNameTheCurrentOriginHost(t *testing.T) {
	app, _ := fixture(t)
	get := func(url string) int {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest("GET", url, nil))
		return w.Code
	}
	if code := get("http://localhost/login"); code != 200 {
		t.Fatalf("public page before sign-in: %d", code)
	}
	if code := get("http://localhost/api/me"); code != 401 {
		t.Fatalf("private route before sign-in: %d", code)
	}
	if code := get("http://other.example/login"); code != 403 {
		t.Fatalf("foreign host accepted: %d", code)
	}
	app.origin = "http://127.0.0.1:9"
	if code := get("http://localhost/login"); code != 403 {
		t.Fatalf("previous origin host accepted: %d", code)
	}
	if code := get("http://127.0.0.1:9/login"); code != 200 {
		t.Fatalf("current origin host rejected: %d", code)
	}
}

// ServeHTTP decides once what a path is; notebook route selection, the API
// error envelope and the body limit all use that split.
func TestRequestPathsAreClassifiedOnce(t *testing.T) {
	for path, want := range map[string]requestPath{
		"/p/default/api/note":    {path: "/p/default/api/note", notebook: "default", route: "/api/note", scoped: true},
		"/p//api/note":           {path: "/p//api/note", route: "/api/note", scoped: true},
		"/p/default":             {path: "/p/default", route: "/p/default"},
		"/api/v1/actions/x":      {path: "/api/v1/actions/x", route: "/api/v1/actions/x", api: true, rpc: true},
		"/api/v1":                {path: "/api/v1", route: "/api/v1"},
		"/mcp":                   {path: "/mcp", route: "/mcp", rpc: true},
		"/static/app.js":         {path: "/static/app.js", route: "/static/app.js", static: true},
		"/p/default/static/a.js": {path: "/p/default/static/a.js", notebook: "default", route: "/static/a.js", scoped: true},
	} {
		if got := parseRequestPath(path); got != want {
			t.Errorf("%s: %+v, want %+v", path, got, want)
		}
	}
	app, _ := fixture(t)
	for path, status := range map[string]int{"/p/default": 404, "/p/default/api/status": 200, "/notebooks/default/api/status": 200} {
		if w := call(app, "GET", path, "", testToken); w.Code != status {
			t.Errorf("%s: %d %s", path, w.Code, w.Body)
		}
	}
}
