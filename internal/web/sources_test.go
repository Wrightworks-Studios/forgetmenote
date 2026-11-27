package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSourceUploadBeyondLegacyBudgetRetainsReadableOriginal(t *testing.T) {
	app, _ := fixture(t)
	response := call(app, "GET", "/api/sources/formats", "", testToken)
	var formats []map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &formats); err != nil {
		t.Fatal(err)
	}
	if len(formats) == 0 || formats[0]["maxBytes"] != float64(64<<20) {
		t.Errorf("source discovery does not advertise the 64 MiB default")
	}
	// Comments keep the extracted text small while the original exceeds both
	// the legacy source limit and the ordinary 32 MiB note-file budget.
	comment := append([]byte("<!--"), bytes.Repeat([]byte("x"), 1<<20)...)
	comment = append(comment, []byte("-->")...)
	original := append([]byte("<title>Large source</title><p>Amber textbook evidence.</p>"), bytes.Repeat(comment, 36)...)
	body, _ := json.Marshal(map[string]any{"filename": "large.html", "data": base64.StdEncoding.EncodeToString(original)})
	preview := call(app, "POST", "/api/sources/preview", string(body), testToken)
	if preview.Code != 200 {
		t.Fatalf("large source preview returned %d", preview.Code)
	}
	var prepared preparedSource
	if err := json.Unmarshal(preview.Body.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(map[string]any{"prepared": prepared})
	imported := call(app, "POST", "/api/sources/import", string(body), testToken)
	if imported.Code != 200 {
		t.Fatalf("large source save returned %d", imported.Code)
	}
	var note noteOutput
	if err := json.Unmarshal(imported.Body.Bytes(), &note); err != nil || note.Metadata.Source == nil {
		t.Fatal("source metadata missing", err)
	}
	download := call(app, "GET", "/p/default/assets/"+note.Metadata.Source.Original+"?revision="+note.Revision, "", testToken)
	if download.Code != 200 || !bytes.Equal(download.Body.Bytes(), original) {
		t.Fatalf("large source original download returned %d or changed bytes", download.Code)
	}
}

func TestSourceFormatsAdvertiseOperatorUploadBudget(t *testing.T) {
	t.Setenv("FMN_LIMIT_SOURCE_BYTES", "12345")
	app, _ := fixture(t)
	for _, raw := range [][]byte{
		call(app, "GET", "/api/sources/formats", "", testToken).Body.Bytes(),
		actionCall(t, actionClient(t, app, catalog.Bootstrap, "notes:read"), "source_formats", map[string]any{"project": "default"}, false),
	} {
		if !bytes.Contains(raw, []byte(`"maxBytes":12345`)) {
			t.Error("browser/API/MCP discovery omitted the configured source budget")
		}
	}
}

