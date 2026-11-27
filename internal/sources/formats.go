package sources

import (
	"forgetmenote/internal/integrations"
	"forgetmenote/internal/workspace"
)

// formatIntegration names the optional integration that extracts a format,
// or nothing for the formats every build extracts.
func formatIntegration(format workspace.SourceFormat) string {
	if format.Extensions[0] == ".msg" {
		return "outlook"
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
		out = append(out, format)
	}
	return out
}
