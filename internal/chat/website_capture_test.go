package chat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/sources"
)

func TestWebsiteCaptureUnwrapsFrameAndPreservesVisualEvidence(t *testing.T) {
	var seen string
	var picture bytes.Buffer
	// One palette colour keeps this tall page cheap to decode and section under -race.
	png.Encode(&picture, image.NewPaletted(image.Rect(0, 0, 2100, 15001), color.Palette{color.White}))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Error("missing service credential")
		}
		var request map[string]any
		json.NewDecoder(r.Body).Decode(&request)
		seen = request["url"].(string)
		if request["cookies"] != nil || request["setExtraHTTPHeaders"] != nil {
			t.Error("destination credential supplied")
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "meta": map[string]any{"status": 200, "finalUrl": seen}, "result": map[string]string{"content": "<html><body><h1>Sign maker</h1><p>Vehicle wraps</p></body></html>", "screenshot": base64.StdEncoding.EncodeToString(picture.Bytes())}})
	}))
	defer server.Close()
	capture := &WebsiteCapture{client: server.Client(), endpoint: server.URL, credential: "synthetic-token", fetch: func(ctx context.Context, u string) ([]byte, string, string, error) {
		if u == "https://signs.example.org/" {
			return []byte(`<html><head><title>Signs</title></head><frameset><frame src="https://signs.example.net/landing"></frameset></html>`), "text/html", u, nil
		}
		return []byte(`<html><body>Signs</body></html>`), "text/html", u, nil
	}}
	result, err := capture.Capture(t.Context(), "https://signs.example.org/")
	if err != nil {
		t.Fatal(err)
	}
	if seen != "https://signs.example.net/landing" || result.Record.URL != "https://signs.example.org/" || len(result.Record.Navigation) != 2 {
		t.Fatalf("lost original-to-frame provenance: %+v", result.Record)
	}
	if result.Record.Width != 2100 || result.Record.Height != 15001 || len(result.Images) != 1 || !strings.Contains(result.Text, "Vehicle wraps") || result.Record.SHA256 == "" {
		t.Fatal("missing visual/text evidence")
	}
	if result.Images[0] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(picture.Bytes()) {
		t.Fatal("full screenshot bytes changed")
	}
	for _, img := range result.Images {
		if err := validateImages(Message{Role: "user", Images: []string{img}}, Connection{Format: "responses", Capabilities: Capabilities{Images: true}}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWebsiteCaptureRejectsInvalidAndIncompleteResponses(t *testing.T) {
	for _, test := range []struct {
		name     string
		response any
		status   int
	}{
		{"failure", map[string]any{"success": false}, 200},
		{"missing provenance", map[string]any{"success": true, "meta": map[string]any{"status": 200}, "result": map[string]string{"screenshot": "bad"}}, 200},
		{"blocked", map[string]any{"success": true, "meta": map[string]any{"status": 403, "finalUrl": "https://example.org/"}}, 200},
		{"private final", map[string]any{"success": true, "meta": map[string]any{"status": 200, "finalUrl": "http://127.0.0.1/"}}, 200},
		{"service failure", map[string]string{"error": "synthetic-token"}, 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(test.status)
				json.NewEncoder(w).Encode(test.response)
			}))
			defer s.Close()
			c := &WebsiteCapture{client: s.Client(), endpoint: s.URL, credential: "synthetic-token", fetch: func(ctx context.Context, u string) ([]byte, string, string, error) {
				return []byte("<body>Landing</body>"), "text/html", u, nil
			}}
			_, err := c.Capture(t.Context(), "https://example.org/")
			if err == nil || strings.Contains(err.Error(), "synthetic-token") || calls != 1 {
				t.Fatalf("unsafe response handling: %v, calls %d", err, calls)
			}
		})
	}
}
func TestWebsiteFrameDestinationRejectsAmbiguousAndPrivateFrames(t *testing.T) {
	for _, markup := range []string{`<body><iframe src="https://video.example.org"></iframe></body>`, `<frameset><frame src="https://a.example.org"><frame src="https://b.example.org"></frameset>`} {
		u, err := frameDestination(t.Context(), []byte(markup), "https://example.org/")
		if err != nil || u != "" {
			t.Fatal("ordinary embed treated as landing")
		}
	}
	if _, err := frameDestination(t.Context(), []byte(`<frameset><frame src="http://127.0.0.1/"></frameset>`), "https://example.org/"); err == nil {
		t.Fatal("private frame accepted")
	}
	var c *WebsiteCapture
	if _, err := c.Capture(t.Context(), "https://example.org"); err == nil {
		t.Fatal("disabled capture accepted")
	}
}