func TestSourceJSONBudgetIncludesBase64AndEscapedPreviewText(t *testing.T) {
	t.Setenv("FMN_LIMIT_SOURCE_BYTES", "4194304")
	t.Setenv("FMN_LIMIT_FILE_BYTES", "2097152")
	t.Setenv("FMN_LIMIT_BUNDLE_BYTES", "1048576")
	app, _ := fixture(t)
	// HTML entities expand to escaped Markdown, then expand further when the
	// prepared result is returned as JSON for save. Both are valid source bytes.
	original := []byte("<title>Encoded source</title><p>" + strings.Repeat("&lt;", 512<<10) + "</p><!--" + strings.Repeat("x", 1<<20) + "-->")
	body, _ := json.Marshal(map[string]any{"filename": "encoded.html", "data": base64.StdEncoding.EncodeToString(original)})
	preview := call(app, "POST", "/api/sources/preview", string(body), testToken)
	if preview.Code != 200 {
		t.Fatalf("encoded source preview returned %d", preview.Code)
	}
	var prepared preparedSource
	if err := json.Unmarshal(preview.Body.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(map[string]any{"prepared": prepared})
	if len(body) <= workspace.MaxSourceBytes()+(1<<20) {
		t.Fatal("fixture does not exercise encoded request headroom")
	}
	imported := call(app, "POST", "/api/sources/import", string(body), testToken)
	if imported.Code != 200 {
		t.Fatalf("encoded source save returned %d", imported.Code)
	}
}

func TestPDFImportUsesSharedBrowserAndMCPStorage(t *testing.T) {
	dir := t.TempDir()
	helper := "#!/bin/sh\ncat <<'XML'\n" + `<html><body><doc><page width="612" height="792"><flow><block><line><word xMin="10" yMin="10" xMax="30" yMax="20">Amber scan evidence</word></line></block></flow></page><page width="612" height="792"><flow><block><line><word xMin="10" yMin="10" xMax="30" yMax="20">Cobalt 17</word></line></block></flow></page></doc></body></html>` + "\nXML\n"
	if err := os.WriteFile(filepath.Join(dir, "pdftotext"), []byte(helper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	app, store := fixture(t)
	original := []byte("%PDF-1.4\nsynthetic text layer fixture")
	input := map[string]any{"filename": "scan.pdf", "data": base64.StdEncoding.EncodeToString(original)}
	body, _ := json.Marshal(input)
	preview := call(app, "POST", "/api/sources/preview", string(body), testToken)
	if preview.Code != 200 {
		t.Fatal(preview.Code, preview.Body.String())
	}
	var prepared preparedSource
	if err := json.Unmarshal(preview.Body.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prepared.HTML, `id="h-page-2"`) {
		t.Fatal("page citations or table structure lost", prepared.HTML)
	}
	importBody, _ := json.Marshal(map[string]any{"prepared": prepared, "title": "PDF source"})
	response := call(app, "POST", "/api/sources/import", string(importBody), testToken)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var browser noteOutput
	json.Unmarshal(response.Body.Bytes(), &browser)
	client := actionClient(t, app, catalog.Bootstrap, "notes:read notes:write")
	input["project"] = "default"
	raw := actionCall(t, client, "archive_source", input, false)
	var mcpNote noteOutput
	if err := json.Unmarshal(raw, &mcpNote); err != nil {
		t.Fatal(err)
	}
	for _, note := range []noteOutput{browser, mcpNote} {
		if note.Metadata.Source == nil || note.Metadata.Source.Extraction != "extracted" || !strings.Contains(note.Content, "Cobalt 17") {
			t.Fatal("PDF source lost in shared import", note)
		}
		doc, err := store.Read(context.Background(), note.Metadata.Source.Original, note.Revision)
		if err != nil || !bytes.Equal(doc.Content, original) {
			t.Fatal("original PDF not stored alongside converted note", err)
		}
		check := actionCall(t, client, "verify_source", map[string]any{"project": "default", "path": note.Path}, false)
		if !bytes.Contains(check, []byte("Extracted text checksum matches")) {
			t.Fatal("converted text checksum lost", string(check))
		}
	}
}

func TestSourcePreviewAndImport(t *testing.T) {
	app, _ := fixture(t)
	body, _ := json.Marshal(map[string]any{"filename": "page.html", "data": base64.StdEncoding.EncodeToString([]byte(`<html><title>Old grammar</title><body><h1>Verbs</h1><p>The rare amber paradigm.</p><script>steal()</script></body></html>`))})
	preview := call(app, "POST", "/api/sources/preview", string(body), testToken)
	if preview.Code != 200 {
		t.Fatalf("source preview: %d %s", preview.Code, preview.Body.String())
	}
	var prepared map[string]any
	if err := json.Unmarshal(preview.Body.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prepared["preview"].(string), "steal()") || !strings.Contains(prepared["preview"].(string), "amber paradigm") {
		t.Fatal("unsafe or missing extraction")
	}
	payload, _ := json.Marshal(map[string]any{"prepared": prepared, "title": "Grammar reference"})
	imported := call(app, "POST", "/api/sources/import", string(payload), testToken)
	if imported.Code != 200 {
		t.Fatalf("source import: %d %s", imported.Code, imported.Body.String())
	}
	if repeated := call(app, "POST", "/api/sources/import", string(payload), testToken); repeated.Code != 409 {
		t.Fatal("retry duplicated the source", repeated.Code)
	}
	var note noteOutput
	if err := json.Unmarshal(imported.Body.Bytes(), &note); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note.Content, "amber paradigm") || !strings.Contains(note.Content, "source_sha256:") {
		t.Fatal("missing source text or provenance", note.Content)
	}
	found := call(app, "GET", "/api/notes?q=amber", "", testToken)
	if found.Code != 200 || !strings.Contains(found.Body.String(), note.Path) {
		t.Fatal("source not searchable", found.Body.String())
	}
}

func TestSourcePreviewSealAndMCPPermissions(t *testing.T) {
	app, store := fixture(t)
	ctx := context.Background()
	body, _ := json.Marshal(map[string]any{"filename": "page.html", "data": base64.StdEncoding.EncodeToString([]byte("<p>Source evidence</p>"))})
	if res := call(app, "POST", "/api/sources/preview", string(body), ""); res.Code != 401 {
		t.Fatal("anonymous preview", res.Code)
	}
	preview := call(app, "POST", "/api/sources/preview", string(body), testToken)
	var prepared preparedSource
	json.Unmarshal(preview.Body.Bytes(), &prepared)
	prepared.Capture.Record.Method = "server-http" // cannot claim an upload was independently fetched
	payload, _ := json.Marshal(map[string]any{"prepared": prepared, "title": "Forgery"})
	if res := call(app, "POST", "/api/sources/import", string(payload), testToken); res.Code != 403 {
		t.Fatal("tampered capture accepted", res.Code)
	}
	json.Unmarshal(preview.Body.Bytes(), &prepared)
	prepared.Expires = 1
	payload, _ = json.Marshal(map[string]any{"prepared": prepared, "title": "Expired"})
	if res := call(app, "POST", "/api/sources/import", string(payload), testToken); res.Code != 403 {
		t.Fatal("expired capture accepted", res.Code)
	}
	client := actionClient(t, app, catalog.Bootstrap, "notes:read notes:write")
	raw := actionCall(t, client, "archive_source", map[string]any{"project": "default", "filename": "page.html", "data": base64.StdEncoding.EncodeToString([]byte("<p>MCP source evidence</p>")), "title": "MCP reference"}, false)
	var doc noteOutput
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Metadata.Source == nil {
		t.Fatal("source metadata missing", string(raw))
	}
	actionCall(t, client, "verify_source", map[string]any{"project": "default", "path": doc.Path}, false)
	actionCall(t, client, "read_source_original", map[string]any{"project": "default", "path": doc.Metadata.Source.Original}, false)
	account, _ := app.catalog.Account(catalog.Bootstrap)
	history, _ := store.History(ctx, doc.Path)
	if len(history) != 1 || history[0].Source != "mcp" || history[0].UserID != account.Identity().ID {
		t.Fatal("MCP attribution", history)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "reader", "read"); err != nil {
		t.Fatal(err)
	}
	reader := actionClient(t, app, "reader", "notes:read notes:write")
	actionCall(t, reader, "archive_source", map[string]any{"project": "default", "filename": "x.html", "data": base64.StdEncoding.EncodeToString([]byte("<p>denied</p>"))}, true)
	actionCall(t, reader, "read_source_original", map[string]any{"project": "default", "path": doc.Metadata.Source.Original}, false)
	app.catalog.Member(catalog.Bootstrap, "default", "reader", "")
	actionCall(t, reader, "read_source_original", map[string]any{"project": "default", "path": doc.Metadata.Source.Original}, true)
}

func TestSourcePreviewCannotBeTransferredBetweenUsers(t *testing.T) {
	app, _ := fixture(t)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	for _, user := range []string{"alice", "bob"} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", user, "edit"); err != nil {
			t.Fatal(err)
		}
	}
	body, _ := json.Marshal(map[string]any{"filename": "x.html", "data": base64.StdEncoding.EncodeToString([]byte("<p>Original</p>"))})
	response := proxyCall(app, "alice", "POST", "/p/default/api/sources/preview", string(body))
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var prepared preparedSource
	json.Unmarshal(response.Body.Bytes(), &prepared)
	payload, _ := json.Marshal(map[string]any{"prepared": prepared, "title": "Original"})
	if res := proxyCall(app, "bob", "POST", "/p/default/api/sources/import", string(payload)); res.Code != 403 {
		t.Fatal("another user used Alice's preview", res.Code)
	}
	if res := proxyCall(app, "alice", "POST", "/p/default/api/sources/import", string(payload)); res.Code != 200 {
		t.Fatal("own preview rejected", res.Code, res.Body.String())
	}
}

