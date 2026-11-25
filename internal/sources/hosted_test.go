package sources

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/pg/pgtest"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func hostedVisionFixture(t *testing.T, number int, blocks []documentBlock) *Hosted {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request["store"] != false || request["tools"] != nil || request["max_output_tokens"] != float64(2048) {
			t.Error("unbounded or retained hosted request", request["store"], request["tools"])
		}
		result, _ := json.Marshal(map[string]any{"partial": false, "sections": []documentSection{{Number: number, Blocks: blocks}}})
		json.NewEncoder(w).Encode(map[string]any{"status": "completed", "model": "synthetic-vision", "usage": map[string]int{"input_tokens": 20, "output_tokens": 8}, "output": []any{map[string]any{"type": "message", "content": []any{map[string]string{"type": "output_text", "text": string(result)}}}}})
	}))
	t.Cleanup(server.Close)
	ledger, err := decision.OpenLedger(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ledger.Close)
	hosted, err := NewHosted(HostedConfig{Endpoint: server.URL, Model: "synthetic-vision", Local: true, ContextTokens: 32768, OutputTokens: 2048}, ledger, decision.Scope{Actor: "synthetic", Notebook: "notes", Feature: "extraction", Run: "source-fixture"}, func(context.Context) error { return nil }, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return hosted
}
func TestHostedPDFOCRPreservesPhysicalSelectionAndUnselectedPages(t *testing.T) {
	for _, name := range []string{"pdftotext", "pdftoppm", "pdfimages", "tesseract"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip("requires Poppler and Tesseract")
		}
	}
	raw, err := os.ReadFile("testdata/scanned.pdf")
	if err != nil {
		t.Fatal(err)
	}
	hosted := hostedVisionFixture(t, 3, []documentBlock{{Kind: "text", Text: "Cobalt external evidence"}, {Kind: "table", Rows: [][]string{{"Name", "Value"}, {"Amber", "17"}}}})
	got, err := PrepareWithOptions(t.Context(), "scan.pdf", "", raw, Options{ExternalOCR: true, FirstPage: 3, LastPage: 3, hosted: hosted})
	if err != nil || !strings.Contains(got.Text, "## Page 1") || !strings.Contains(got.Text, "## Page 2") || !strings.Contains(got.Text, "## Page 3") || strings.Count(got.Text, "Cobalt external evidence") != 1 || !strings.Contains(got.Text, "| Amber | 17 |") || string(got.Original) != string(raw) {
		t.Fatalf("hosted physical replacement: %q %v", got.Text, err)
	}
}
func TestHostedImageInterpretationRemainsLabeledAndOptedIn(t *testing.T) {
	requireOCR(t)
	if _, err := exec.LookPath("tesseract"); err != nil {
		t.Skip("requires Tesseract")
	}
	raw, err := os.ReadFile("testdata/reference.png")
	if err != nil {
		t.Fatal(err)
	}
	hosted := hostedVisionFixture(t, 1, []documentBlock{{Kind: "text", Text: "A blue flower [unsafe](javascript:evil)"}})
	got, err := PrepareWithOptions(t.Context(), "figure.png", "", raw, Options{DescribeImages: true, hosted: hosted})
	if err != nil || !strings.Contains(got.Text, "Amber") || !strings.Contains(got.Text, "AI image descriptions (unverified)") || strings.Contains(got.Text, "[unsafe](") {
		t.Fatalf("hosted interpretation: %q %v", got.Text, err)
	}
	if _, err := PrepareWithOptions(t.Context(), "figure.png", "", raw, Options{DescribeImages: true}); err == nil {
		t.Fatal("direct importer bypassed hosted policy boundary")
	}
}

func TestHostedPresentationInterpretationPreservesNativeSlideText(t *testing.T) {
	rawImage, err := os.ReadFile("testdata/reference.png")
	if err != nil {
		t.Fatal(err)
	}
	raw := packageBytes(t, map[string]string{
		"ppt/presentation.xml":            `<p:presentation xmlns:p="urn:p" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst><p:sldId id="1" r:id="s1"/></p:sldIdLst></p:presentation>`,
		"ppt/_rels/presentation.xml.rels": `<Relationships><Relationship Id="s1" Target="slides/slide1.xml"/></Relationships>`,
		"ppt/slides/slide1.xml":           `<p:sld xmlns:p="urn:p" xmlns:a="urn:a"><p:spTree><a:p><a:r><a:t>Amber slide evidence</a:t></a:r></a:p></p:spTree></p:sld>`,
		"ppt/media/image1.png":            string(rawImage),
	})
	hosted := hostedVisionFixture(t, 1, []documentBlock{{Kind: "text", Text: "Cobalt visible diagram"}})
	got, err := PrepareWithOptions(t.Context(), "slides.pptx", "", raw, Options{DescribeImages: true, hosted: hosted})
	if err != nil || !strings.Contains(got.Text, "## Slide 1") || !strings.Contains(got.Text, "Amber slide evidence") || !strings.Contains(got.Text, "Cobalt visible diagram") || !strings.Contains(got.Text, "unverified") {
		t.Fatalf("native and hosted presentation content: %q %v", got.Text, err)
	}
}

func TestHostedVisionRejectsBlocksOutsideRequestedSchema(t *testing.T) {
	hosted := hostedVisionFixture(t, 1, []documentBlock{{Kind: "heading", Text: "Provider ignored schema"}})
	if _, err := hosted.vision(t.Context(), []hostedImage{{number: 1, data: []byte("synthetic boundary")}}, true); err == nil {
		t.Fatal("hosted provider returned a block outside the requested text/table schema")
	}
}