func TestWebsiteCaptureConfigurationKeepsSecretsOperatorOwned(t *testing.T) {
	c := WebsiteCaptureConfig{AccountIDFile: filepath.Join(t.TempDir(), "account"), SecretFile: filepath.Join(t.TempDir(), "token")}
	os.WriteFile(c.AccountIDFile, []byte(strings.Repeat("a", 32)), 0600)
	os.WriteFile(c.SecretFile, []byte("synthetic-token"), 0600)
	client, err := NewWebsiteCapture(c)
	if err != nil {
		t.Fatal(err)
	}
	if client.endpoint != "https://api.cloudflare.com/client/v4/accounts/"+strings.Repeat("a", 32)+"/browser-run/snapshot" {
		t.Fatal("wrong configured account")
	}
	config := &Config{LocalOnly: true, WebsiteCapture: &c, Connections: []Connection{{ID: "local", Label: "Local", Format: "responses", BaseURL: "http://localhost:1234/v1", Models: []string{"gpt-6-luna"}, DefaultModel: "gpt-6-luna", Local: true}}}
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "Cloudflare website capture") {
		t.Fatalf("local-only capture guard did not run: %v", err)
	}
	os.WriteFile(c.SecretFile, []byte("synthetic\ncredential"), 0600)
	if _, err := NewWebsiteCapture(c); err == nil {
		t.Fatal("multiline credential accepted")
	}
}

func TestWebsiteWrapperScanIsBoundedAndCancelable(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := frameDestination(ctx, []byte("<frameset></frameset>"), "https://example.org/"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ignored deadline: %v", err)
	}
	if target, err := frameDestination(t.Context(), []byte(strings.Repeat("<div>", 1000000)), "https://example.org/"); err != nil || target != "" {
		t.Fatal("large page parsed as wrapper")
	}
}

func TestWebsiteCaptureMissingHTMLDoesNotRefetchStaticContent(t *testing.T) {
	var picture bytes.Buffer
	png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 1280, 900)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"success": true, "meta": map[string]any{"status": 200, "finalUrl": "https://example.org/"}, "result": map[string]string{"screenshot": base64.StdEncoding.EncodeToString(picture.Bytes())}})
	}))
	defer server.Close()
	preflights := 0
	c := &WebsiteCapture{client: server.Client(), endpoint: server.URL, fetch: func(ctx context.Context, u string) ([]byte, string, string, error) {
		preflights++
		return []byte("<body>Original</body>"), "text/html", u, nil
	}}
	_, err := c.Capture(t.Context(), "https://example.org/")
	if err == nil || !errors.Is(err, ErrWebsiteCaptureSettled) || preflights != 1 {
		t.Fatalf("missing rendered content silently refetched: %v, preflights %d", err, preflights)
	}
}

func TestWebsiteCaptureUsesBrowserWhenStaticClientIsDenied(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var picture bytes.Buffer
			png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 2, 2)))
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var request struct {
					URL string `json:"url"`
				}
				json.NewDecoder(r.Body).Decode(&request)
				if request.URL != "https://www.example.org/" {
					t.Errorf("lost validated final URL: %s", request.URL)
				}
				json.NewEncoder(w).Encode(map[string]any{"success": true, "meta": map[string]any{"status": 200, "finalUrl": request.URL}, "result": map[string]string{"content": "<html><body>Public landing page</body></html>", "screenshot": base64.StdEncoding.EncodeToString(picture.Bytes())}})
			}))
			defer server.Close()
			capture := &WebsiteCapture{client: server.Client(), endpoint: server.URL, fetch: func(context.Context, string) ([]byte, string, string, error) {
				return nil, "", "", &sources.HTTPStatusError{StatusCode: status, URL: "https://www.example.org/"}
			}}
			out, err := capture.Capture(t.Context(), "https://example.org/")
			if err != nil || calls != 1 || len(out.Images) != 1 || out.Record.FinalURL != "https://www.example.org/" || len(out.Record.Navigation) != 2 {
				t.Fatalf("static denial blocked browser capture: calls=%d record=%+v err=%v", calls, out.Record, err)
			}
		})
	}
}

