package workspace

import (
	"path"
	"strings"
)

// This catalog is shared by storage, downloads, browser discovery, and MCP.
// Extraction stays in internal/sources; these are the supported file contracts.
type SourceFormat struct {
	Name        string   `json:"name"`
	Extensions  []string `json:"extensions"`
	MIME        string   `json:"mime"`
	Description string   `json:"description"`
	MaxBytes    int      `json:"maxBytes"`
}

func SourceFormats() []SourceFormat {
	formats := []SourceFormat{
		{"PDF", []string{".pdf"}, "application/pdf", "Local Poppler text layer with physical page anchors; scanned regions, figures and images are a connected agent's to read. Tables, equations and reading order need verification.", 0},
		{"Webpage", []string{".html", ".htm"}, "text/html", "Static page text and headings; no scripts or linked assets.", 0},
		{"Text", []string{".txt"}, "text/plain", "UTF-8 or BOM-marked UTF-16 text.", 0},
		{"Word", []string{".docx"}, "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "Main document paragraphs, headings, and tables; no pagination or embedded objects.", 0},
		{"EPUB", []string{".epub"}, "application/epub+zip", "Chapter text in spine order; no DRM decryption.", 0},
		{"Excel", []string{".xlsx"}, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "Sheets and stored cell values, including hidden content; formulas are never calculated.", 0},
		{"CSV", []string{".csv"}, "text/csv", "Comma-delimited records, including quoted multiline fields.", 0},
		{"TSV", []string{".tsv"}, "text/tab-separated-values", "Tab-delimited records.", 0},
		{"PNG", []string{".png"}, "image/png", "Stored as the original; a connected agent reads it and adds its text.", 0},
		{"JPEG", []string{".jpg", ".jpeg"}, "image/jpeg", "Stored as the original; a connected agent reads it and adds its text.", 0},
		{"TIFF", []string{".tiff", ".tif"}, "image/tiff", "Stored as the original; a connected agent reads it and adds its text.", 0},
		{"WebP", []string{".webp"}, "image/webp", "Stored as the original; a connected agent reads it and adds its text.", 0},
		{"Bitmap", []string{".bmp"}, "image/bmp", "Stored as the original; a connected agent reads it and adds its text.", 0},
		{"PowerPoint", []string{".pptx"}, "application/vnd.openxmlformats-officedocument.presentationml.presentation", "Slide text and tables; animations, notes, images and embedded media are not read.", 0},
		{"Email", []string{".eml"}, "message/rfc822", "Message headers and body; attachment names only.", 0},
		{"Outlook message", []string{".msg"}, "application/vnd.ms-outlook", "Message headers and body; attachment names only.", 0},
		{"OpenDocument text", []string{".odt"}, "application/vnd.oasis.opendocument.text", "Document text and tables; original layout is not retained.", 0},
		{"OpenDocument spreadsheet", []string{".ods"}, "application/vnd.oasis.opendocument.spreadsheet", "Stored cell values; formulas are not recalculated.", 0},
		{"OpenDocument presentation", []string{".odp"}, "application/vnd.oasis.opendocument.presentation", "Slide text and tables; embedded media are not transcribed.", 0},
		{"Pages", []string{".pages"}, "application/vnd.apple.pages", "Stored as the original; a connected agent reads it and adds its text.", 0},
		{"Keynote", []string{".key"}, "application/vnd.apple.keynote", "Stored as the original; a connected agent reads it and adds its text.", 0},
		{"WAV", []string{".wav"}, "audio/wav", "Stored as the original; a connected agent reads it and adds its text.", 0},
		{"MP3", []string{".mp3"}, "audio/mpeg", "Stored as the original; a connected agent reads it and adds its text.", 0},
		{"M4A", []string{".m4a"}, "audio/mp4", "Stored as the original; a connected agent reads it and adds its text.", 0},
		{"AAC", []string{".aac"}, "audio/aac", "Stored as the original; a connected agent reads it and adds its text.", 0},
		{"Ogg", []string{".ogg"}, "audio/ogg", "Stored as the original; a connected agent reads it and adds its text.", 0},
		{"FLAC", []string{".flac"}, "audio/flac", "Stored as the original; a connected agent reads it and adds its text.", 0},
		{"MP4", []string{".mp4"}, "video/mp4", "Stored as the original; a connected agent reads it and adds its text.", 0},
		{"QuickTime", []string{".mov"}, "video/quicktime", "Stored as the original; a connected agent reads it and adds its text.", 0},
		{"AVI", []string{".avi"}, "video/x-msvideo", "Stored as the original; a connected agent reads it and adds its text.", 0},
	}
	for i := range formats {
		formats[i].MaxBytes = MaxSourceBytes()
	}
	return formats
}
func SourceFormatForName(name string) SourceFormat {
	ext := strings.ToLower(path.Ext(name))
	for _, format := range SourceFormats() {
		for _, candidate := range format.Extensions {
			if ext == candidate {
				return format
			}
		}
	}
	return SourceFormat{}
}
func SourceFormatForMIME(mime string) SourceFormat {
	for _, format := range SourceFormats() {
		if format.MIME == mime {
			return format
		}
	}
	return SourceFormat{}
}