// A document extraction refuses is the caller's to correct, whichever rule
// refused it. The size of an upload alone is not such a rule.
func TestRejectedHTMLUploadIsClientError(t *testing.T) {
	app, _ := fixture(t)
	upload := func(document string) string {
		body, _ := json.Marshal(map[string]any{"filename": "page.html", "data": base64.StdEncoding.EncodeToString([]byte(document))})
		return string(body)
	}
	for name, test := range map[string]struct{ document, message string }{
		"one token over the limit":  {"<!--" + strings.Repeat("x", 16<<20) + "-->", "HTML token exceeds FMN_LIMIT_SOURCE_TEXT_BYTES"},
		"too many tokens":           {strings.Repeat("<br>", 100001), "HTML exceeds 100000 tokens"},
		"nesting beyond the parser": {strings.Repeat("<div>", 1000), "HTML could not be parsed"},
		"text over the limit":       {strings.Repeat("<p>"+strings.Repeat("x", 200)+"</p>", 30000), "extracted source text exceeds FMN_LIMIT_SOURCE_TEXT_BYTES"},
		"not UTF-8":                 {"<p>\xff</p>", "HTML text must be UTF-8"},
	} {
		t.Run(name, func(t *testing.T) {
			response := call(app, "POST", "/api/sources/preview", upload(test.document), testToken)
			var failure struct{ Code, Error string }
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
				t.Fatal(err)
			}
			if response.Code != 400 || failure.Code != "invalid_request" || !strings.Contains(failure.Error, test.message) {
				t.Fatalf("refused upload: %d %s", response.Code, response.Body.String())
			}
		})
	}
	comments := strings.Repeat("<!--"+strings.Repeat("x", 1<<10)+"-->", 16<<10)
	if response := call(app, "POST", "/api/sources/preview", upload(comments), testToken); response.Code != 200 {
		t.Fatalf("many small comments: %d %s", response.Code, response.Body.String())
	}
}