func TestWebsiteCaptureStaticDenialDoesNotBypassSecurityOrProviderFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		preflight error
		wantCalls int
		settled   bool
	}{
		{"security", errors.New("source capture cannot access private or special-use addresses"), 0, false},
		{"untyped denial", errors.New("source returned HTTP 403; nothing was archived"), 0, false},
		{"private final", &sources.HTTPStatusError{StatusCode: 403, URL: "http://127.0.0.1/"}, 0, false},
		{"not found", &sources.HTTPStatusError{StatusCode: 404, URL: "https://example.org/"}, 0, false},
		{"browser also denied", &sources.HTTPStatusError{StatusCode: 403, URL: "https://example.org/"}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				json.NewEncoder(w).Encode(map[string]any{"success": true, "meta": map[string]any{"status": 403, "finalUrl": "https://example.org/"}})
			}))
			defer server.Close()
			capture := &WebsiteCapture{client: server.Client(), endpoint: server.URL, fetch: func(context.Context, string) ([]byte, string, string, error) { return nil, "", "", tc.preflight }}
			_, err := capture.Capture(t.Context(), "https://example.org/")
			if err == nil || calls != tc.wantCalls || errors.Is(err, ErrWebsiteCaptureSettled) != tc.settled || errors.Is(err, ErrWebsiteCaptureNotAttempted) == tc.settled {
				t.Fatalf("unsafe denial fallback: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestWebsiteCaptureWaitsForLoadingReportAndRetainsDiagnostics(t *testing.T) {
	var picture bytes.Buffer
	png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 1280, 1800)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		json.NewDecoder(r.Body).Decode(&request)
		// A valid, scripted page may need more than the old 15-second action
		// window even after navigation has settled (production reproduction).
		if request["actionTimeout"] != float64(60000) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"code": 6002}}})
			return
		}

		wait, ok := request["waitForSelector"].(map[string]any)
		if !ok || wait["selector"] == nil {
			t.Error("capture did not wait for bounded loading completion")
		}
		marker := "missing"
		if ok {
			marker = strings.TrimSuffix(strings.TrimPrefix(wait["selector"].(string), `meta[name="`), `"]`)
		}
		report := `{"version":1,"reachedBottom":false,"timedOut":true,"error":false,"pendingImages":2,"brokenImages":1,"fontsReady":true,"documentWidth":1280,"documentHeight":2400}`
		content := `<html><head><meta name="` + marker + `" content='` + report + `'></head><body>Original landing text</body></html>`
		json.NewEncoder(w).Encode(map[string]any{"success": true, "meta": map[string]any{"status": 200, "finalUrl": "https://example.org/"}, "result": map[string]string{"content": content, "screenshot": base64.StdEncoding.EncodeToString(picture.Bytes())}})
	}))
	defer server.Close()
	c := &WebsiteCapture{client: server.Client(), endpoint: server.URL, fetch: func(context.Context, string) ([]byte, string, string, error) {
		return []byte("<body>Landing</body>"), "text/html", "https://example.org/", nil
	}}
	out, err := c.Capture(t.Context(), "https://example.org/")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out.Record)
	var record map[string]any
	json.Unmarshal(raw, &record)
	load, ok := record["loading"].(map[string]any)
	if !ok || load["reachedBottom"] != false || load["timedOut"] != true || load["pendingImages"] != float64(2) || load["brokenImages"] != float64(1) || load["screenshotClipped"] != true {
		t.Fatalf("lost known loading failures: %s", raw)
	}
	if out.Images[0] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(picture.Bytes()) || !strings.Contains(out.Text, "Original landing text") {
		t.Fatal("loading diagnostics displaced original evidence")
	}
}

