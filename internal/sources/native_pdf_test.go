package sources

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestNativePDFMixedAndBlankPages(t *testing.T) {
	for _, tool := range []string{"pdftotext", "pdfimages", "pdftoppm", "tesseract"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("requires Poppler and Tesseract: " + tool)
		}
	}
	for _, name := range []string{"mixed", "scanned", "searchable"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/" + name + ".pdf")
			if err != nil {
				t.Fatal(err)
			}
			got, err := Prepare(t.Context(), name+".pdf", "", raw)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got.Record.Extractor, "Tesseract") || got.Record.Extraction != "extracted" {
				t.Fatalf("native extraction missing: %+v", got.Record)
			}
			times := 1
			if name == "scanned" {
				times = 2
			}
			if strings.Count(got.Text, "Amber evidence") != times || strings.Count(got.Text, "Cobalt findings") != times {
				t.Fatalf("lost or duplicated evidence: %s", got.Text)
			}
			if name == "mixed" && strings.Count(got.Text, "Native header") != 1 {
				t.Fatalf("native text duplicated: %s", got.Text)
			}
			if name == "scanned" && !strings.Contains(got.Text, "## Page 2\n\n_No readable text") {
				t.Fatalf("blank physical page lost: %s", got.Text)
			}
			if string(got.Original) != string(raw) {
				t.Fatal("original changed")
			}
		})
	}
}

func TestNativePDFQualityCorpusRetainsHonestWarnings(t *testing.T) {
	for _, tool := range []string{"pdftotext", "pdfimages", "pdftoppm", "tesseract"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("requires Poppler/Tesseract")
		}
	}
	for _, name := range []string{"columns", "table", "equations", "rotated", "poor"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/" + name + ".pdf")
			if err != nil {
				t.Fatal(err)
			}
			got, err := Prepare(t.Context(), name+".pdf", "", raw)
			if err != nil || !strings.Contains(got.Text, "## Page 1") || string(got.Original) != string(raw) || !strings.Contains(got.Record.Warning, "Verify against the retained original") {
				t.Fatalf("quality corpus %s: %s %v", name, got.Record.Warning, err)
			}
			// Local OCR cannot certify numeric cells or mathematical notation. This
			// fixture checks the warning/original boundary, not invented fidelity.
			if name == "table" && !strings.Contains(got.Text, "Amber") {
				if got.Record.Extraction != "partial" || !strings.Contains(got.Record.Warning, "incomplete") {
					t.Fatal("unreadable ruled table reported complete", got.Record)
				}
			} else if name != "equations" && (!strings.Contains(got.Text, "Amber") || !strings.Contains(got.Text, "Cobalt")) {
				t.Fatalf("quality corpus readable words missing: %q", got.Text)
			}
		})
	}
}
