package sources

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/workspace"
)

func TestDelimitedImportBeyondLegacyRowLimit(t *testing.T) {
	var data strings.Builder
	for i := 0; i < 10001; i++ {
		fmt.Fprintf(&data, "row%d,%d\n", i, i)
	}
	got, err := Prepare(t.Context(), "results.csv", "", []byte(data.String()))
	if err != nil || !strings.Contains(got.Text, "row10000") {
		t.Fatalf("lost imported rows: %v", err)
	}
}

func TestCaptureRejectsInternalURLs(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "http://127.0.0.1/x", "http://[::1]/", "http://169.254.169.254/latest/meta-data", "https://10.0.0.1/x", "http://user:pass@example.org/", "https://example.org:8080/"} {
		if ValidateURL(u) == nil {
			t.Fatal("accepted", u)
		}
	}
	for _, s := range []string{"::ffff:127.0.0.1", "100.100.100.200", "192.168.1.1", "fe80::1", "0.0.0.0", "64:ff9b::7f00:1", "64:ff9b:1::a00:1", "fec0::1"} {
		if PublicAddress(netip.MustParseAddr(s)) {
			t.Fatal("public", s)
		}
	}
	if !PublicAddress(netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("public address rejected")
	}
	if _, _, _, err := Fetch(context.Background(), "http://localhost/"); err == nil {
		t.Fatal("DNS resolving to loopback accepted")
	}
}
func TestHTMLExtractionDoesNotExecuteOrInventLinks(t *testing.T) {
	data := []byte(`<title>Grammar</title><h1>Evidence</h1><p>Rare amber root.</p><script>secret()</script><iframe>evil</iframe><p hidden>hidden</p><p>[evil](javascript:alert(1)) &lt;img src=x&gt;</p>`)
	capture, err := Prepare(context.Background(), "a.html", "https://example.org/reference", data)
	if err != nil {
		t.Fatal(err)
	}
	if capture.Title != "Grammar" || strings.Contains(capture.Text, "secret()") || strings.Contains(capture.Text, "hidden") || !strings.Contains(capture.Text, "## Evidence") || strings.Contains(capture.Text, "[evil](") {
		t.Fatal("extraction", capture.Text)
	}
	if string(capture.Original) != string(data) || capture.Record.Method != "uploaded" || capture.Record.FinalURL != "" {
		t.Fatal("uploaded bytes incorrectly claim a fetch")
	}
}