func TestWebsiteLoadingRequiresCompleteTypedReport(t *testing.T) {
	valid := `{"version":1,"reachedBottom":true,"timedOut":false,"error":false,"pendingImages":0,"brokenImages":0,"fontsReady":true,"documentWidth":1280,"documentHeight":1800}`
	for _, raw := range []string{
		`{"version":1,"reachedBottom":true,"documentWidth":1280,"documentHeight":1800}`,
		strings.Replace(valid, `"error":false`, `"error":null`, 1),
		strings.Replace(valid, `"pendingImages":0`, `"pendingImages":null`, 1),
		strings.Replace(valid, `"timedOut":false`, `"timedOut":"false"`, 1),
		strings.Replace(valid, `"version":1`, `"version":3`, 1),
		`{broken`,
	} {
		out := websiteLoading(`<head><meta name="fixture" content='`+raw+`'></head>`, "fixture", 1280, 1800)
		if out == nil || !out.Error {
			t.Fatalf("invalid loading report accepted: %s, %+v", raw, out)
		}
	}
	for _, content := range []string{`<body>No report</body>`, `<meta name="wrong" content='` + valid + `'>`, strings.Repeat(" ", 70000) + `<meta name="fixture" content='` + valid + `'>`} {
		out := websiteLoading(content, "fixture", 1280, 1800)
		if out == nil || !out.Error {
			t.Fatal("missing or out-of-bounds report accepted")
		}
	}
	if out := websiteLoading(`<meta name="fixture" content='`+valid+`'>`, "fixture", 1280, 1800); out.Error || out.ScreenshotClipped || !out.ReachedBottom {
		t.Fatal("valid loading report lost", out)
	}
}

func TestWebsiteCaptureSectionsCoverOriginalWithoutChangingPNG(t *testing.T) {
	var picture bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 320, 1600))
	for y := 0; y < 1600; y++ {
		for x := 0; x < 320; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(y % 251), G: uint8(x % 251), A: 255})
		}
	}
	if err := png.Encode(&picture, img); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"success": true, "meta": map[string]any{"status": 200, "finalUrl": "https://example.org/"}, "result": map[string]string{"content": "<body>Public original</body>", "screenshot": base64.StdEncoding.EncodeToString(picture.Bytes())}})
	}))
	defer server.Close()
	c := &WebsiteCapture{client: server.Client(), endpoint: server.URL, credential: "synthetic", fetch: func(context.Context, string) ([]byte, string, string, error) {
		return []byte("<body>Public</body>"), "text/html", "https://example.org/", nil
	}}
	out, err := c.Capture(t.Context(), "https://example.org/")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	var decoded struct {
		Sections []struct {
			Image               string `json:"image"`
			X, Y, Width, Height int
		}
	}
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Sections) != 3 {
		t.Fatalf("want three full-coverage sections, got %d", len(decoded.Sections))
	}
	end := 0
	for _, section := range decoded.Sections {
		if section.X != 0 || section.Width != 320 || section.Y > end || section.Y+section.Height > 1600 {
			t.Fatal("gap or invalid section", section.Y, section.Height)
		}
		b, err := base64.StdEncoding.DecodeString(strings.SplitN(section.Image, ",", 2)[1])
		if err != nil {
			t.Fatal(err)
		}
		cropped, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if cropped.Bounds().Dx() != section.Width || cropped.Bounds().Dy() != section.Height || cropped.At(77, 0) != img.At(77, section.Y) {
			t.Fatal("section changed pixels")
		}
		end = section.Y + section.Height
	}
	if end != 1600 || out.Images[0] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(picture.Bytes()) {
		t.Fatal("lost full capture")
	}
}

