package workspace

import (
	"path"
	"strings"
)

// This catalog is shared by storage, downloads, browser discovery, and MCP.
// Extraction stays in internal/sources; these are the supported file contracts.
type SourceFormat struct {
	Name           string   `json:"name"`
	Extensions     []string `json:"extensions"`
	MIME           string   `json:"mime"`
	Description    string   `json:"description"`
	DescribeImages bool     `json:"describeImages,omitempty"`
	MaxBytes       int      `json:"maxBytes"`
}

func SourceFormats() []SourceFormat {
	formats := []SourceFormat{
		{"PDF", []string{".pdf"}, "application/pdf", "Local Poppler text and English Tesseract OCR with physical page anchors. Tables, equations and reading order need verification; hosted OCR is explicit.", true, 0},
		{"Webpage", []string{".html", ".htm"}, "text/html", "Static page text and headings; no scripts or linked assets.", false, 0},
		{"Text", []string{".txt"}, "text/plain", "UTF-8 or BOM-marked UTF-16 text.", false, 0},
		{"Word", []string{".docx"}, "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "Main document paragraphs, headings, and tables; no pagination or embedded objects.", false, 0},
		{"EPUB", []string{".epub"}, "application/epub+zip", "Chapter text in spine order; no DRM decryption.", false, 0},
		{"Excel", []string{".xlsx"}, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "Sheets and stored cell values, including hidden content; formulas are never calculated.", false, 0},
		{"CSV", []string{".csv"}, "text/csv", "Comma-delimited records, including quoted multiline fields.", false, 0},
		{"TSV", []string{".tsv"}, "text/tab-separated-values", "Tab-delimited records.", false, 0},
		{"PNG", []string{".png"}, "image/png", "English OCR with page anchors; OCR may contain errors.", true, 0},
		{"JPEG", []string{".jpg", ".jpeg"}, "image/jpeg", "English OCR with page anchors; OCR may contain errors.", true, 0},
		{"TIFF", []string{".tiff", ".tif"}, "image/tiff", "English OCR for each frame; OCR may contain errors.", true, 0},
		{"WebP", []string{".webp"}, "image/webp", "English OCR with page anchors; OCR may contain errors.", true, 0},
		{"Bitmap", []string{".bmp"}, "image/bmp", "English OCR with page anchors; OCR may contain errors.", true, 0},
		{"PowerPoint", []string{".pptx"}, "application/vnd.openxmlformats-officedocument.presentationml.presentation", "Slide text and tables; animations, notes, and embedded media are not transcribed.", true, 0},
		{"Email", []string{".eml"}, "message/rfc822", "Message headers and body; attachment names only.", false, 0},
		{"Outlook message", []string{".msg"}, "application/vnd.ms-outlook", "Message headers and body; attachment names only.", false, 0},
		{"OpenDocument text", []string{".odt"}, "application/vnd.oasis.opendocument.text", "Document text and tables; original layout is not retained.", false, 0},
		{"OpenDocument spreadsheet", []string{".ods"}, "application/vnd.oasis.opendocument.spreadsheet", "Stored cell values; formulas are not recalculated.", false, 0},
		{"OpenDocument presentation", []string{".odp"}, "application/vnd.oasis.opendocument.presentation", "Slide text and tables; embedded media are not transcribed.", false, 0},
		{"Pages", []string{".pages"}, "application/vnd.apple.pages", "Document text and tables; complex layout may be omitted.", false, 0},
		{"Keynote", []string{".key"}, "application/vnd.apple.keynote", "Slide text and tables; animations and embedded media are not transcribed.", false, 0},
		{"WAV", []string{".wav"}, "audio/wav", "Explicit hosted transcription with time segments; up to 5 minutes. Transcripts may contain errors.", false, 0},
		{"MP3", []string{".mp3"}, "audio/mpeg", "Explicit hosted transcription with time segments; up to 5 minutes. Transcripts may contain errors.", false, 0},
		{"M4A", []string{".m4a"}, "audio/mp4", "Explicit hosted transcription with time segments; up to 5 minutes. Transcripts may contain errors.", false, 0},
		{"AAC", []string{".aac"}, "audio/aac", "Explicit hosted transcription with time segments; up to 5 minutes. Transcripts may contain errors.", false, 0},
		{"Ogg", []string{".ogg"}, "audio/ogg", "Explicit hosted transcription with time segments; up to 5 minutes. Transcripts may contain errors.", false, 0},
		{"FLAC", []string{".flac"}, "audio/flac", "Explicit hosted transcription with time segments; up to 5 minutes. Transcripts may contain errors.", false, 0},
		{"MP4", []string{".mp4"}, "video/mp4", "Explicit hosted audio transcription; up to 5 minutes. Video frames are not analyzed.", false, 0},
		{"QuickTime", []string{".mov"}, "video/quicktime", "Explicit hosted audio transcription; up to 5 minutes. Video frames are not analyzed.", false, 0},
		{"AVI", []string{".avi"}, "video/x-msvideo", "Explicit hosted audio transcription; up to 5 minutes. Video frames are not analyzed.", false, 0},
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
