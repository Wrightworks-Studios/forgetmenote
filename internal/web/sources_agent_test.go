package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
	"forgetmenote/plugins"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func encoded(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

// notebookCall is a browser request to the default notebook at the origin an
// MCP test client set.
func notebookCall(app *Server, method, path, body string) *httptest.ResponseRecorder {
	if !strings.HasPrefix(path, "/p/") {
		path = "/p/default" + path
	}
	return apiCall(app, method, path, body, testToken)
}

// agentText is an extraction a connected agent uploads with its original.
func agentText(unit string, texts ...string) map[string]any {
	sections := []map[string]any{}
	for i, text := range texts {
		sections = append(sections, map[string]any{"number": i + 1, "text": text})
	}
	return map[string]any{"unit": unit, "sections": sections}
}

type awaitingListing struct {
	Revision string `json:"revision"`
	Sources  []struct {
		Path, Title, Format, MIME, Extraction, Warning, Original, Revision string
	} `json:"sources"`
}

func awaitingPaths(t *testing.T, raw []byte) (awaitingListing, map[string]bool) {
	t.Helper()
	var listing awaitingListing
	if err := json.Unmarshal(raw, &listing); err != nil {
		t.Fatal(err, string(raw))
	}
	paths := map[string]bool{}
	for _, source := range listing.Sources {
		paths[source.Path] = true
	}
	return listing, paths
}

// An agent's own extraction is archived beside the original it uploads, as
// text the server labels client-supplied and unverified, through the same
// method for MCP and the browser's preview.
func TestArchiveSourceWithAgentText(t *testing.T) {
	app, store := fixture(t)
	client := actionClient(t, app, catalog.Bootstrap, "notes:read notes:write")
	png, err := os.ReadFile("../sources/testdata/reference.png")
	if err != nil {
		t.Fatal(err)
	}
	text := map[string]any{"unit": "page", "sections": []map[string]any{{"number": 1, "title": "Front", "text": "Whales sighted <b>off</b> the [quay](https://example.com).\n\nSecond paragraph."}}}
	raw := actionCall(t, client, "archive_source", map[string]any{"project": "default", "filename": "notice.png", "data": encoded(png), "title": "Harbour notice", "extractor": "Test agent; test-model-1", "text": text}, false)
	var note noteOutput
	if err := json.Unmarshal(raw, &note); err != nil {
		t.Fatal(err)
	}
	record := note.Metadata.Source
	if record == nil || record.Extraction != "client-supplied" || record.Extractor != "Test agent; test-model-1" || record.MIME != "image/png" || record.Method != "uploaded" || record.SHA256 != workspace.ContentHash(png) || !strings.Contains(record.Warning, "not verified") {
		t.Fatalf("source record: %+v", record)
	}
	// The server labels the sections; the agent's words stay literal.
	if !strings.Contains(note.Content, "## Extracted content\n\n## Page 1 — Front\n\n") || !strings.Contains(note.Content, `Whales sighted \<b\>off\</b\> the \[quay\](https://example.com).`+"\n\nSecond paragraph.") {
		t.Fatal("supplied text", note.Content)
	}
	var verified workspace.SourceVerification
	json.Unmarshal(actionCall(t, client, "verify_source", map[string]any{"project": "default", "path": note.Path}, false), &verified)
	if verified.OriginalStatus != "match" || verified.TextStatus != "match" {
		t.Fatalf("verification: %+v", verified)
	}
	download := notebookCall(app, "GET", "/p/default/assets/"+record.Original+"?revision="+note.Revision, "")
	if download.Code != 200 || !bytes.Equal(download.Body.Bytes(), png) {
		t.Fatal("original download", download.Code)
	}
	if found := notebookCall(app, "GET", "/api/notes?q=Whales", ""); !strings.Contains(found.Body.String(), note.Path) {
		t.Fatal("supplied text is not searchable", found.Body.String())
	}
	history, _ := store.History(context.Background(), note.Path)
	if len(history) != 1 || history[0].Source != "mcp" {
		t.Fatal("attribution", history)
	}

	// Supplied text replaces what the server would extract from its format.
	raw = actionCall(t, client, "archive_source", map[string]any{"project": "default", "filename": "log.txt", "data": encoded([]byte("Words the server reads.")), "extractor": "Test agent; test-model-1", "text": agentText("section", "Words the agent read.")}, false)
	note = noteOutput{}
	json.Unmarshal(raw, &note)
	if note.Metadata.Source == nil || note.Metadata.Source.Extraction != "client-supplied" || !strings.Contains(note.Content, "## Section 1\n\nWords the agent read.") || strings.Contains(note.Content, "Words the server reads.") {
		t.Fatal("supplied text did not replace server extraction", note.Content)
	}

	// The browser's preview and save take the same input.
	body, _ := json.Marshal(map[string]any{"filename": "recording.wav", "data": encoded([]byte("RIFF recording")), "extractor": "Test agent; test-model-1", "text": agentText("segment", "Good morning, harbour.", "Tide is rising.")})
	preview := notebookCall(app, "POST", "/api/sources/preview", string(body))
	if preview.Code != 200 {
		t.Fatal(preview.Code, preview.Body.String())
	}
	var prepared preparedSource
	if err := json.Unmarshal(preview.Body.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.Capture.Record.Extraction != "client-supplied" || !strings.Contains(prepared.Preview, "## Segment 2\n\nTide is rising.") {
		t.Fatalf("preview: %+v", prepared.Capture.Record)
	}
	payload, _ := json.Marshal(map[string]any{"prepared": prepared, "title": "Morning recording"})
	imported := notebookCall(app, "POST", "/api/sources/import", string(payload))
	if imported.Code != 200 {
		t.Fatal(imported.Code, imported.Body.String())
	}
	note = noteOutput{}
	json.Unmarshal(imported.Body.Bytes(), &note)
	if note.Metadata.Source == nil || note.Metadata.Source.Extraction != "client-supplied" || note.Metadata.Source.MIME != "audio/wav" {
		t.Fatalf("saved preview: %+v", note.Metadata.Source)
	}
}

// Agent text is untrusted input: whatever the server cannot store as
// numbered, bounded, literal sections beside an original is refused, and
// nothing is committed.
func TestArchiveSourceRefusesInvalidAgentText(t *testing.T) {
	t.Setenv("FMN_LIMIT_SOURCE_TEXT_BYTES", "4096")
	app, store := fixture(t)
	client := actionClient(t, app, catalog.Bootstrap, "notes:read notes:write")
	before, _ := store.Status(context.Background())
	original := encoded([]byte("Original words"))
	section := func(number int, title, text string) map[string]any {
		return map[string]any{"number": number, "title": title, "text": text}
	}
	sections := func(list ...map[string]any) map[string]any {
		return map[string]any{"unit": "page", "sections": list}
	}
	many := []map[string]any{}
	for i := 1; i <= 10001; i++ {
		many = append(many, section(i, "", ""))
	}
	valid := agentText("page", "Readable words")
	for name, test := range map[string]struct {
		args    map[string]any
		message string
	}{
		"text over the limit":         {map[string]any{"filename": "a.txt", "data": original, "extractor": "Agent", "text": agentText("page", strings.Repeat("x", 5000))}, "FMN_LIMIT_SOURCE_TEXT_BYTES"},
		"escaped text over the limit": {map[string]any{"filename": "a.txt", "data": original, "extractor": "Agent", "text": agentText("page", strings.Repeat("<", 3000))}, "FMN_LIMIT_SOURCE_TEXT_BYTES"},
		"an unknown unit":             {map[string]any{"filename": "a.txt", "data": original, "extractor": "Agent", "text": agentText("chapter", "Words")}, "page, slide, sheet, segment or section"},
		"no sections":                 {map[string]any{"filename": "a.txt", "data": original, "extractor": "Agent", "text": sections()}, "at least one section"},
		"too many sections":           {map[string]any{"filename": "a.txt", "data": original, "extractor": "Agent", "text": sections(many...)}, "10000 sections"},
		"a numbering gap":             {map[string]any{"filename": "a.txt", "data": original, "extractor": "Agent", "text": sections(section(1, "", "One"), section(3, "", "Three"))}, "number sections from 1"},
		"numbering from zero":         {map[string]any{"filename": "a.txt", "data": original, "extractor": "Agent", "text": sections(section(0, "", "Zero"))}, "number sections from 1"},
		"a title on two lines":        {map[string]any{"filename": "a.txt", "data": original, "extractor": "Agent", "text": sections(section(1, "One\nTwo", "Words"))}, "title"},
		"a long title":                {map[string]any{"filename": "a.txt", "data": original, "extractor": "Agent", "text": sections(section(1, strings.Repeat("t", 201), "Words"))}, "title"},
		"a NUL in the text":           {map[string]any{"filename": "a.txt", "data": original, "extractor": "Agent", "text": agentText("page", "Wor\x00ds")}, "text"},
		"no text in any section":      {map[string]any{"filename": "a.txt", "data": original, "extractor": "Agent", "text": agentText("page", " ", "\n")}, "no text"},
		"no extractor":                {map[string]any{"filename": "a.txt", "data": original, "text": valid}, "extractor"},
		"an extractor on two lines":   {map[string]any{"filename": "a.txt", "data": original, "extractor": "Agent\nmodel", "text": valid}, "extractor"},
		"a long extractor":            {map[string]any{"filename": "a.txt", "data": original, "extractor": strings.Repeat("a", 201), "text": valid}, "extractor"},
		"an extractor without text":   {map[string]any{"filename": "a.txt", "data": original, "extractor": "Agent"}, "extractor"},
		"text without the original":   {map[string]any{"filename": "a.pdf", "url": "https://example.com/a.pdf", "extractor": "Agent", "text": valid}, "original"},
		"text and nothing else":       {map[string]any{"extractor": "Agent", "text": valid}, "original"},
		"hosted processing with text": {map[string]any{"filename": "a.png", "data": original, "extractor": "Agent", "text": valid, "externalOCR": true}, "hosted processing"},
		"an unsupported format":       {map[string]any{"filename": "a.exe", "data": original, "extractor": "Agent", "text": valid}, "unsupported"},
	} {
		t.Run(name, func(t *testing.T) {
			args := map[string]any{"project": "default"}
			for key, value := range test.args {
				args[key] = value
			}
			result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "archive_source", Arguments: args})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(result)
			if !result.IsError || !strings.Contains(string(raw), test.message) {
				t.Fatalf("archive_source: %s", raw)
			}
			body, _ := json.Marshal(test.args)
			response := notebookCall(app, "POST", "/api/sources/preview", string(body))
			var failure struct{ Code, Error string }
			json.Unmarshal(response.Body.Bytes(), &failure)
			if response.Code != 400 || failure.Code != "invalid_request" || !strings.Contains(failure.Error, test.message) {
				t.Fatalf("preview: %d %s", response.Code, response.Body.String())
			}
		})
	}
	// Source jobs run server extraction; agent text goes to archive_source.
	result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "prepare_source", Arguments: map[string]any{"project": "default", "filename": "a.txt", "data": original, "extractor": "Agent", "text": valid}})
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(result); !result.IsError || !strings.Contains(string(raw), "archive_source") {
		t.Fatalf("prepare_source: %s", raw)
	}
	// A reader may not archive, with or without text.
	if err := app.catalog.Member(catalog.Bootstrap, "default", "reader", "read"); err != nil {
		t.Fatal(err)
	}
	reader := actionClient(t, app, "reader", "notes:read notes:write")
	actionCall(t, reader, "archive_source", map[string]any{"project": "default", "filename": "a.txt", "data": original, "extractor": "Agent", "text": valid}, true)
	after, _ := store.Status(context.Background())
	if after.Revision != before.Revision {
		t.Fatal("a refused upload committed")
	}
}

