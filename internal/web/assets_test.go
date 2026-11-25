package web

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func TestBrowserAssetsRequireCompleteBuild(t *testing.T) {
	manifest, err := loadBrowserManifest(assets)
	if err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{}
	for _, url := range manifest.Files {
		files["ui/"+strings.TrimPrefix(url, "/static/")] = &fstest.MapFile{Data: []byte("built"), Mode: 0644}
	}
	data, _ := assets.ReadFile("ui/dist/manifest.json")
	files["ui/dist/manifest.json"] = &fstest.MapFile{Data: data, Mode: 0644}
	files["ui/dist/THIRD_PARTY_LICENSES.txt"] = &fstest.MapFile{Data: []byte("licenses"), Mode: 0644}
	target := "ui/" + strings.TrimPrefix(manifest.Aliases["/static/dist/collaboration.js"], "/static/")
	if err := validateBrowserAssets(files); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []fs.FileMode{0644, fs.ModeDir | 0755} {
		files[target] = &fstest.MapFile{Mode: mode}
		if err := validateBrowserAssets(files); err == nil || !strings.Contains(err.Error(), "npm ci && npm run build") {
			t.Fatalf("incomplete bundle accepted: %v", err)
		}
	}
	delete(files, target)
	if err := validateBrowserAssets(files); err == nil {
		t.Fatal("missing bundle accepted")
	}
	if err := validateBrowserAssets(assets); err != nil {
		t.Fatalf("locally built bundle: %v", err)
	}
}

func TestBrowserAssetsRequireThePreloadedFonts(t *testing.T) {
	data, _ := assets.ReadFile("ui/dist/manifest.json")
	for _, font := range []string{"/static/fonts/Onest.woff2", "/static/fonts/Fraunces.woff2", "/static/fonts/SourceSerif4-Roman.woff2"} {
		var manifest browserManifest
		if err := json.Unmarshal(data, &manifest); err != nil || manifest.Aliases[font] == "" {
			t.Fatalf("built manifest lacks %s: %v", font, err)
		}
		delete(manifest.Aliases, font)
		raw, _ := json.Marshal(manifest)
		files := fstest.MapFS{"ui/dist/manifest.json": &fstest.MapFile{Data: raw, Mode: 0644}}
		for _, url := range manifest.Files {
			files["ui/"+strings.TrimPrefix(url, "/static/")] = &fstest.MapFile{Data: []byte("built"), Mode: 0644}
		}
		if _, err := loadBrowserManifest(files); err == nil || !strings.Contains(err.Error(), font) {
			t.Fatalf("build without %s accepted: %v", font, err)
		}
	}
}