func TestWebsiteCaptureUsesTwoCompleteSectionsForModeratelyTallPage(t *testing.T) {
	var picture bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 320, 1000))
	for y := 0; y < 1000; y++ {
		for x := 0; x < 320; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(y % 251), G: uint8(x % 251), A: 255})
		}
	}
	if err := png.Encode(&picture, img); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"success": true, "meta": map[string]any{"status": 200, "finalUrl": "https://example.org/"}, "result": map[string]string{"content": "<body>Public original</body>", "screenshot": base64.StdEncoding.EncodeToString(picture.Bytes())}})
	}))
	defer server.Close()
	c := &WebsiteCapture{client: server.Client(), endpoint: server.URL, credential: "synthetic", fetch: func(context.Context, string) ([]byte, string, string, error) {
		return []byte("<body>Public</body>"), "text/html", "https://example.org/", nil
	}}
	out, err := c.Capture(t.Context(), "https://example.org/")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Sections) != 2 {
		t.Fatalf("want two complete sections, got %d", len(out.Sections))
	}
	end := 0
	for _, section := range out.Sections {
		if section.X != 0 || section.Width != 320 || section.Y > end || section.Y+section.Height > 1000 {
			t.Fatal("invalid full-page coverage", section)
		}
		b, err := base64.StdEncoding.DecodeString(strings.SplitN(section.Image, ",", 2)[1])
		if err != nil {
			t.Fatal(err)
		}
		crop, err := png.Decode(bytes.NewReader(b))
		if err != nil || crop.At(77, 0) != img.At(77, section.Y) || crop.At(77, section.Height-1) != img.At(77, section.Y+section.Height-1) {
			t.Fatal("section changed pixels", err)
		}
		end = section.Y + section.Height
	}
	if end != 1000 || out.Images[0] != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(picture.Bytes()) {
		t.Fatal("original capture changed or rows omitted")
	}
}

func TestWebsiteSectionsOptionalCapacityAndCancellationPreserveCapture(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, test := range []struct {
		width, height int
		ctx           context.Context
	}{{4000, 15000, t.Context()}, {320, 1600, ctx}} {
		sections, err := websiteSections(test.ctx, []byte("not decoded"), test.width, test.height)
		if err != nil || len(sections) != 0 {
			t.Fatal("optional capacity/cancellation decoded or failed", err)
		}
	}
}

func TestWebsiteCaptureRetainsNumericRejectionWithoutProviderProse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		w.Write([]byte(`{"errors":[{"code":1234,"message":"synthetic-token arbitrary secret"}]}`))
	}))
	defer upstream.Close()
	capture := &WebsiteCapture{client: upstream.Client(), endpoint: upstream.URL, credential: "synthetic-token", fetch: func(ctx context.Context, u string) ([]byte, string, string, error) {
		return []byte("<body>Landing</body>"), "text/html", u, nil
	}}
	_, err := capture.Capture(t.Context(), "http://example.org/")
	if err == nil || !strings.Contains(err.Error(), "HTTP 422, code 1234") || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "token") {
		t.Fatal(err)
	}
}

func TestWebsiteFrameObservationsAreTypedAndPreserved(t *testing.T) {
	raw := `{"version":2,"reachedBottom":true,"timedOut":false,"error":false,"pendingImages":0,"brokenImages":0,"fontsReady":true,"documentWidth":1280,"documentHeight":1800,"dismissedOverlays":0,"frameCount":2,"pendingFrames":1,"unobservedFrames":0,"videoCount":1,"pendingVideos":1}`
	out := websiteLoading(`<meta name="fixture" content='`+raw+`'>`, "fixture", 1280, 1800)
	if out.Error || out.PendingFrames == nil || *out.PendingFrames != 1 || out.PendingVideos == nil || *out.PendingVideos != 1 {
		t.Fatal(out)
	}
	bad := strings.Replace(raw, `"pendingFrames":1`, `"pendingFrames":3`, 1)
	if !websiteLoading(`<meta name="fixture" content='`+bad+`'>`, "fixture", 1280, 1800).Error {
		t.Fatal("invalid frame count accepted")
	}
}

func TestWebsiteEmbedPlaceholdersArePreserved(t *testing.T) {
	raw := `{"version":3,"reachedBottom":true,"timedOut":false,"error":false,"pendingImages":0,"brokenImages":0,"fontsReady":true,"documentWidth":1280,"documentHeight":1800,"dismissedOverlays":0,"frameCount":0,"pendingFrames":0,"unobservedFrames":0,"videoCount":0,"pendingVideos":0,"embedPlaceholderCount":9}`
	out := websiteLoading(`<meta name="fixture" content='`+raw+`'>`, "fixture", 1280, 1800)
	if out.Error || out.Version != 3 {
		t.Fatal(out)
	}
}