// A source stored without text waits for an agent: the listing names it, and
// the refresh path attaches the agent's text to it under the same revision
// and lock checks as any refresh.
func TestAgentTextAttachedToStoredSource(t *testing.T) {
	app, store := fixture(t)
	client := actionClient(t, app, catalog.Bootstrap, "notes:read notes:write")
	original := []byte("<html><body><p> </p></body></html>")
	var note noteOutput
	json.Unmarshal(actionCall(t, client, "archive_source", map[string]any{"project": "default", "filename": "blank.html", "data": encoded(original), "title": "Blank page"}, false), &note)
	if note.Metadata.Source == nil || note.Metadata.Source.Extraction != "no-text" {
		t.Fatalf("fixture: %+v", note.Metadata.Source)
	}
	listing, paths := awaitingPaths(t, actionCall(t, client, "list_sources_awaiting_text", map[string]any{"project": "default"}, false))
	if !paths[note.Path] {
		t.Fatal("the source without text is not listed", listing)
	}
	revision := listing.Sources[0].Revision
	refresh := func(revision string) map[string]any {
		return map[string]any{"project": "default", "path": note.Path, "revision": revision, "filename": path.Base(note.Metadata.Source.Original), "data": encoded(original), "extractor": "Test agent; test-model-1", "text": agentText("page", "Words the agent read.")}
	}
	actionCall(t, client, "archive_source", refresh(strings.Repeat("2", 40)), true)
	var updated noteOutput
	json.Unmarshal(actionCall(t, client, "archive_source", refresh(revision), false), &updated)
	if updated.Path != note.Path || updated.Metadata.Title != "Blank page" || updated.Metadata.Source == nil || updated.Metadata.Source.Extraction != "client-supplied" || updated.Metadata.Source.ID != note.Metadata.Source.ID || updated.Metadata.Source.Original != note.Metadata.Source.Original || !strings.Contains(updated.Content, "Words the agent read.") {
		t.Fatalf("refresh: %+v\n%s", updated.Metadata.Source, updated.Content)
	}
	// The refresh is one more revision; the earlier capture stays in history.
	if history, _ := store.History(context.Background(), note.Path); len(history) != 2 {
		t.Fatal("history", history)
	}
	if _, paths = awaitingPaths(t, actionCall(t, client, "list_sources_awaiting_text", map[string]any{"project": "default"}, false)); paths[note.Path] {
		t.Fatal("a source with agent text is still listed")
	}
	// A refresh respects locks like any other.
	lock := actionCall(t, client, "set_note_lock", map[string]any{"project": "default", "path": note.Path, "locked": true, "revision": updated.Revision}, false)
	var locked struct{ Revision string }
	json.Unmarshal(lock, &locked)
	actionCall(t, client, "archive_source", refresh(locked.Revision), true)
}

