package chat

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"forgetmenote/internal/durable"
	"forgetmenote/internal/sources"
	"forgetmenote/internal/workspace"
	"golang.org/x/net/html"
)

// WebsiteCaptureConfig is operator-owned. Packages cannot supply credentials,
// request headers, browser scripts, or a different service endpoint.
type WebsiteCaptureConfig struct {
	AccountIDFile string `json:"accountIDFile"`
	SecretFile    string `json:"secretFile"`
}

func (c WebsiteCaptureConfig) Validate() error {
	if !filepath.IsAbs(c.AccountIDFile) || !filepath.IsAbs(c.SecretFile) {
		return errors.New("website capture requires a Cloudflare absolute account ID and token files")
	}
	return nil
}

type WebsiteCapture struct {
	client               *http.Client
	endpoint, credential string
	fetch                func(context.Context, string) ([]byte, string, string, error)
}

type WebsiteEvidence struct {
	Text           string           `json:"text"`
	Images         []string         `json:"images"`
	Record         WebsiteRecord    `json:"record"`
	Sections       []WebsiteSection `json:"sections,omitempty"`
	PairedSections []WebsiteSection `json:"pairedSections,omitempty"`
}

// Sections are overlapping, unscaled regions of the same captured PNG. The
// original remains authoritative; these let vision providers see page detail.
type WebsiteSection struct {
	Image  string `json:"image"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// PNG work reads/writes bounded chunks through the capture's cancellation scope.
type websiteImageBuffer struct {
	bytes.Buffer
	ctx context.Context
}

func (b *websiteImageBuffer) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	return b.Buffer.Read(p)
}
func (b *websiteImageBuffer) Write(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	return b.Buffer.Write(p)
}

func websiteSections(ctx context.Context, raw []byte, width, height int) ([]WebsiteSection, error) {
	return websiteSectionsCount(ctx, raw, width, height, 0)
}
func websiteSectionsCount(ctx context.Context, raw []byte, width, height, requested int) ([]WebsiteSection, error) {
	if height <= 2*width || ctx.Err() != nil {
		return nil, nil
	}
	// Optional regions are useful only when each fits Clef's documented 16MP
	// image limit. This is not a limit on the authoritative capture.
	overlap := min(64, height/12)
	// Prefer the smallest complete set whose regions are about two widths tall.
	// Three regions plus the creative fit Clef's four-image request limit.
	count := min(3, (height+2*width-1)/(2*width))
	// Clef reduces each image above roughly one megapixel. Use the third
	// available slot when it preserves more detail without omitting page rows.
	count = max(count, min(3, int((int64(width)*int64(height)+999999)/1000000)))
	if requested == 2 {
		count = 2
	}
	if count == 2 && requested == 0 && int64(width)*int64((height+1)/2+2*overlap) > 16_000_000 {
		count = 3
	}
	if int64(width)*int64((height+count-1)/count+2*overlap) > 16_000_000 {
		return nil, nil
	}
	input := &websiteImageBuffer{Buffer: *bytes.NewBuffer(raw), ctx: ctx}
	original, err := png.Decode(input)
	if err != nil {
		return nil, err
	}
	sections := make([]WebsiteSection, 0, count)
	// No page rows are removed and the authoritative PNG is unchanged.
	for i := 0; i < count; i++ {
		start, end := i*height/count, (i+1)*height/count
		if i > 0 {
			start -= overlap
		}
		if i < count-1 {
			end += overlap
		}
		region := image.Rect(0, start, width, end)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		crop := original.(interface {
			SubImage(image.Rectangle) image.Image
		}).SubImage(region)
		encoded := websiteImageBuffer{ctx: ctx}
		if err = png.Encode(&encoded, crop); err != nil {
			return nil, err
		}
		if encoded.Len() > 4<<20 {
			return nil, nil
		}
		inline, err := InlineImage(encoded.Bytes())
		if err != nil {
			return nil, err
		}
		sections = append(sections, WebsiteSection{Image: inline, Y: start, Width: width, Height: end - start})
	}
	total := 0
	for _, section := range sections {
		total += len(section.Image) * 3 / 4
	}
	if total > 8<<20 {
		return nil, nil
	}
	return sections, nil
}

type WebsiteRecord struct {
	URL        string          `json:"url"`
	FinalURL   string          `json:"finalURL"`
	Navigation []string        `json:"navigation"`
	Method     string          `json:"method"`
	CapturedAt string          `json:"capturedAt"`
	SHA256     string          `json:"sha256"`
	Width      int             `json:"width"`
	Height     int             `json:"height"`
	Warning    string          `json:"warning"`
	Loading    *WebsiteLoading `json:"loading,omitempty"`
}

//go:embed website_load.js
var websiteLoadScript string

// Loading observations are page evidence, never proof of semantic coverage.
type WebsiteLoading struct {
	Version               int  `json:"version"`
	ReachedBottom         bool `json:"reachedBottom"`
	TimedOut              bool `json:"timedOut"`
	Error                 bool `json:"error"`
	PendingImages         int  `json:"pendingImages"`
	BrokenImages          int  `json:"brokenImages"`
	FontsReady            bool `json:"fontsReady"`
	DocumentWidth         int  `json:"documentWidth"`
	DocumentHeight        int  `json:"documentHeight"`
	ScreenshotClipped     bool `json:"screenshotClipped"`
	DismissedOverlays     int  `json:"dismissedOverlays"`
	FrameCount            *int `json:"frameCount,omitempty"`
	PendingFrames         *int `json:"pendingFrames,omitempty"`
	UnobservedFrames      *int `json:"unobservedFrames,omitempty"`
	VideoCount            *int `json:"videoCount,omitempty"`
	PendingVideos         *int `json:"pendingVideos,omitempty"`
	EmbedPlaceholderCount *int `json:"embedPlaceholderCount,omitempty"`
}

func websiteLoading(content, marker string, width, height int) *WebsiteLoading {
	failed := &WebsiteLoading{Version: 1, Error: true}
	// The script prepends the report to the head. Do not scan arbitrary page
	// bodies looking for it; a missing report is an incomplete observation.
	t := html.NewTokenizer(io.LimitReader(strings.NewReader(content), 64<<10))
	t.SetMaxBuf(16 << 10)
	for {
		kind := t.Next()
		if kind == html.ErrorToken {
			return failed
		}
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			continue
		}
		token := t.Token()
		if token.Data != "meta" {
			continue
		}
		name, value := "", ""
		for _, a := range token.Attr {
			if a.Key == "name" {
				name = a.Val
			}
			if a.Key == "content" {
				value = a.Val
			}
		}
		if name != marker {
			continue
		}
		var load WebsiteLoading
		var fields map[string]json.RawMessage
		if len(value) > 4096 || json.Unmarshal([]byte(value), &fields) != nil || (len(fields) != 9 && len(fields) != 10 && len(fields) != 15 && len(fields) != 16) {
			return failed
		}
		if len(fields) == 10 && fields["dismissedOverlays"] == nil {
			return failed
		}
		if raw, ok := fields["dismissedOverlays"]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return failed
		}
		for _, key := range []string{"version", "reachedBottom", "timedOut", "error", "pendingImages", "brokenImages", "fontsReady", "documentWidth", "documentHeight"} {
			v, ok := fields[key]
			if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				return failed
			}
		}
		if json.Unmarshal([]byte(value), &load) != nil || (load.Version != 1 && load.Version != 2 && load.Version != 3) || load.PendingImages < 0 || load.BrokenImages < 0 || load.DocumentWidth < 1 || load.DocumentHeight < 1 || load.DismissedOverlays < 0 || load.DismissedOverlays > 3 {
			return failed
		}
		if load.Version == 1 && len(fields) != 9 && len(fields) != 10 {
			return failed
		}
		if load.Version >= 2 && (len(fields) != 13+load.Version || fields["dismissedOverlays"] == nil || load.UnobservedFrames == nil || load.FrameCount == nil || load.PendingFrames == nil || load.VideoCount == nil || load.PendingVideos == nil || *load.FrameCount < 0 || *load.PendingFrames < 0 || *load.PendingFrames > *load.FrameCount || *load.UnobservedFrames < 0 || *load.UnobservedFrames+*load.PendingFrames > *load.FrameCount || *load.VideoCount < 0 || *load.PendingVideos < 0 || *load.PendingVideos > *load.VideoCount) {
			return failed
		}
		if load.Version == 3 && (load.EmbedPlaceholderCount == nil || *load.EmbedPlaceholderCount < 0) {
			return failed
		}
		load.ScreenshotClipped = width < load.DocumentWidth || height < load.DocumentHeight
		return &load
	}
}

func NewWebsiteCapture(c WebsiteCaptureConfig) (*WebsiteCapture, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	account, err := durable.ReadSecret(c.AccountIDFile, 128)
	if errors.Is(err, durable.ErrOpen) {
		return nil, errors.New("cannot read website capture account ID")
	}
	if err != nil || !regexp.MustCompile(`^[a-fA-F0-9]{32}$`).MatchString(account) {
		return nil, errors.New("invalid website capture account ID file")
	}
	token, err := durable.ReadSecret(c.SecretFile, 4096)
	switch {
	case errors.Is(err, durable.ErrOpen):
		return nil, errors.New("cannot read website capture credential")
	case errors.Is(err, durable.ErrNotRegular):
		return nil, errors.New("invalid website capture credential file")
	case err != nil:
		return nil, errors.New("invalid website capture credential")
	}
	// The request includes navigation (up to 45s) and settled-page rendering
	// (up to 60s). The HTTP deadline must allow both provider phases.
	tr := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 8 * time.Second}).DialContext, TLSHandshakeTimeout: 8 * time.Second, ResponseHeaderTimeout: 120 * time.Second, MaxResponseHeaderBytes: 64 << 10, DisableKeepAlives: true}
	return &WebsiteCapture{client: &http.Client{Transport: tr, Timeout: 120 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, endpoint: "https://api.cloudflare.com/client/v4/accounts/" + account + "/browser-run/snapshot", credential: token, fetch: sources.Fetch}, nil
}

// frameDestination unwraps only a frameset with one frame and no visible body
// text. Ordinary embedded videos, advertising and multiple frames stay in place.
func frameDestination(ctx context.Context, data []byte, base string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Frameset wrappers are tiny documents. Large real pages are not parsed
	// locally to discover arbitrary embeds; Cloudflare owns their rendering.
	if len(data) > 16384 {
		return "", nil
	}
	tokenizer := html.NewTokenizer(bytes.NewReader(data))
	tokenizer.SetMaxBuf(16384)
	var frames []string
	skip := 0
	hasFrameset, visible := false, false
	for tokens := 0; ; tokens++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if tokens > 4096 {
			return "", errors.New("website wrapper exceeds token limit")
		}
		kind := tokenizer.Next()
		if kind == html.ErrorToken {
			if tokenizer.Err() != io.EOF {
				return "", errors.New("invalid website wrapper HTML")
			}
			break
		}
		token := tokenizer.Token()
		ignored := token.Data == "head" || token.Data == "script" || token.Data == "style" || token.Data == "noframes"
		if kind == html.StartTagToken && ignored {
			skip++
		}
		if kind == html.EndTagToken && ignored {
			skip = max(0, skip-1)
		}
		if skip != 0 {
			continue
		}
		if kind == html.StartTagToken && token.Data == "frameset" {
			hasFrameset = true
		}
		if (kind == html.StartTagToken || kind == html.SelfClosingTagToken) && token.Data == "frame" {
			for _, a := range token.Attr {
				if a.Key == "src" {
					frames = append(frames, a.Val)
				}
			}
		}
		if kind == html.TextToken && strings.TrimSpace(token.Data) != "" {
			visible = true
		}
	}
	if !hasFrameset || visible || len(frames) != 1 {
		return "", nil
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	v, err := url.Parse(frames[0])
	if err != nil {
		return "", err
	}
	target := u.ResolveReference(v).String()
	return target, sources.ValidateURL(target)
}

var ErrWebsiteCaptureNotAttempted = errors.New("website capture request was not attempted")
var ErrWebsiteCaptureSettled = errors.New("website capture returned a definitive response")

func (c *WebsiteCapture) Capture(ctx context.Context, advertised string) (WebsiteEvidence, error) {
	return c.capture(ctx, advertised, false)
}

// A separately receipted retry may close ordinary promotions after the original
// public view has been retained. Access and age gates are never opened.
func (c *WebsiteCapture) CaptureAfterDismissingPromotions(ctx context.Context, advertised string) (WebsiteEvidence, error) {
	return c.capture(ctx, advertised, true)
}

func (c *WebsiteCapture) capture(ctx context.Context, advertised string, dismiss bool) (out WebsiteEvidence, captureErr error) {
	attempted := false
	responded := false
	defer func() {
		if captureErr != nil && !attempted {
			captureErr = fmt.Errorf("%w: %w", ErrWebsiteCaptureNotAttempted, captureErr)
		} else if captureErr != nil && responded {
			captureErr = fmt.Errorf("%w: %w", ErrWebsiteCaptureSettled, captureErr)
		}
	}()
	if c == nil {
		return out, errors.New("Cloudflare website capture is not configured")
	}
	if err := sources.ValidateURL(advertised); err != nil {
		return out, err
	}
	navigation := []string{advertised}
	target := advertised
	// The bounded static preflight also applies FMN's DNS/redirect validation.
	for depth := 0; depth < 4; depth++ {
		raw, typ, final, err := c.fetch(ctx, target)
		if err != nil {
			var denied *sources.HTTPStatusError
			if !errors.As(err, &denied) || denied.StatusCode != http.StatusUnauthorized && denied.StatusCode != http.StatusForbidden {
				return out, err
			}
			// A public server's denial of this static client is not a browser
			// capture result. The shared transport already vetted its final URL.
			if err := sources.ValidateURL(denied.URL); err != nil {
				return out, err
			}
			if denied.URL != target {
				navigation = append(navigation, denied.URL)
			}
			target = denied.URL
			break
		}
		if final != target {
			navigation = append(navigation, final)
		}
		target = final
		if !strings.HasPrefix(typ, "text/html") {
			return out, fmt.Errorf("website capture requires HTML (content type %s)", strings.Map(func(r rune) rune {
				if r >= 32 && r < 127 {
					return r
				}
				return -1
			}, typ[:min(len(typ), 128)]))
		}
		next, err := frameDestination(ctx, raw, target)
		if err != nil {
			return out, err
		}
		if next == "" {
			break
		}
		if depth == 3 {
			return out, errors.New("website frame navigation limit reached")
		}
		for _, seen := range navigation {
			if next == seen {
				return out, errors.New("website frame navigation loop")
			}
		}
		navigation = append(navigation, next)
		target = next
	}
	var nonce [16]byte
	rand.Read(nonce[:])
	marker := "fmn-capture-" + hex.EncodeToString(nonce[:])
	body, _ := json.Marshal(map[string]any{
		"url": target, "formats": []string{"content", "screenshot"},
		"viewport":          map[string]int{"width": 1280, "height": 900},
		"screenshotOptions": map[string]any{"fullPage": true, "type": "png"},
		"gotoOptions":       map[string]any{"waitUntil": "networkidle2", "timeout": 45000},
		"addScriptTag":      []map[string]string{{"content": strings.ReplaceAll(strings.ReplaceAll(websiteLoadScript, "__FMN_CAPTURE_TOKEN__", marker), "__FMN_ALLOW_PROMOTION_DISMISSAL__", fmt.Sprint(dismiss))}},
		"waitForSelector":   map[string]any{"selector": `meta[name="` + marker + `"]`, "timeout": 10000},
		"actionTimeout":     60000,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", c.endpoint, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.credential)
	req.Header.Set("Content-Type", "application/json")
	attempted = true
	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		return out, errors.New("Cloudflare website capture is unavailable")
	}
	defer resp.Body.Close()
	responded = true
	if resp.StatusCode != 200 {
		// Retain only the numeric provider code, never error prose or headers.
		var rejected struct {
			Errors []struct {
				Code int `json:"code"`
			} `json:"errors"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		code := 0
		if json.Unmarshal(raw, &rejected) == nil && len(rejected.Errors) == 1 {
			code = rejected.Errors[0].Code
		}
		return out, fmt.Errorf("Cloudflare website capture rejected request (HTTP %d, code %d)", resp.StatusCode, code)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<30)+1))
	if err != nil || len(raw) > 1<<30 {
		return out, errors.New("website capture response exceeds limits or was interrupted")
	}
	var envelope struct {
		Success bool `json:"success"`
		Result  struct {
			Content    string `json:"content"`
			Screenshot string `json:"screenshot"`
		} `json:"result"`
		Meta struct {
			FinalURL  string `json:"finalUrl"`
			Status    int    `json:"status"`
			Redirects []struct {
				URL string `json:"url"`
			} `json:"redirectChain"`
		} `json:"meta"`
	}
	if json.Unmarshal(raw, &envelope) != nil || !envelope.Success || envelope.Meta.Status != 200 {
		return out, errors.New("website capture did not establish a successful landing page")
	}
	final := envelope.Meta.FinalURL
	if final == "" {
		return out, errors.New("website capture omitted final URL provenance")
	}
	if err := sources.ValidateURL(final); err != nil {
		return out, err
	}
	for _, r := range envelope.Meta.Redirects {
		if err := sources.ValidateURL(r.URL); err != nil {
			return out, err
		}
		navigation = append(navigation, r.URL)
	}
	if navigation[len(navigation)-1] != final {
		navigation = append(navigation, final)
	}
	screenshot, err := base64.StdEncoding.Strict().DecodeString(envelope.Result.Screenshot)
	if err != nil {
		return out, errors.New("invalid website screenshot")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(screenshot))
	if err != nil || format != "png" || cfg.Width < 1 || cfg.Height < 1 {
		return out, errors.New("invalid website screenshot")
	}
	// Preserve Cloudflare's complete PNG; downstream provider limits still apply.
	inline, err := InlineImage(screenshot)
	if err != nil {
		return out, err
	}
	out.Images = []string{inline}
	// Section preparation is optional: preserve a settled, successful capture
	// even when its image cannot be sectioned for this provider.
	out.Sections, _ = websiteSections(ctx, screenshot, cfg.Width, cfg.Height)
	out.PairedSections, _ = websiteSectionsCount(ctx, screenshot, cfg.Width, cfg.Height, 2)
	if strings.TrimSpace(envelope.Result.Content) == "" {
		return WebsiteEvidence{}, errors.New("website capture omitted rendered HTML")
	}
	capture, err := sources.Prepare(ctx, "landing.html", final, []byte(envelope.Result.Content))
	if err != nil {
		return WebsiteEvidence{}, err
	}
	out.Text = capture.Text
	out.Record = WebsiteRecord{URL: advertised, FinalURL: final, Navigation: navigation, Method: "cloudflare-browser-run", CapturedAt: time.Now().UTC().Format(time.RFC3339Nano), SHA256: workspace.ContentHash(screenshot), Width: cfg.Width, Height: cfg.Height, Loading: websiteLoading(envelope.Result.Content, marker, cfg.Width, cfg.Height), Warning: "Full-page visual capture after bounded public-page scrolling and image/font settling. Loading observations do not prove semantic coverage; overlays, frames, video and responsive variants may remain incomplete. No gates were crossed."}
	return out, nil
}