// Each format's own refusal reaches the caller as a client error, whichever
// extractor made it. Formats a connected agent extracts are not read, so their
// originals are not refused (TestAgentFormatsArchiveTheOriginalOnly).
func TestRefusedDocumentsAreClientErrors(t *testing.T) {
	app, _ := fixture(t)
	const notPackage = "invalid document ZIP container"
	tests := map[string]struct{ document, message string }{
		"notes.txt":   {"\xff\xfe\xff", "invalid UTF-16 text"},
		"table.csv":   {"a,b\"c\n", "invalid delimited text"},
		"table.tsv":   {"a\tb\"c\n", "invalid delimited text"},
		"memo.docx":   {"not a package", notPackage},
		"sheet.xlsx":  {"not a package", notPackage},
		"book.epub":   {"not a package", notPackage},
		"deck.pptx":   {"not a package", notPackage},
		"memo.odt":    {"not a package", notPackage},
		"sheet.ods":   {"not a package", notPackage},
		"deck.odp":    {"not a package", notPackage},
		"note.eml":    {"not an email", "invalid email message"},
		"note.msg":    {"not a compound file", "invalid Outlook compound file"},
		"unknown.bin": {"not a format", "unsupported or invalid source format"},
	}
	for filename, test := range tests {
		if !offeredSource(filename) && filename != "unknown.bin" {
			// Refused by the integration this build leaves out, before parsing.
			test.message = "this build leaves out"
		}
		t.Run(filename, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"filename": filename, "data": base64.StdEncoding.EncodeToString([]byte(test.document))})
			response := call(app, "POST", "/api/sources/preview", string(body), testToken)
			var failure struct{ Code, Error string }
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
				t.Fatal(err)
			}
			if response.Code != 400 || failure.Code != "invalid_request" || !strings.Contains(failure.Error, test.message) {
				t.Fatalf("refused upload: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

// A request that can never be captured is the caller's to correct, as a refused
// document is, whether it is refused on arrival or by its job.
func TestRefusedSourceRequestsAreClientErrors(t *testing.T) {
	t.Setenv("FMN_LIMIT_SOURCE_BYTES", "1024")
	app, _ := fixture(t)
	const publicURL = "provide a public HTTP or HTTPS URL without credentials"
	upload := base64.StdEncoding.EncodeToString([]byte("Amber"))
	for name, test := range map[string]struct {
		input   map[string]any
		message string
	}{
		"nothing to capture":     {map[string]any{}, publicURL},
		"not base64":             {map[string]any{"filename": "a.txt", "data": "***"}, "provide a nonempty base64 file"},
		"over the byte limit":    {map[string]any{"filename": "a.txt", "data": strings.Repeat("A", 2048)}, "source exceeds FMN_LIMIT_SOURCE_BYTES"},
		"URL scheme":             {map[string]any{"url": "ftp://example.org/a"}, publicURL},
		"URL credentials":        {map[string]any{"url": "https://user:secret@example.org/a"}, publicURL},
		"URL port":               {map[string]any{"url": "https://example.org:8443/a"}, "source URLs must use port 80 or 443"},
		"private address":        {map[string]any{"url": "http://169.254.169.254/latest/meta-data"}, "private or special-use"},
		"host resolving private": {map[string]any{"url": "http://localhost/a"}, "private or special-use"},
		"provenance URL":         {map[string]any{"filename": "a.txt", "data": upload, "url": "ftp://example.org/a"}, publicURL},
		"long filename":          {map[string]any{"filename": strings.Repeat("a", 1025) + ".txt", "data": upload}, "source filename or size exceeds its budget"},
		"long path":              {map[string]any{"filename": "a.txt", "data": upload, "path": strings.Repeat("a", 513)}, "source path exceeds its budget"},
	} {
		t.Run(name, func(t *testing.T) {
			body, _ := json.Marshal(test.input)
			for _, path := range []string{"/api/sources/preview", "/api/sources/jobs"} {
				response := call(app, "POST", path, string(body), testToken)
				if name == "host resolving private" && path == "/api/sources/jobs" {
					continue // Refused by its job, which this path only starts.
				}
				var failure struct{ Code, Error string }
				if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
					t.Fatal(path, response.Code, response.Body.String())
				}
				if response.Code != 400 || failure.Code != "invalid_request" || !strings.Contains(failure.Error, test.message) {
					t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
				}
			}
		})
	}
}

func TestRejectedHTMLIsClearToMCPCallers(t *testing.T) {
	app, _ := fixture(t)
	client := actionClient(t, app, catalog.Bootstrap, "notes:read notes:write")
	input := map[string]any{"project": "default", "filename": "page.html", "data": base64.StdEncoding.EncodeToString([]byte("<!--" + strings.Repeat("x", 5<<20) + "-->"))}
	result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "archive_source", Arguments: input})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	if !result.IsError || !strings.Contains(string(raw), "HTML token exceeds FMN_LIMIT_SOURCE_TEXT_BYTES") {
		t.Fatalf("archive_source: %s", raw)
	}
	result, err = client.CallTool(t.Context(), &mcp.CallToolParams{Name: "archive_source", Arguments: map[string]any{"project": "default", "filename": "a.txt", "data": "***"}})
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ = json.Marshal(result); !result.IsError || !strings.Contains(string(raw), "provide a nonempty base64 file") {
		t.Fatalf("archive_source: %s", raw)
	}
	var out sourceJobOutput
	if err := json.Unmarshal(actionCall(t, client, "prepare_source", input, false), &out); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for out.Job.State != "failed" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		if err := json.Unmarshal(actionCall(t, client, "source_job", map[string]any{"project": "default", "job": out.Job.ID}, false), &out); err != nil {
			t.Fatal(err)
		}
	}
	if out.Job.State != "failed" || out.Job.Warning != "Rejected: HTML token exceeds FMN_LIMIT_SOURCE_TEXT_BYTES" {
		t.Fatalf("source_job: %+v", out.Job)
	}
}

