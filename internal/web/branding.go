package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"sync"
	"time"

	"forgetmenote/internal/branding"
	"forgetmenote/internal/catalog"
	"forgetmenote/plugins"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type instanceBrand struct {
	Name   string `json:"name"`
	Icon   string `json:"icon"`
	Custom bool   `json:"custom"`
	// version fingerprints a custom logo for the URLs derived from it.
	version string
}

func (s *Server) mcpIdentity() *mcp.Implementation {
	brand := currentBrand(s.catalog)
	mime := "image/svg+xml"
	if brand.Custom {
		mime = "image/png"
	}
	return &mcp.Implementation{Name: plugins.MCPName(brand.Name), Title: brand.Name, Version: "0.1.0", WebsiteURL: s.origin, Icons: []mcp.Icon{{Source: s.origin + brand.Icon, MIMEType: mime}}}
}

// The SDK retains startup metadata. Legacy initialize replies and current
// protocol result metadata must both reflect administrator changes without
// mutating the SDK's shared implementation pointer.
func (s *Server) brandMCPResults(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, req)
		if err != nil || result == nil {
			return result, err
		}
		switch method {
		case "ping", "logging/setLevel", "resources/subscribe", "resources/unsubscribe":
			// These legacy methods return the SDK's empty result, which cannot
			// carry metadata. Preserve its response instead of calling GetMeta.
			return result, nil
		}
		info := s.mcpIdentity()
		if init, ok := result.(*mcp.InitializeResult); ok {
			response := *init
			response.ServerInfo = info
			result = &response
		}
		meta := maps.Clone(result.GetMeta())
		if meta == nil {
			meta = map[string]any{}
		}
		meta[mcp.MetaKeyServerInfo] = info
		result.SetMeta(meta)
		return result, nil
	}
}

// currentBrand runs for every page and MCP result; the catalog's logo version
// spares it copying and hashing the logo.
func currentBrand(projects *catalog.Catalog) instanceBrand {
	brand := instanceBrand{Name: projects.InstanceName(), Icon: "/static/brand/flower.svg"}
	if version := projects.InstanceLogoVersion(); version != "" {
		brand.Custom = true
		brand.version = version
		brand.Icon = "/static/brand/instance.png?v=" + brand.version
	}
	return brand
}

var defaultIconTag = fmt.Sprintf(`"%x"`, sha256.Sum256(branding.DefaultIcon()))

func (s *Server) instanceIcon(w http.ResponseWriter, r *http.Request) {
	// The logo version is its SHA-256. Only a logo that changes while it is
	// read is hashed here, so the tag always describes the bytes served.
	version := s.catalog.InstanceLogoVersion()
	logo, tag := s.catalog.InstanceLogo(), defaultIconTag
	if len(logo) == 0 {
		if r.URL.Path == "/favicon.ico" {
			http.ServeFileFS(w, r, branding.Files(), "favicon.ico")
			return
		}
		logo = branding.DefaultIcon()
	} else if version != "" && version == s.catalog.InstanceLogoVersion() {
		tag = `"` + version + `"`
	} else {
		tag = fmt.Sprintf(`"%x"`, sha256.Sum256(logo))
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, no-cache")
	w.Header().Set("ETag", tag)
	http.ServeContent(w, r, "icon.png", time.Time{}, bytes.NewReader(logo))
}

// Installed-app icons. The default brand's are committed; a custom logo's are
// derived from it on first request and served while it remains current.
var appIcons = []struct {
	size     int
	maskable bool
	file     string // default brand asset
	custom   string // path derived from the custom logo
}{
	{192, false, "app-icon-192.png", "instance-192.png"},
	{512, false, "app-icon-512.png", "instance-512.png"},
	{512, true, "app-icon-maskable-512.png", "instance-maskable-512.png"},
}

// The installed window's background and title bar use the light theme's page
// color (--bg in styles/base.css); pages override it for the active theme.
const appBackground = "#faf9f5"

type manifestIcon struct {
	Src     string `json:"src"`
	Sizes   string `json:"sizes"`
	Type    string `json:"type"`
	Purpose string `json:"purpose"`
}

// webManifest describes the instance as an installable app. It is public, like
// the sign-in page's name and icons, because browsers fetch it without
// credentials, and it is not stored because administrators can rebrand.
func (s *Server) webManifest(w http.ResponseWriter, r *http.Request) {
	brand := currentBrand(s.catalog)
	icons := make([]manifestIcon, 0, len(appIcons))
	for _, icon := range appIcons {
		src := "/static/brand/" + icon.file
		if brand.Custom {
			src = "/static/brand/" + icon.custom + "?v=" + brand.version
		}
		purpose := "any"
		if icon.maskable {
			purpose = "maskable"
		}
		sizes := strconv.Itoa(icon.size)
		icons = append(icons, manifestIcon{Src: src, Sizes: sizes + "x" + sizes, Type: "image/png", Purpose: purpose})
	}
	body, err := json.Marshal(map[string]any{
		"id": "/", "name": brand.Name, "short_name": brand.Name,
		"start_url": "/", "scope": "/", "display": "standalone",
		"background_color": appBackground, "theme_color": appBackground,
		"icons": icons,
	})
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(body)
}

// appIconCache keeps the icons derived from the current custom logo, so
// launcher requests do not each decode and resample an image.
type appIconCache struct {
	mu    sync.Mutex
	logo  [sha256.Size]byte
	icons map[string][]byte
}

func (c *appIconCache) get(logo []byte, name string, size int, maskable bool) ([]byte, error) {
	sum := sha256.Sum256(logo)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.logo != sum || c.icons == nil {
		c.logo, c.icons = sum, map[string][]byte{}
	}
	if icon, ok := c.icons[name]; ok {
		return icon, nil
	}
	icon, err := branding.AppIcon(logo, size, maskable)
	if err != nil {
		return nil, err
	}
	c.icons[name] = icon
	return icon, nil
}

func (s *Server) appIcon(size int, maskable bool, file, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		logo := s.catalog.InstanceLogo()
		if len(logo) == 0 {
			// A manifest from before the logo was removed still gets an icon.
			http.ServeFileFS(w, r, branding.Files(), file)
			return
		}
		icon, err := s.customIcons.get(logo, name, size, maskable)
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, no-cache")
		w.Header().Set("ETag", fmt.Sprintf(`"%x"`, sha256.Sum256(icon)))
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(icon))
	}
}