func TestExtractedWordsKeepSearchablePunctuation(t *testing.T) {
	capture, err := Prepare(context.Background(), "x.html", "", []byte("<p>A forget-me-not flower. Version 1.2!</p>"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(capture.Text, "forget-me-not") || !strings.Contains(capture.Text, "1.2!") {
		t.Fatal("ordinary punctuation was obscured for literal search", capture.Text)
	}
}

func TestCanceledSourceDoesNoExtraction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Prepare(ctx, "page.html", "", []byte("<p>hello</p>")); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

// Every path into HTML extraction reports a token over the limit the same way.
func TestOversizedHTMLTokenIsRejectedInEveryContainer(t *testing.T) {
	reference := func(name string) []byte {
		raw, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for name, test := range map[string]struct {
		filename string
		limit    int
		document func(token string) []byte
	}{
		"page": {"page.html", 1024, func(token string) []byte { return []byte(token) }},
		"email": {"message.eml", 1024, func(token string) []byte {
			return []byte("From: a@example.org\r\nContent-Type: text/html\r\n\r\n" + token)
		}},
		"EPUB chapter": {"book.epub", 4096, func(token string) []byte {
			book, err := openPackage(reference("reference.epub"))
			if err != nil {
				t.Fatal(err)
			}
			parts := map[string]string{}
			for name := range book.files {
				data, err := book.read(name)
				if err != nil {
					t.Fatal(err)
				}
				parts[name] = string(data)
			}
			parts["Book/first.xhtml"] = token
			return packageBytes(t, parts)
		}},
		"Outlook body": {"message.msg", 96, func(token string) []byte {
			// As in TestNativeOutlookBodyFallbackAndCorruptCounts: the fixture's
			// plain text body becomes its HTML body, in the same mini-stream slot.
			data := reference("reference.msg")
			before, _ := utf16le("__substg1.0_1000001F")
			after, _ := utf16le("__substg1.0_10130102")
			data = bytes.ReplaceAll(data, before, after)
			binary.LittleEndian.PutUint64(data[1528:1536], uint64(len(token)))
			clear(data[2176:2304])
			copy(data[2176:], token)
			return data
		}},
	} {
		t.Run(name, func(t *testing.T) {
			skipOmitted(t, test.filename)
			t.Setenv("FMN_LIMIT_SOURCE_TEXT_BYTES", strconv.Itoa(test.limit))
			token := "<!--" + strings.Repeat("x", test.limit-4)
			_, err := Prepare(t.Context(), test.filename, "", test.document(token))
			if !errors.Is(err, ErrRejected) || err.Error() != "HTML token exceeds FMN_LIMIT_SOURCE_TEXT_BYTES" {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// lateCancel reports cancellation once it has been checked more than after
// times; with after zero it never does.
type lateCancel struct {
	context.Context
	after, checks int
}

func (c *lateCancel) Err() error {
	c.checks++
	if c.after > 0 && c.checks > c.after {
		return context.Canceled
	}
	return nil
}

// The job worker keeps a canceled job resumable, so a parser stopped at any
// point must not read as a refused document.
func TestCancellationIsNeverARejection(t *testing.T) {
	documents := map[string][]byte{
		"page.html":   []byte("<title>Amber</title><p>Cobalt</p>"),
		"message.eml": []byte("From: a@example.org\r\nContent-Type: text/html\r\n\r\n<p>Cobalt</p>"),
	}
	for _, extension := range []string{"txt", "csv", "tsv", "docx", "xlsx", "epub", "msg"} {
		raw, err := os.ReadFile("testdata/reference." + extension)
		if err != nil {
			t.Fatal(err)
		}
		documents["reference."+extension] = raw
	}
	for filename, document := range documents {
		t.Run(filename, func(t *testing.T) {
			skipOmitted(t, filename)
			whole := &lateCancel{Context: context.Background()}
			if _, err := Prepare(whole, filename, "", document); err != nil {
				t.Fatal(err)
			}
			for after := 1; after < whole.checks; after += max(1, whole.checks/100) {
				_, err := Prepare(&lateCancel{Context: context.Background(), after: after}, filename, "", document)
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled after %d of %d checks: %v", after, whole.checks, err)
				}
			}
		})
	}
}

// Text can outgrow the limit in escaping or conversion, after each format's
// own reader accepted it.
func TestTextOverTheLimitIsARejection(t *testing.T) {
	t.Setenv("FMN_LIMIT_SOURCE_TEXT_BYTES", "64")
	if _, err := Prepare(t.Context(), "notes.txt", "", []byte(strings.Repeat("<", 64))); !errors.Is(err, ErrRejected) {
		t.Fatalf("escaped text: %v", err)
	}
	_, _, _, err := renderDocumentResult(documentResult{Extractor: "test", Unit: "page", Sections: []documentSection{{Number: 1, Blocks: []documentBlock{{Kind: "text", Text: strings.Repeat("x", 128)}}}}})
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("converted text: %v", err)
	}
}

func TestOptionTheFormatLacksIsARejection(t *testing.T) {
	if _, err := PrepareWithOptions(t.Context(), "notes.txt", "", []byte("Amber"), Options{DescribeImages: true, hosted: &Hosted{}}); !errors.Is(err, ErrRejected) {
		t.Fatalf("image descriptions: %v", err)
	}
	for _, options := range []Options{{Transcribe: true}, {ExternalOCR: true}, {DescribeImages: true}} {
		capture := workspace.SourceCapture{Record: workspace.SourceRecord{MIME: "text/plain"}}
		if err := (&Hosted{}).Process(t.Context(), &capture, options); !errors.Is(err, ErrRejected) {
			t.Fatalf("%+v: %v", options, err)
		}
	}
}

// A refused URL is the caller's; a fetch that failed says nothing about it.
func TestFailedFetchIsNotARejection(t *testing.T) {
	if _, _, _, err := Fetch(t.Context(), "http://localhost/"); !errors.Is(err, ErrRejected) {
		t.Fatalf("private host: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, _, _, err := Fetch(ctx, "http://forgetmenote-test.invalid/"); err == nil || errors.Is(err, ErrRejected) {
		t.Fatalf("unresolved host: %v", err)
	}
}

func TestHostedSelectionIsARejection(t *testing.T) {
	raw, err := os.ReadFile("testdata/reference.png")
	if err != nil {
		t.Fatal(err)
	}
	image := func() *workspace.SourceCapture {
		return &workspace.SourceCapture{Original: raw, Record: workspace.SourceRecord{MIME: "image/png"}}
	}
	for name, options := range map[string]Options{
		"page beyond the document": {ExternalOCR: true, FirstPage: 2, LastPage: 2},
		"two kinds of processing":  {ExternalOCR: true, DescribeImages: true},
	} {
		if err := (&Hosted{}).Process(t.Context(), image(), options); !errors.Is(err, ErrRejected) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	small := &Hosted{config: HostedConfig{Model: "synthetic", ContextTokens: 1}}
	if err := small.Process(t.Context(), image(), Options{ExternalOCR: true}); !errors.Is(err, ErrRejected) {
		t.Fatalf("context budget: %v", err)
	}
	// Selected frames skip local decoding, so hosted processing meets them first.
	_, err = PrepareWithOptions(t.Context(), "photo.png", "", []byte("not an image"), Options{ExternalOCR: true, FirstPage: 1, LastPage: 1, hosted: &Hosted{}})
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("undecodable selected frame: %v", err)
	}
}

// Hosted processing has no shared cancellation check to rely on.
func TestCanceledHostedDecodingIsNotARejection(t *testing.T) {
	raw, err := os.ReadFile("testdata/reference.png")
	if err != nil {
		t.Fatal(err)
	}
	process := func(ctx context.Context) error {
		capture := workspace.SourceCapture{Original: raw, Record: workspace.SourceRecord{MIME: "image/png"}}
		return (&Hosted{}).Process(ctx, &capture, Options{ExternalOCR: true})
	}
	whole := &lateCancel{Context: context.Background()}
	process(whole)
	for after := 1; after < whole.checks; after++ {
		if err := process(&lateCancel{Context: context.Background(), after: after}); errors.Is(err, ErrRejected) {
			t.Fatalf("canceled after %d of %d checks: %v", after, whole.checks, err)
		}
	}
}

func convert(t *testing.T, name string) error {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Prepare(t.Context(), filepath.Base(name), "", raw)
	return err
}

// fakeTool puts only a shell script of this name on the path.
func fakeTool(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestConverterRefusalIsARejection(t *testing.T) {
	fakeTool(t, "forgetmenote-iwork", "exit 1")
	if err := convert(t, "iwork/modern.pages"); !errors.Is(err, ErrRejected) {
		t.Fatalf("iWork: %v", err)
	}
	fakeTool(t, "ffprobe", "exit 1")
	if err := convert(t, "announcement.wav"); !errors.Is(err, ErrRejected) {
		t.Fatalf("media: %v", err)
	}
}

// A tool that did not get to judge the document says nothing about it.
func TestConverterFailureIsNotARejection(t *testing.T) {
	notRejected := func(name string) {
		t.Helper()
		if formatIncluded(workspace.SourceFormatForName(name)) != nil {
			return
		}
		if err := convert(t, name); err == nil || errors.Is(err, ErrRejected) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	t.Setenv("PATH", t.TempDir())
	for _, name := range []string{"iwork/modern.pages", "reference.png", "announcement.wav"} {
		notRejected(name)
	}
	fakeTool(t, "forgetmenote-iwork", "exit 2")
	notRejected("iwork/modern.pages")
	fakeTool(t, "ffprobe", "kill -KILL $$")
	notRejected("announcement.wav")
	// OCR reads a frame this server wrote, not the upload.
	fakeTool(t, "tesseract", "exit 1")
	notRejected("reference.png")
}

func TestHTMLWorkBudget(t *testing.T) {
	if _, err := Prepare(context.Background(), "page.html", "", []byte(strings.Repeat("<br>", 100001))); err == nil {
		t.Fatal("unbounded DOM accepted")
	}
}
