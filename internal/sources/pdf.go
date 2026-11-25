package sources

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	X1   float64 `xml:"xMin,attr"`
	Y1   float64 `xml:"yMin,attr"`
	X2   float64 `xml:"xMax,attr"`
	Y2   float64 `xml:"yMax,attr"`
	Text string  `xml:",chardata"`
}
type pdfGeometry struct {
	Width  float64 `xml:"width,attr"`
	Height float64 `xml:"height,attr"`
	Flows  []struct {
		Blocks []struct {
			Lines []struct {
				Words []pdfWord `xml:"word"`
			} `xml:"line"`
		} `xml:"block"`
	} `xml:"flow"`
}

func (p pdfGeometry) words() []pdfWord {
	var words []pdfWord
	for _, flow := range p.Flows {
		for _, block := range flow.Blocks {
			for _, line := range block.Lines {
				words = append(words, line.Words...)
			}
		}
	}
	return words
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

var errExtractorOutput = errors.New("extractor output exceeds its budget")

// toolFailure is a local tool that ran and exited with status 1, as FFprobe and
// the iWork adapter do on a file they cannot read. The sandbox helper exits 2,
// 126 or 127 without running the tool, and limits kill it with a signal.
type toolFailure struct{ error }

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
		failure := fmt.Errorf("%s failed or exceeded its output budget", name)
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, toolFailure{failure}
		}
		return nil, failure
	}
	return out.Bytes(), nil
}

func extractPDF(ctx context.Context, data []byte, options Options) (text, extractor, state, warning string) {
	select {
	case documentWork <- struct{}{}:
		defer func() { <-documentWork }()
	case <-ctx.Done():
		return "", pdfCanceledTools, "failed", "PDF extraction canceled."
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
	images := map[int]bool{}
	raw, imageErr := sourceCommand(ctx, dir, 1<<20, "pdfimages", "-list", filename)
	if imageErr == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			n, e := strconv.Atoi(fields[0])
			if e == nil {
				images[n] = true
			}
		}
	}
	var out boundedText
	partial, readable := false, false
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
			native := geometry.text()
			page.Text = workspace.EscapeSourceText(native)
			if (images[n] || imageErr != nil) && !(options.ExternalOCR && options.selectedPage(n, len(doc.Pages))) {
				scan, e := ocrPDFPage(ctx, dir, filename, n, geometry)
				if e != nil {
					page.Partial = true
					page.Warning = e.Error() + "; OCR for this page is incomplete."
				} else if strings.TrimSpace(scan) != "" {
					if page.Text != "" {
						page.Text += "\n\n"
					}
					page.Text += workspace.EscapeSourceText(strings.TrimSpace(scan))
				}
			}
			if imageErr != nil {
				page.Partial = true
				page.Warning = "Image inventory unavailable; rendered-page OCR was attempted. " + page.Warning
			}
			if options.pageDone != nil {
				if err := options.pageDone(page); err != nil {
					return "", extractor, "failed", err.Error()
				}
			}
		}
		partial = partial || page.Partial
		readable = readable || strings.TrimSpace(page.Text) != ""
		if page.Text == "" {
			page.Text = "_No readable text on this physical page._"
		}
		if page.Warning != "" {
			page.Text += "\n\n" + workspace.EscapeSourceText(page.Warning)
		}
		if _, err := fmt.Fprintf(&out, "## Page %d\n\n%s\n\n", n, page.Text); err != nil {
			return "", extractor, "failed", "PDF text exceeds FMN_LIMIT_SOURCE_TEXT_BYTES. Original retained."
		}
	}
	state = "extracted"
	if partial {
		state = "partial"
		warning = "Extraction is incomplete. " + warning
	} else if !readable {
		state = "no-text"
	}
	return out.String(), extractor, state, warning
}

// The original rendering and masked copy together occupy at most 64 MiB.
type scratchWriter struct {
	io.Writer
	remaining int
}

func (w *scratchWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, errors.New("page scratch exceeds 32 MiB")
	}
	n, err := w.Writer.Write(p)
	w.remaining -= n
	return n, err
}