// The listing names every source note whose extraction is empty or failed,
// with what an agent needs to refresh it, and requires only Read.
func TestSourcesAwaitingTextListing(t *testing.T) {
	app, _ := fixture(t)
	client := actionClient(t, app, catalog.Bootstrap, "notes:read notes:write")
	archive := func(args map[string]any) noteOutput {
		t.Helper()
		args["project"] = "default"
		var note noteOutput
		json.Unmarshal(actionCall(t, client, "archive_source", args, false), &note)
		if note.Metadata.Source == nil {
			t.Fatal("not archived", args["filename"])
		}
		return note
	}
	extracted := archive(map[string]any{"filename": "readable.txt", "data": encoded([]byte("Readable words"))})
	blank := archive(map[string]any{"filename": "blank.html", "data": encoded([]byte("<p></p>")), "title": "Blank page"})
	broken := archive(map[string]any{"filename": "broken.pdf", "data": encoded([]byte("%PDF-1.4\nnot a document"))})
	supplied := archive(map[string]any{"filename": "notice.png", "data": encoded([]byte("image")), "extractor": "Agent", "text": agentText("page", "Notice")})
	if broken.Metadata.Source.Extraction != "failed" {
		t.Fatalf("fixture: %+v", broken.Metadata.Source)
	}
	actionCall(t, client, "save_note", map[string]any{"project": "default", "path": "plain.md", "content": "# Plain note\n", "revision": ""}, false)
	check := func(raw []byte) {
		t.Helper()
		listing, paths := awaitingPaths(t, raw)
		if !paths[blank.Path] || !paths[broken.Path] || paths[extracted.Path] || paths[supplied.Path] || paths["plain.md"] || len(listing.Sources) != 2 {
			t.Fatalf("listing: %+v", listing)
		}
		for _, source := range listing.Sources {
			if source.Revision != listing.Revision || len(source.Revision) != 40 || !workspace.SourceOriginalPath(source.Original) {
				t.Fatalf("listed source: %+v", source)
			}
			if source.Path == blank.Path && (source.Title != "Blank page" || source.Format != "Webpage" || source.MIME != "text/html" || source.Extraction != "no-text" || source.Original != blank.Metadata.Source.Original) {
				t.Fatalf("listed source: %+v", source)
			}
			if source.Path == broken.Path && (source.Format != "PDF" || source.Extraction != "failed" || source.Warning == "") {
				t.Fatalf("listed source: %+v", source)
			}
		}
	}
	check(actionCall(t, client, "list_sources_awaiting_text", map[string]any{"project": "default"}, false))
	response := notebookCall(app, "GET", "/api/sources/awaiting-text", "")
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	check(response.Body.Bytes())
	if err := app.catalog.Member(catalog.Bootstrap, "default", "reader", "read"); err != nil {
		t.Fatal(err)
	}
	check(actionCall(t, actionClient(t, app, "reader", "notes:read"), "list_sources_awaiting_text", map[string]any{"project": "default"}, false))
	actionCall(t, actionClient(t, app, "outsider", "notes:read"), "list_sources_awaiting_text", map[string]any{"project": "default"}, true)
}

