package web

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestSettingsHaveDistinctScopes(t *testing.T) {
	app, _ := fixture(t)
	cookie := recoverySession(t, app)
	w := oidcRequest(app, "GET", "/", "", cookie)
	root, err := html.Parse(strings.NewReader(w.Body.String()))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*html.Node{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for _, attr := range n.Attr {
			if attr.Key == "id" {
				byID[attr.Val] = n
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	if byID["project-locks-open"] != nil {
		t.Error("workspace locks shortcut duplicates item protection")
	}
	for id, parent := range map[string]string{
		"theme": "account-dialog", "reading-font": "account-dialog", "font-size": "account-dialog", "ai-connection": "account-dialog",
		"member-form": "settings-dialog", "backup-link": "settings-dialog", "member-picker": "member-form",
		"user-form": "admin-dialog", "project-form": "workspace-create-dialog",
		"oidc-identities": "identity-details", "github-attribution-form": "github-details",
	} {
		child := byID[id]
		if child == nil || byID[parent] == nil {
			t.Errorf("missing %s or %s", id, parent)
			continue
		}
		for child != nil && child != byID[parent] {
			child = child.Parent
		}
		if child == nil {
			t.Errorf("%s must be inside %s", id, parent)
		}
	}
	for _, id := range []string{"identity-details", "github-details"} {
		if n := byID[id]; n != nil {
			for _, attr := range n.Attr {
				if attr.Key == "open" {
					t.Errorf("%s should start collapsed", id)
				}
			}
		}
	}
}

// Settings modules reach the browser inside the app bundle, not as raw files.
func TestSettingsModulesAreBundled(t *testing.T) {
	app, _ := fixture(t)
	if w := oidcRequest(app, "GET", "/static/app.mjs", ""); w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Errorf("app bundle unavailable: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	for _, path := range []string{"/static/retrieval-settings.mjs", "/static/containers.mjs", "/static/settings.mjs", "/static/administration.mjs", "/static/automations.mjs"} {
		if w := oidcRequest(app, "GET", path, ""); w.Code != 404 {
			t.Errorf("raw module served: %s %d", path, w.Code)
		}
	}
}

func TestPolishedControlsAreAccessible(t *testing.T) {
	app, _ := fixture(t)
	cookie := recoverySession(t, app)
	w := oidcRequest(app, "GET", "/", "", cookie)
	markup := w.Body.String()
	for _, control := range []string{`<fieldset id="theme"`, `name="theme-choice" value="light"`, `name="theme-choice" value="dark"`, `name="theme-choice" value="parchment"`, `id="editor-tools"`, `id="formatting-tools"`} {
		if !strings.Contains(markup, control) {
			t.Errorf("missing accessible control: %s", control)
		}
	}
}
