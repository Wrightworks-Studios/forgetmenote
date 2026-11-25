package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"regexp"
	"strings"

	"forgetmenote/internal/catalog"
)

type browserManifest struct {
	Version  int               `json:"version"`
	Aliases  map[string]string `json:"aliases"`
	Files    []string          `json:"files"`
	Headless []string          `json:"headless"`
}

var fingerprintedAsset = regexp.MustCompile(`^/static/dist/(?:[^/]+/)*[^/]+-[A-Z0-9]{8}\.[a-z0-9]+(?:\.LEGAL\.txt)?$`)

func loadBrowserManifest(files fs.FS) (browserManifest, error) {
	var manifest browserManifest
	data, err := fs.ReadFile(files, "ui/dist/manifest.json")
	if err == nil {
		err = json.Unmarshal(data, &manifest)
	}
	if err != nil || manifest.Version != 1 {
		return manifest, fmt.Errorf("browser manifest is missing or invalid; run npm ci && npm run build, then rebuild ForgetMeNote")
	}
	known := make(map[string]bool)
	for _, url := range manifest.Files {
		name := strings.TrimPrefix(url, "/static/")
		if !fingerprintedAsset.MatchString(url) || !fs.ValidPath(name) || strings.ContainsAny(name, "?#\\") || known[url] {
			return manifest, fmt.Errorf("invalid browser asset %q", url)
		}
		info, err := fs.Stat(files, "ui/"+name)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return manifest, fmt.Errorf("browser assets are missing or incomplete (%s); run npm ci && npm run build, then rebuild ForgetMeNote", name)
		}
		known[url] = true
	}
	for legacy, url := range manifest.Aliases {
		if !strings.HasPrefix(legacy, "/static/") || !known[url] {
			return manifest, fmt.Errorf("invalid browser alias %q", legacy)
		}
	}
	// Pages name these through the asset template function, which fails to
	// render an unknown alias.
	for _, name := range []string{"/static/app.mjs", "/static/style.css", "/static/assistant.mjs", "/static/invitation.mjs", "/static/connect-ai.mjs", "/static/chat.mjs", "/static/dist/source-editor.js", "/static/dist/rendered-preview.js", "/static/dist/collaboration.js", "/static/dist/source-view.js", "/static/dist/writing-worker.js", "/static/fonts/Onest.woff2", "/static/fonts/Fraunces.woff2", "/static/fonts/SourceSerif4-Roman.woff2"} {
		if manifest.Aliases[name] == "" {
			return manifest, fmt.Errorf("browser entry %s is missing; run npm ci && npm run build, then rebuild ForgetMeNote", name)
		}
	}
	for _, url := range manifest.Headless {
		if !known[url] {
			return manifest, fmt.Errorf("invalid headless asset %q", url)
		}
	}
	return manifest, nil
}

// Validate before opening private processing state: an operator can compile Go
// from a fresh clone, but serving the editor requires its generated assets.
func validateBrowserAssets(files fs.FS) error {
	if _, err := loadBrowserManifest(files); err != nil {
		return err
	}
	for _, name := range []string{"THIRD_PARTY_LICENSES.txt"} {
		info, err := fs.Stat(files, "ui/dist/"+name)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("browser assets are missing or incomplete (%s); run npm ci && npm run build, then rebuild ForgetMeNote", name)
		}
	}
	return nil
}

func browserAssetHandler(files fs.FS, manifest browserManifest) http.Handler {
	public, _ := fs.Sub(files, "ui")
	serve := http.StripPrefix("/static/", http.FileServer(http.FS(public)))
	immutable := make(map[string]bool, len(manifest.Files))
	for _, url := range manifest.Files {
		immutable[url] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w = &assetResponseWriter{ResponseWriter: w}
		if target := manifest.Aliases[r.URL.Path]; target != "" {
			copy := r.Clone(r.Context())
			copy.URL.Path = target
			copy.URL.RawPath = ""
			serve.ServeHTTP(w, copy)
			return
		}
		if immutable[r.URL.Path] {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		serve.ServeHTTP(w, r)
	})
}

type assetResponseWriter struct{ http.ResponseWriter }

func (w *assetResponseWriter) WriteHeader(status int) {
	// FileServer removes caching headers on errors; errors must still carry the
	// instance's no-store policy, including invalid ranges of immutable files.
	if status >= 400 {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.ResponseWriter.WriteHeader(status)
}

// pageFuncs serve the page templates. A page that names an asset missing from
// the build fails to render rather than linking to nothing.
func pageFuncs(projects *catalog.Catalog, manifest browserManifest) template.FuncMap {
	return template.FuncMap{
		"brand": func() instanceBrand { return currentBrand(projects) },
		"asset": func(path string) (string, error) {
			if url := manifest.Aliases[path]; url != "" {
				return url, nil
			}
			return "", fmt.Errorf("browser asset %s is not in the build manifest; run npm run build", path)
		},
	}
}

func (s *Server) headlessAsset(path string) bool {
	if path == "/static/style.css" || path == "/static/invitation.mjs" || path == "/static/connect-ai.mjs" || strings.HasPrefix(path, "/static/brand/") || strings.HasPrefix(path, "/static/fonts/") {
		return true
	}
	for _, url := range s.browserAssets.Headless {
		if path == url {
			return true
		}
	}
	return false
}
