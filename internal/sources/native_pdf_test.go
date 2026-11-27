package sources

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The real Poppler reads each fixture's text layer and nothing else. The
// scanned fixtures, which have none, wait for a connected agent.
func TestNativePDFReadsTheTextLayerOnly(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("requires Poppler's pdftotext")
	}
	for name, test := range map[string]struct {
		extraction string
		words      []string
	}{
		"searchable": {"extracted", []string{"Amber evidence", "Cobalt findings"}},
		// The native header is read once; the scanned region beside it is not.
		"mixed":     {"extracted", []string{"Native header"}},
		"scanned":   {"no-text", nil},
		"columns":   {"no-text", nil},
		"table":     {"no-text", nil},
		"equations": {"no-text", nil},
		"rotated":   {"no-text", nil},
		"poor":      {"no-text", nil},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/" + name + ".pdf")
			if err != nil {
				t.Fatal(err)
			}
			got, err := Prepare(t.Context(), name+".pdf", "", raw)
			if err != nil {
				t.Fatal(err)
			}
			if got.Record.Extraction != test.extraction || AwaitingText(got.Record) != (test.extraction != "extracted") || string(got.Original) != string(raw) || !strings.Contains(got.Text, "## Page 1") {
				t.Fatalf("record: %+v\n%s", got.Record, got.Text)
			}
			for _, words := range test.words {
				if strings.Count(got.Text, words) != 1 {
					t.Fatalf("%q lost or duplicated: %s", words, got.Text)
				}
			}
			if name == "scanned" && !strings.Contains(got.Text, "## Page 2\n\n_No text layer") {
				t.Fatalf("physical page lost: %s", got.Text)
			}
		})
	}
}