func TestSourceFormatsAndDocumentDownloads(t *testing.T) {
	app, _ := fixture(t)
	response := call(app, "GET", "/api/sources/formats", "", testToken)
	if response.Code != 200 || !strings.Contains(response.Body.String(), ".xlsx") {
		t.Fatalf("format discovery: %d %s", response.Code, response.Body.String())
	}
	for _, ext := range []string{"docx", "epub", "xlsx", "txt", "csv", "tsv"} {
		t.Run(ext, func(t *testing.T) {
			raw, err := os.ReadFile("../sources/testdata/reference." + ext)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]any{"filename": "reference." + ext, "data": base64.StdEncoding.EncodeToString(raw)})
			preview := call(app, "POST", "/api/sources/preview", string(body), testToken)
			if preview.Code != 200 {
				t.Fatal(preview.Code, preview.Body.String())
			}
			var prepared preparedSource
			if err := json.Unmarshal(preview.Body.Bytes(), &prepared); err != nil {
				t.Fatal(err)
			}
			payload, _ := json.Marshal(map[string]any{"prepared": prepared})
			imported := call(app, "POST", "/api/sources/import", string(payload), testToken)
			if imported.Code != 200 {
				t.Fatal(imported.Code, imported.Body.String())
			}
			var note noteOutput
			json.Unmarshal(imported.Body.Bytes(), &note)
			if note.Metadata.Source == nil {
				t.Fatal("missing provenance")
			}
			original := call(app, "GET", "/p/default/assets/"+note.Metadata.Source.Original+"?revision="+note.Revision, "", testToken)
			if original.Code != 200 || !bytes.Equal(original.Body.Bytes(), raw) || original.Header().Get("Content-Type") != workspace.SourceFormatForName("x."+ext).MIME || !strings.HasPrefix(original.Header().Get("Content-Disposition"), "attachment;") {
				t.Fatal("original download", original.Code, original.Header())
			}
			found := call(app, "GET", "/api/notes?q=Amber", "", testToken)
			if !strings.Contains(found.Body.String(), note.Path) {
				t.Fatal("not searchable")
			}
		})
	}
	client := actionClient(t, app, catalog.Bootstrap, "notes:read notes:write")
	formats := actionCall(t, client, "source_formats", map[string]any{"project": "default"}, false)
	if !strings.Contains(string(formats), ".xlsx") {
		t.Fatal("MCP formats missing")
	}
	raw, _ := os.ReadFile("../sources/testdata/reference.xlsx")
	result := actionCall(t, client, "archive_source", map[string]any{"project": "default", "filename": "book.xlsx", "data": base64.StdEncoding.EncodeToString(raw)}, false)
	var note noteOutput
	json.Unmarshal(result, &note)
	data := actionCall(t, client, "read_source_original", map[string]any{"project": "default", "path": note.Metadata.Source.Original, "revision": note.Revision}, false)
	var chunk binaryChunk
	json.Unmarshal(data, &chunk)
	decoded, _ := base64.StdEncoding.DecodeString(chunk.Data)
	if !bytes.Equal(decoded, raw) {
		t.Fatal("MCP original changed")
	}
}

