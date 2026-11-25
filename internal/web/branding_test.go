package web

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
	"testing"

	"forgetmenote/internal/branding"
)

func TestMCPAdministratorCanSetBranding(t *testing.T) {
	app, _ := fixture(t)
	client := actionClient(t, app, "@local-admin", "notes:read notes:manage")
	actionCall(t, client, "set_instance_branding", map[string]any{"name": "MCP Team", "logo": base64.StdEncoding.EncodeToString(branding.DefaultIcon())}, false)
	if app.catalog.InstanceName() != "MCP Team" || len(app.catalog.InstanceLogo()) == 0 {
		t.Fatal("MCP branding did not persist")
	}
}

func TestAdministratorBrandingReachesPagesAndCompanion(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	var logo bytes.Buffer
	im := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	im.Set(0, 0, color.NRGBA{R: 220, A: 255})
	if err := png.Encode(&logo, im); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"name": "Team <Research>", "logo": logo.Bytes()})
	w := localRequest(app, "/api/organization/action/set_instance_branding", string(body), admin)
	if w.Code != 200 {
		t.Fatalf("save branding: %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/", "/login", "/invitation", "/account/connect"} {
		w = oidcRequest(app, "GET", path, "", admin)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "Team &lt;Research&gt;") || !strings.Contains(w.Body.String(), "/static/brand/instance.png?v=") {
			t.Fatalf("custom brand missing on %s: %d %s", path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "/static/brand/flower.svg") {
			t.Fatalf("stale default brand on %s", path)
		}
	}
	w = call(app, "GET", "/static/brand/instance.png", "", "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("public logo: %d %s", w.Code, w.Body.String())
	}
	publicLogo := append([]byte(nil), w.Body.Bytes()...)
	if _, err := png.Decode(bytes.NewReader(publicLogo)); err != nil {
		t.Fatal(err)
	}
	w = call(app, "GET", "/favicon.ico", "", "")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), publicLogo) {
		t.Fatal("favicon differs from instance logo")
	}
	w = oidcRequest(app, "GET", "/account/connect/download/codex", "", admin)
	z, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, f := range z.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		files[f.Name], err = io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(files["assets/icon.png"], publicLogo) {
		t.Fatal("companion does not contain instance logo")
	}
	for _, file := range []string{"plugin.json", ".codex-plugin/plugin.json"} {
		for _, field := range []string{`"composerIcon": "./assets/icon.png"`, `"logo": "./assets/icon.png"`} {
			if !bytes.Contains(files[file], []byte(field)) {
				t.Fatalf("%s missing %s", file, field)
			}
		}
	}
	// Name-only clients preserve the logo; an explicit empty logo restores the default.
	w = localRequest(app, "/api/organization/action/set_instance_name", `{"name":"Renamed"}`, admin)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = call(app, "GET", "/static/brand/instance.png", "", ""); !bytes.Equal(w.Body.Bytes(), publicLogo) {
		t.Fatal("renaming removed the logo")
	}
	w = localRequest(app, "/api/organization/action/set_instance_branding", `{"name":"Renamed","logo":""}`, admin)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = oidcRequest(app, "GET", "/", "", admin)
	if !strings.Contains(w.Body.String(), "/static/brand/flower.svg") {
		t.Fatal("default logo was not restored")
	}
}

func TestInstanceBrandingRejectsUntrustedUpdates(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	for _, logo := range [][]byte{[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), []byte("not an image")} {
		body, _ := json.Marshal(map[string]any{"name": "Uncommitted", "logo": logo})
		w := localRequest(app, "/api/organization/action/set_instance_branding", string(body), admin)
		if w.Code != 400 {
			t.Fatalf("invalid logo: %d %s", w.Code, w.Body.String())
		}
		if app.catalog.InstanceName() != "ForgetMeNote" {
			t.Fatal("failed logo update changed the name")
		}
	}
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	w := proxyCall(app, "reader", "POST", "/api/organization/action/set_instance_branding", `{"name":"Unauthorized","logo":""}`)
	if w.Code != 403 {
		t.Fatalf("non-admin branding: %d %s", w.Code, w.Body.String())
	}
}

func TestSignedInPagesUseForgetMeNoteBrand(t *testing.T) {
	app, _ := fixture(t)
	cookie := recoverySession(t, app)
	for _, path := range []string{"/login", "/"} {
		w := oidcRequest(app, "GET", path, "", cookie)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "ForgetMeNote") || strings.Contains(w.Body.String(), ">Gitnotes<") {
			t.Fatalf("%s not rebranded: %d", path, w.Code)
		}
	}
}

