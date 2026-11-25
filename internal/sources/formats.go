package sources

import (
	"strings"

	"forgetmenote/internal/integrations"
	"forgetmenote/internal/workspace"
)

// formatIntegration names the optional integration that extracts a format,
// or nothing for the formats every build extracts.
func formatIntegration(format workspace.SourceFormat) string {
	switch format.Extensions[0] {
	case ".msg":
		return "outlook"
	case ".pages", ".key":
		return "iwork"
	}
	if strings.HasPrefix(format.MIME, "image/") {
		return "ocr"
	}
	return ""
}

func formatIncluded(format workspace.SourceFormat) error {
	if name := formatIntegration(format); name != "" {
		if err := integrations.Check(name); err != nil {
			return rejection(err.Error())
		}
	}
	return nil
}

// Formats are the source formats this build imports: the shared catalog, which
// still describes stored originals of every format, less those whose optional
// extractor the build leaves out.
func Formats() []workspace.SourceFormat {
	var out []workspace.SourceFormat
	for _, format := range workspace.SourceFormats() {
		if formatIncluded(format) != nil {
			continue
		}
		if format.MIME == "application/pdf" && !integrations.Included("ocr") {
			format.Description = "Local Poppler text with physical page anchors. This build leaves out local OCR, so scanned regions need hosted OCR. Tables, equations and reading order need verification."
		}
		out = append(out, format)
	}
	return out
}