// source_formats says, per format, whether the server extracts it or the
// connected agent should, for MCP and the browser alike.
func TestSourceFormatsSayWhoExtracts(t *testing.T) {
	app, _ := fixture(t)
	client := actionClient(t, app, catalog.Bootstrap, "notes:read")
	check := func(raw []byte) {
		t.Helper()
		var formats []struct {
			Extensions    []string
			MIME          string
			ExtractedBy   string `json:"extractedBy"`
			AgentExtracts string `json:"agentExtracts"`
		}
		if err := json.Unmarshal(raw, &formats); err != nil {
			t.Fatal(err, string(raw))
		}
		seen := map[string]bool{}
		for _, format := range formats {
			ext := format.Extensions[0]
			seen[ext] = true
			want := "server"
			if strings.HasPrefix(format.MIME, "image/") || strings.HasPrefix(format.MIME, "audio/") || strings.HasPrefix(format.MIME, "video/") || ext == ".pages" || ext == ".key" {
				want = "agent"
			}
			if format.ExtractedBy != want {
				t.Errorf("%s: extracted by %q, want %q", ext, format.ExtractedBy, want)
			}
			if (ext == ".pdf") != (format.AgentExtracts != "") || ext == ".pdf" && !strings.Contains(format.AgentExtracts, "scanned") {
				t.Errorf("%s: agent extracts %q", ext, format.AgentExtracts)
			}
		}
		for _, ext := range []string{".pdf", ".docx", ".xlsx", ".epub", ".eml", ".csv", ".html", ".txt", ".png", ".wav", ".mp4"} {
			if !seen[ext] {
				t.Errorf("%s is not listed", ext)
			}
		}
	}
	response := notebookCall(app, "GET", "/api/sources/formats", "")
	if response.Code != 200 {
		t.Fatal(response.Code)
	}
	check(response.Body.Bytes())
	var out struct {
		Formats json.RawMessage `json:"formats"`
	}
	json.Unmarshal(actionCall(t, client, "source_formats", map[string]any{"project": "default"}, false), &out)
	check(out.Formats)
}

// Connected agents learn from the server's instructions and the source
// tools' descriptions which uploads to extract themselves and how.
func TestMCPInstructionsTellAgentsWhatToExtract(t *testing.T) {
	app, _ := fixture(t)
	client := actionClient(t, app, catalog.Bootstrap, "notes:read notes:write")
	instructions := client.InitializeResult().Instructions
	if !strings.Contains(instructions, plugins.Instructions()) {
		t.Fatal("instructions omit the workspace guidance")
	}
	for _, want := range []string{"source_formats", "extractedBy", "archive_source", "`extractor`", "`data`", "list_sources_awaiting_text", "sections"} {
		if !strings.Contains(instructions, want) {
			t.Errorf("instructions omit %s", want)
		}
	}
	tools, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	described := map[string]string{}
	for _, tool := range tools.Tools {
		described[tool.Name] = tool.Description
	}
	for _, name := range []string{"archive_source", "source_formats", "list_sources_awaiting_text"} {
		if !strings.Contains(described[name], "extractedBy") || !strings.Contains(described[name], "agent") {
			t.Errorf("%s does not repeat the extraction rule: %q", name, described[name])
		}
	}
}