func TestSourceDestinationFolder(t *testing.T) {
	app, store := fixture(t)
	prepare := func() preparedSource {
		body, _ := json.Marshal(map[string]any{"filename": "evidence.txt", "data": base64.StdEncoding.EncodeToString([]byte("Folder evidence"))})
		res := call(app, "POST", "/api/sources/preview", string(body), testToken)
		if res.Code != 200 {
			t.Fatal(res.Code, res.Body.String())
		}
		var p preparedSource
		json.Unmarshal(res.Body.Bytes(), &p)
		return p
	}
	prepared := prepare()
	payload, _ := json.Marshal(map[string]any{"prepared": prepared, "folder": "research/references"})
	res := call(app, "POST", "/api/sources/import", string(payload), testToken)
	if res.Code != 200 {
		t.Fatal("folder import", res.Code, res.Body.String())
	}
	var note noteOutput
	json.Unmarshal(res.Body.Bytes(), &note)
	if !strings.HasPrefix(note.Path, "research/references/") {
		t.Fatal("destination ignored", note.Path)
	}
	// Changing destinations on a retry must not duplicate the capture.
	payload, _ = json.Marshal(map[string]any{"prepared": prepared, "folder": "elsewhere"})
	if res = call(app, "POST", "/api/sources/import", string(payload), testToken); res.Code != 409 {
		t.Fatal("duplicate capture", res.Code)
	}
	refreshed := prepare()
	payload, _ = json.Marshal(map[string]any{"prepared": refreshed, "path": note.Path, "revision": note.Revision, "folder": "elsewhere"})
	if res = call(app, "POST", "/api/sources/import", string(payload), testToken); res.Code < 400 {
		t.Fatal("refresh moved source")
	}
	payload, _ = json.Marshal(map[string]any{"prepared": refreshed, "path": note.Path, "revision": note.Revision})
	if res = call(app, "POST", "/api/sources/import", string(payload), testToken); res.Code != 200 {
		t.Fatal("refresh failed", res.Code, res.Body.String())
	}
	var updated noteOutput
	json.Unmarshal(res.Body.Bytes(), &updated)
	if updated.Path != note.Path {
		t.Fatal("refresh changed location")
	}
	before, _ := store.Status(context.Background())
	for _, folder := range []string{"../escape", "/tmp", ".git", "a/../b", "a//b"} {
		payload, _ = json.Marshal(map[string]any{"prepared": prepare(), "folder": folder})
		if res = call(app, "POST", "/api/sources/import", string(payload), testToken); res.Code < 400 {
			t.Fatal("unsafe folder", folder)
		}
	}
	after, _ := store.Status(context.Background())
	if after.Dirty || before.Revision != after.Revision {
		t.Fatal("rejected imports changed workspace")
	}
	client := actionClient(t, app, catalog.Bootstrap, "notes:read notes:write")
	raw := actionCall(t, client, "archive_source", map[string]any{"project": "default", "filename": "root.txt", "data": base64.StdEncoding.EncodeToString([]byte("Root evidence")), "folder": ""}, false)
	var root noteOutput
	json.Unmarshal(raw, &root)
	if strings.Contains(root.Path, "/") {
		t.Fatal("root destination ignored", root.Path)
	}
}
