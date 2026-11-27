package web

import (
	"encoding/json"
	"os"
	"path"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The server reads no image, audio, video, Pages or Keynote original: it
// stores the original alone, labelled original-only, and lists it for a
// connected agent to read and refresh with its text.
func TestAgentFormatsArchiveTheOriginalOnly(t *testing.T) {
	app, _ := fixture(t)
	client := actionClient(t, app, catalog.Bootstrap, "notes:read notes:write")
	read := func(name string) []byte {
		t.Helper()
		raw, err := os.ReadFile("../sources/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	originals := map[string][]byte{
		"notice.png":   read("reference.png"),
		"notice.tiff":  read("reference.tiff"),
		"notice.wav":   read("announcement.wav"),
		"notice.pages": read("iwork/modern.pages"),
		"notice.key":   read("iwork/modern.key"),
		// The server does not read these formats, so it checks only the
		// name: a file that is not the image it is named is stored as is.
		"notice.jpg": []byte("not an image"),
	}
	archived := map[string]noteOutput{}
	for name, original := range originals {
		var note noteOutput
		if err := json.Unmarshal(actionCall(t, client, "archive_source", map[string]any{"project": "default", "filename": name, "data": encoded(original)}, false), &note); err != nil {
			t.Fatal(err)
		}
		record := note.Metadata.Source
		if record == nil || record.Extraction != "original-only" || record.SHA256 != workspace.ContentHash(original) || record.TextSHA256 != workspace.ContentHash(nil) || !strings.Contains(record.Warning, "connected agent") || strings.Contains(record.Extractor, "Tesseract") || strings.Contains(record.Extractor, "FFmpeg") {
			t.Fatalf("%s: %+v", name, record)
		}
		if download := notebookCall(app, "GET", "/assets/"+record.Original+"?revision="+note.Revision, ""); download.Code != 200 || download.Body.String() != string(original) {
			t.Fatalf("%s: original %d", name, download.Code)
		}
		verified := actionCall(t, client, "verify_source", map[string]any{"project": "default", "path": note.Path}, false)
		if !strings.Contains(string(verified), `"originalStatus":"match"`) || !strings.Contains(string(verified), `"textStatus":"match"`) {
			t.Fatalf("%s: %s", name, verified)
		}
		archived[name] = note
	}
	// The browser's preview stores the original alone too.
	preview := notebookCall(app, "POST", "/api/sources/preview", `{"filename":"poster.webp","data":"`+encoded(read("reference.webp"))+`"}`)
	if preview.Code != 200 || !strings.Contains(preview.Body.String(), `"extraction":"original-only"`) {
		t.Fatalf("preview: %d %s", preview.Code, preview.Body.String())
	}
	listing, paths := awaitingPaths(t, actionCall(t, client, "list_sources_awaiting_text", map[string]any{"project": "default"}, false))
	for name, note := range archived {
		if !paths[note.Path] {
			t.Fatalf("%s is not listed: %+v", name, listing)
		}
	}
	for _, source := range listing.Sources {
		if source.Extraction != "original-only" {
			t.Fatalf("listed: %+v", source)
		}
	}
	// An agent's text completes the source.
	note := archived["notice.png"]
	var updated noteOutput
	json.Unmarshal(actionCall(t, client, "archive_source", map[string]any{"project": "default", "path": note.Path, "revision": listing.Revision, "filename": path.Base(note.Metadata.Source.Original), "data": encoded(originals["notice.png"]), "extractor": "Test agent; test-model-1", "text": agentText("page", "Harbour closed at noon.")}, false), &updated)
	if updated.Metadata.Source == nil || updated.Metadata.Source.Extraction != "client-supplied" || !strings.Contains(updated.Content, "Harbour closed at noon.") {
		t.Fatalf("refresh: %+v", updated.Metadata.Source)
	}
}

// Hosted OCR, figure descriptions and transcription are gone from every
// transport: their options are unknown, the formats no longer offer them, and
// the instance AI policy no longer has switches for them.
func TestHostedSourceProcessingIsGone(t *testing.T) {
	app, store := fixture(t)
	client := actionClient(t, app, catalog.Bootstrap, "notes:read notes:write")
	before, _ := store.Status(t.Context())
	original := encoded([]byte("Original words"))
	for _, option := range []string{"externalOCR", "describeImages", "transcribe", "firstPage"} {
		var value any = true
		if option == "firstPage" {
			value = 1
		}
		for _, tool := range []string{"archive_source", "prepare_source"} {
			result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"project": "default", "filename": "a.png", "data": original, option: value}})
			if err == nil && !result.IsError {
				t.Fatalf("%s accepted %s", tool, option)
			}
		}
		body, _ := json.Marshal(map[string]any{"filename": "a.png", "data": original, option: value})
		for _, route := range []string{"/api/sources/preview", "/api/sources/jobs"} {
			if response := notebookCall(app, "POST", route, string(body)); response.Code != 400 {
				t.Fatalf("%s accepted %s: %d %s", route, option, response.Code, response.Body.String())
			}
		}
	}
	if after, _ := store.Status(t.Context()); after.Revision != before.Revision {
		t.Fatal("a refused upload committed")
	}
	formats := notebookCall(app, "GET", "/api/sources/formats", "")
	for _, key := range []string{"externalOCR", "hostedDescriptions", "transcription", "describeImages"} {
		if strings.Contains(formats.Body.String(), `"`+key+`"`) {
			t.Fatalf("formats still offer %s: %s", key, formats.Body.String())
		}
	}
	settings := localRequest(app, "/api/organization/action/ai_settings", `{}`, recoverySession(t, app))
	if settings.Code != 200 {
		t.Fatal(settings.Code, settings.Body.String())
	}
	for _, key := range []string{"sourceOCR", "sourceDescriptions", "sourceTranscription", "sourceVision"} {
		if strings.Contains(settings.Body.String(), `"`+key+`"`) {
			t.Fatalf("AI settings still offer %s: %s", key, settings.Body.String())
		}
	}
}
