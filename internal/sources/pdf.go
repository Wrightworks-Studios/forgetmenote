package sources

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"forgetmenote/internal/procgroup"
	"forgetmenote/internal/workspace"
)

// PDFPage is the restart checkpoint of one physical page. Text is already
// escaped; only this owner generates the page anchors and warning labels.
type PDFPage struct {
	Number  int    `json:"number"`
	Text    string `json:"text"`
	Partial bool   `json:"partial,omitempty"`
	Warning string `json:"warning,omitempty"`
}

type pdfWord struct {
	Text string `xml:",chardata"`
}
type pdfGeometry struct {
	Flows []struct {
		Blocks []struct {
			Lines []struct {
				Words []pdfWord `xml:"word"`
			} `xml:"line"`
		} `xml:"block"`
	} `xml:"flow"`
}

func (p pdfGeometry) text() string {
	var out strings.Builder
	for _, flow := range p.Flows {
		for _, block := range flow.Blocks {
			for _, line := range block.Lines {
				for i, word := range line.Words {
					if i > 0 {
						out.WriteByte(' ')
					}
					out.WriteString(word.Text)
				}
				out.WriteByte('\n')
			}
			out.WriteByte('\n')
		}
	}
	return strings.TrimSpace(out.String())
}

// The server reads a PDF's text layer only. It renders, inventories and
// recognizes no page, so scanned regions, figures and images are a connected
// agent's to read from the retained original.
const (
	pdfExtractor = "ForgetMeNote PDF v3; Poppler text layer (physical pages)"
	pdfWarning   = "Only the PDF's text layer is read; scanned regions, figures and images are not. Tables, equations and reading order may contain errors. Verify against the retained original."
	// pdfAgentWarning follows the pages without a text layer.
	pdfAgentWarning = " A connected agent can read the original and add its text of the whole document."
)

var errExtractorOutput = errors.New("extractor output exceeds its budget")

func sourceCommand(ctx context.Context, dir string, limit int, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, extractorTimeout)
	defer cancel()
	cmd := localCommand(ctx, dir, name, args...)
	out := procgroup.Output{Limit: limit, Err: errExtractorOutput}
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s failed or exceeded its output budget", name)
	}
	return out.Bytes(), nil
}

func extractPDF(ctx context.Context, data []byte, options Options) (text, extractor, state, warning string) {
	select {
	case documentWork <- struct{}{}:
		defer func() { <-documentWork }()
	case <-ctx.Done():
		return "", "Poppler", "failed", "PDF extraction canceled."
	}
	extractor = sourceExtractor(pdfExtractor)
	warning = pdfWarning
	dir, err := scratchDirectory(ctx, "forgetmenote-pdf-")
	if err != nil {
		return "", extractor, "failed", "Could not prepare PDF extraction. Original retained."
	}
	defer os.RemoveAll(dir)
	filename := filepath.Join(dir, "source.pdf")
	if err = os.WriteFile(filename, data, 0600); err != nil {
		return "", extractor, "failed", "Could not prepare PDF extraction. Original retained."
	}
	raw, err := sourceCommand(ctx, dir, 8*workspace.MaxSourceText()+1<<20, "pdftotext", "-bbox-layout", "-enc", "UTF-8", filename, "-")
	if err != nil {
		return "", extractor, "failed", err.Error() + "; encrypted or malformed PDFs may require another copy. Original retained."
	}
	var doc struct {
		Pages []pdfGeometry `xml:"body>doc>page"`
	}
	if err = xml.Unmarshal(raw, &doc); err != nil || len(doc.Pages) == 0 || len(doc.Pages) > 10000 {
		return "", extractor, "failed", "Invalid or excessive PDF page geometry. Original retained."
	}
	var out boundedText
	partial, readable := false, false
	var blank []int
	for i, geometry := range doc.Pages {
		if ctx.Err() != nil {
			return "", extractor, "failed", "PDF extraction canceled. Original retained."
		}
		n := i + 1
		if options.pageStart != nil {
			if err := options.pageStart(n); err != nil {
				return "", extractor, "failed", err.Error()
			}
		}
		page := PDFPage{Number: n}
		if i < len(options.completedPages) && options.completedPages[i].Number == n {
			page = options.completedPages[i]
		} else {
			page.Text = workspace.EscapeSourceText(geometry.text())
			if options.pageDone != nil {
				if err := options.pageDone(page); err != nil {
					return "", extractor, "failed", err.Error()
				}
			}
		}
		// A checkpoint an earlier release stored may be partial.
		partial = partial || page.Partial
		if strings.TrimSpace(page.Text) == "" {
			blank = append(blank, n)
			page.Text = "_No text layer on this physical page._"
		} else {
			readable = true
		}
		if page.Warning != "" {
			page.Text += "\n\n" + workspace.EscapeSourceText(page.Warning)
		}
		if _, err := fmt.Fprintf(&out, "## Page %d\n\n%s\n\n", n, page.Text); err != nil {
			return "", extractor, "failed", "PDF text exceeds FMN_LIMIT_SOURCE_TEXT_BYTES. Original retained."
		}
	}
	state = "extracted"
	switch {
	case !readable:
		state = "no-text"
		warning = "No page has a text layer; the pages may be scans." + pdfAgentWarning + " " + warning
	case len(blank) > 0:
		state = "partial"
		warning = "Extraction is incomplete. " + pagesWithoutText(blank) + pdfAgentWarning + " " + warning
	case partial:
		state = "partial"
		warning = "Extraction is incomplete. " + warning
	}
	return out.String(), extractor, state, warning
}

// pagesWithoutText names the pages, for example "Pages 2, 5 and 7 have no
// text layer; they may be scans.", with at most ten numbers.
func pagesWithoutText(pages []int) string {
	if len(pages) == 1 {
		return fmt.Sprintf("Page %d has no text layer; it may be a scan.", pages[0])
	}
	var names []string
	for _, n := range pages[:min(len(pages), 10)] {
		names = append(names, strconv.Itoa(n))
	}
	list := strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	if len(pages) > 10 {
		list = strings.Join(names, ", ") + fmt.Sprintf(" and %d more", len(pages)-10)
	}
	return "Pages " + list + " have no text layer; they may be scans."
}