func TestFlowerBrandAndIconsAreAvailableBeforeLogin(t *testing.T) {
	app, _ := fixture(t)
	for _, path := range []string{"/static/brand/flower.svg", "/static/brand/favicon.ico", "/favicon.ico", "/static/brand/apple-touch-icon.png", "/static/fonts/Fraunces.woff2"} {
		w := call(app, "GET", path, "", "")
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Errorf("brand asset %s: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/login", "/"} {
		w := call(app, "GET", path, "", testToken)
		html := w.Body.String()
		for _, asset := range []string{`rel="icon"`, `rel="apple-touch-icon"`, `/static/brand/flower.svg`} {
			if !strings.Contains(html, asset) {
				t.Errorf("%s missing %s", path, asset)
			}
		}
	}
}

type webManifest struct {
	ID, Name, ShortName, StartURL, Scope, Display, BackgroundColor, ThemeColor string
	Icons                                                                      []manifestIcon
}

// readManifest fetches the manifest and its icons without credentials, as
// browsers do, and checks each icon's declared size.
func readManifest(t *testing.T, app *Server) webManifest {
	t.Helper()
	w := call(app, "GET", "/manifest.webmanifest", "", "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/manifest+json" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("manifest: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var m webManifest
	fields, _ := json.Marshal(map[string]any{"ID": raw["id"], "Name": raw["name"], "ShortName": raw["short_name"], "StartURL": raw["start_url"], "Scope": raw["scope"], "Display": raw["display"], "BackgroundColor": raw["background_color"], "ThemeColor": raw["theme_color"], "Icons": raw["icons"]})
	if err := json.Unmarshal(fields, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "/" || m.StartURL != "/" || m.Scope != "/" || m.Display != "standalone" || m.BackgroundColor != "#faf9f5" || m.ThemeColor != "#faf9f5" {
		t.Fatalf("manifest fields: %+v", m)
	}
	var sizes []string
	for _, icon := range m.Icons {
		sizes = append(sizes, icon.Sizes+" "+icon.Purpose)
		w := call(app, "GET", icon.Src, "", "")
		if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("icon %s: %d %s", icon.Src, w.Code, w.Body.String())
		}
		im, err := png.Decode(bytes.NewReader(w.Body.Bytes()))
		if err != nil {
			t.Fatal(icon.Src, err)
		}
		if got := fmt.Sprintf("%dx%d", im.Bounds().Dx(), im.Bounds().Dy()); got != icon.Sizes || icon.Type != "image/png" {
			t.Fatalf("icon %s is %s %s, declared %s", icon.Src, got, icon.Type, icon.Sizes)
		}
		if _, _, _, a := im.At(0, 0).RGBA(); icon.Purpose == "maskable" && a != 0xffff {
			t.Fatalf("maskable icon %s is not opaque", icon.Src)
		}
	}
	if strings.Join(sizes, ", ") != "192x192 any, 512x512 any, 512x512 maskable" {
		t.Fatalf("icons: %v", sizes)
	}
	return m
}

func TestWebManifestFollowsInstanceBrand(t *testing.T) {
	app, _ := fixture(t)
	m := readManifest(t, app)
	if m.Name != "ForgetMeNote" || m.ShortName != "ForgetMeNote" || m.Icons[0].Src != "/static/brand/app-icon-192.png" {
		t.Fatalf("default manifest: %+v", m)
	}
	w := call(app, "GET", "/login", "", "")
	for _, head := range []string{`<link rel="manifest" href="/manifest.webmanifest" />`, `<meta name="theme-color" content="#faf9f5" media="(prefers-color-scheme: light)" />`, `<meta name="theme-color" content="#1d211f" media="(prefers-color-scheme: dark)" />`} {
		if !strings.Contains(w.Body.String(), head) {
			t.Fatalf("sign-in page lacks %s", head)
		}
	}
	// The page policy admits the same-origin manifest and icons.
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "img-src 'self' data:") || strings.Contains(csp, "manifest-src") {
		t.Fatalf("policy: %s", csp)
	}

	admin := recoverySession(t, app)
	wide := image.NewNRGBA(image.Rect(0, 0, 60, 20))
	for y := range 20 {
		for x := range 60 {
			wide.Set(x, y, color.NRGBA{G: 120, A: 255})
		}
	}
	var logo bytes.Buffer
	if err := png.Encode(&logo, wide); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"name": "Team <Research>", "logo": logo.Bytes()})
	if w := localRequest(app, "/api/organization/action/set_instance_branding", string(body), admin); w.Code != 200 {
		t.Fatalf("save branding: %d %s", w.Code, w.Body.String())
	}
	m = readManifest(t, app)
	if m.Name != "Team <Research>" || m.ShortName != "Team <Research>" || !strings.HasPrefix(m.Icons[0].Src, "/static/brand/instance-192.png?v=") {
		t.Fatalf("custom manifest: %+v", m)
	}
	// A manifest from before the logo was removed still finds an icon.
	stale := m.Icons[2].Src
	if w := localRequest(app, "/api/organization/action/set_instance_branding", `{"name":"Renamed","logo":""}`, admin); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if m = readManifest(t, app); m.Name != "Renamed" || m.Icons[2].Src != "/static/brand/app-icon-maskable-512.png" {
		t.Fatalf("restored manifest: %+v", m)
	}
	if w := call(app, "GET", stale, "", ""); w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("stale icon: %d", w.Code)
	}
}
