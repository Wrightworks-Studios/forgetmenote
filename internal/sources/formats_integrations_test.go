package sources

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgetmenote/internal/integrations"
	"forgetmenote/internal/workspace"
)

// skipOmitted skips a case whose source format this build does not extract.
func skipOmitted(t *testing.T, filename string) {
	t.Helper()
	if err := formatIncluded(workspace.SourceFormatForName(filename)); err != nil {
		t.Skip(err)
	}
}

// Each build offers exactly the formats it extracts, and refuses the others by
// the integration that would extract them.
func TestFormatsOfferWhatTheBuildExtracts(t *testing.T) {
	offered := map[string]bool{}
	for _, format := range Formats() {
		offered[format.Extensions[0]] = true
	}
	for _, extension := range []string{".pdf", ".html", ".txt", ".docx", ".xlsx", ".epub", ".pptx", ".eml", ".odt", ".png", ".tiff", ".wav", ".mp4", ".pages", ".key"} {
		if !offered[extension] {
			t.Fatal("core format not offered", extension)
		}
	}
	for _, tc := range []struct{ file, integration string }{
		{"reference.msg", "outlook"},
	} {
		format := workspace.SourceFormatForName(tc.file)
		if offered[format.Extensions[0]] != integrations.Included(tc.integration) {
			t.Fatal(tc.file, "offered", offered[format.Extensions[0]])
		}
		if integrations.Included(tc.integration) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("testdata", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		_, err = Prepare(t.Context(), filepath.Base(tc.file), "", raw)
		if !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "fmn_omit_"+tc.integration) {
			t.Fatal(tc.file, err)
		}
	}
}
