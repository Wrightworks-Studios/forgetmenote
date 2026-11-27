package sources

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"forgetmenote/internal/workspace"
)

// SuppliedText is a connected agent's own extraction of the original it
// uploads: physically numbered sections of plain text. The server labels the
// sections as its converters do, so citations and anchors work alike, and
// keeps every supplied word literal.
type SuppliedText struct {
	Unit     string            `json:"unit" jsonschema:"What the sections number: page (a PDF page or an image frame), slide, sheet, segment (a stretch of a recording) or section."`
	Sections []SuppliedSection `json:"sections" jsonschema:"Every section in order, numbered from 1 with none skipped; a section without text keeps its number with empty text."`
}

type SuppliedSection struct {
	Number int    `json:"number" jsonschema:"The section's physical number, from 1."`
	Title  string `json:"title,omitempty" jsonschema:"Optional one-line title of at most 200 characters, such as a slide title or a segment's time range."`
	Text   string `json:"text" jsonschema:"The section's text as read from the original. It is stored literally: Markdown, links and HTML show as typed."`
}

// SuppliedExtraction labels a source note whose text its uploader supplied.
// It is a fifth extraction state beside the server's extracted, partial,
// no-text and failed.
const SuppliedExtraction = "client-supplied"

const maxSuppliedSections = 10000

// PrepareSupplied makes the capture of an original with the uploader's own
// extraction. No server extraction runs, whatever the format: the supplied
// text replaces it, and the record says it is the client's and unverified.
// The original is required and stored as uploaded.
func PrepareSupplied(filename, originalURL string, data []byte, extractor string, text SuppliedText) (workspace.SourceCapture, error) {
	var out workspace.SourceCapture
	if len(data) == 0 {
		return out, rejection("text needs the original it was extracted from: send the file as data")
	}
	if len(data) > workspace.MaxSourceBytes() {
		return out, rejection("source exceeds FMN_LIMIT_SOURCE_BYTES")
	}
	if !workspace.SourceURL(originalURL) {
		return out, rejection("invalid original URL")
	}
	extractor = strings.TrimSpace(extractor)
	if extractor == "" || !utf8.ValidString(extractor) || utf8.RuneCountInString(extractor) > 200 || strings.IndexFunc(extractor, unicode.IsControl) >= 0 {
		return out, rejection("name the agent and model that extracted the text in extractor: one line of at most 200 characters")
	}
	// The format follows the server's own rule: a PDF by its bytes, anything
	// else by its name, and no PDF name without PDF bytes. The supplied text
	// needs no extractor, so a build without one still stores its originals.
	format := workspace.SourceFormatForName(filename)
	if bytes.HasPrefix(data, []byte("%PDF-")) {
		format = workspace.SourceFormatForMIME("application/pdf")
	} else if format.MIME == "application/pdf" {
		format = workspace.SourceFormat{}
	}
	if format.MIME == "" {
		return out, rejection("unsupported or invalid source format; use a supported original file (see source_formats)")
	}
	result, err := suppliedResult(extractor, text)
	if err != nil {
		return out, err
	}
	markdown, readable, _, err := renderSections(result)
	if err != nil {
		return out, refused(err)
	}
	if !readable {
		return out, rejection("the supplied text has no text in any section; omit text to store the original alone")
	}
	id := make([]byte, 16)
	rand.Read(id)
	out.Original = data
	out.Title = filepath.Base(filename)
	if out.Title == "." || out.Title == "/" || out.Title == "" {
		out.Title = "Archived source"
	}
	if len(out.Title) > 300 {
		out.Title = string([]rune(out.Title)[:min(utf8.RuneCountInString(out.Title), 75)])
	}
	out.Text = markdown
	out.Record = workspace.SourceRecord{
		ID: hex.EncodeToString(id), URL: originalURL, Method: "uploaded", CapturedAt: time.Now().UTC().Format(time.RFC3339Nano),
		SHA256: workspace.ContentHash(data), MIME: format.MIME, Extractor: extractor, Extraction: SuppliedExtraction,
		Warning:    "Text supplied by the uploading client and not verified by the server against the original. Check quotations and figures against the original.",
		TextSHA256: workspace.ContentHash([]byte(markdown)),
	}
	return out, nil
}

// suppliedResult checks the supplied sections, which are untrusted input,
// and gives them the shape the server's converters produce.
func suppliedResult(extractor string, text SuppliedText) (documentResult, error) {
	if _, ok := sectionLabels[text.Unit]; !ok {
		return documentResult{}, rejection("text.unit must be page, slide, sheet, segment or section")
	}
	if len(text.Sections) == 0 {
		return documentResult{}, rejection("text needs at least one section")
	}
	if len(text.Sections) > maxSuppliedSections {
		return documentResult{}, rejection(fmt.Sprintf("text has more than %d sections", maxSuppliedSections))
	}
	result := documentResult{Extractor: extractor, Unit: text.Unit, Sections: make([]documentSection, len(text.Sections))}
	size := 0
	for i, section := range text.Sections {
		if section.Number != i+1 {
			return documentResult{}, rejection(fmt.Sprintf("number sections from 1 with none skipped: section %d is numbered %d", i+1, section.Number))
		}
		if !validSectionTitle(section.Title) {
			return documentResult{}, rejection(fmt.Sprintf("section %d's title must be one line of at most 200 characters", section.Number))
		}
		if !utf8.ValidString(section.Text) || strings.ContainsRune(section.Text, 0) {
			return documentResult{}, rejection(fmt.Sprintf("section %d's text must be UTF-8 without NUL characters", section.Number))
		}
		// The rendered text is bounded too; this stops a long upload early.
		if size += len(section.Text) + len(section.Title); size > workspace.MaxSourceText() {
			return documentResult{}, rejection("supplied text exceeds FMN_LIMIT_SOURCE_TEXT_BYTES")
		}
		result.Sections[i] = documentSection{Number: section.Number, Title: section.Title, Blocks: []documentBlock{{Kind: "text", Text: section.Text}}}
	}
	return result, nil
}

// OriginalOnly is the extraction of a source the server stored without
// reading, because a connected agent extracts its format.
const OriginalOnly = "original-only"

// AwaitingText says whether an archived source lacks usable text: the server
// stored its original only, its extraction failed, found no text, or is
// incomplete (a PDF's pages without a text layer among them), so a connected
// agent should supply it.
func AwaitingText(record workspace.SourceRecord) bool {
	switch record.Extraction {
	case OriginalOnly, "no-text", "failed", "partial":
		return true
	}
	return record.TextSHA256 == workspace.ContentHash(nil)
}

// ExtractedBy says who extracts a format's text: "server", or "agent" when the
// connected agent should read the original itself and upload its own text
// with it. For a format the server reads, agentPart names what of it is still
// the agent's.
func ExtractedBy(format workspace.SourceFormat) (by, agentPart string) {
	switch {
	case strings.HasPrefix(format.MIME, "image/"), strings.HasPrefix(format.MIME, "audio/"), strings.HasPrefix(format.MIME, "video/"):
		return "agent", ""
	case format.Extensions[0] == ".pages", format.Extensions[0] == ".key":
		return "agent", ""
	case format.MIME == "application/pdf":
		return "server", "scanned or image-only pages: when a PDF has them, upload your own text of the whole document"
	}
	return "server", ""
}
