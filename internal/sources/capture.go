// Package sources owns bounded capture and extraction of untrusted documents.
package sources

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"forgetmenote/internal/workspace"
	"golang.org/x/net/html"
	"unicode/utf8"
)

// PublicAddress is applied to every DNS answer and every redirect's connection.
// The transport dials the validated IP itself, preventing DNS rebinding.
func PublicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "2001::/23", "2002::/16", "64:ff9b::/96"} {
		if netip.MustParsePrefix(cidr).Contains(ip) {
			return false
		}
	}
	return true
}
func ValidateURL(value string) error {
	if !workspace.SourceURL(value) || value == "" {
		return rejection("provide a public HTTP or HTTPS URL without credentials")
	}
	u, _ := url.Parse(value)
	if u.Port() != "" && u.Port() != "80" && u.Port() != "443" {
		return rejection("source URLs must use port 80 or 443")
	}
	if ip, e := netip.ParseAddr(u.Hostname()); e == nil && !PublicAddress(ip) {
		return rejection("source capture cannot access private or special-use addresses")
	}
	return nil
}

// HTTPStatusError retains the final public URL after the shared transport has
// validated DNS, TLS and redirects. Browser capture may use it when a site
// denies this static client; security and transport failures remain distinct.
type HTTPStatusError struct {
	StatusCode int
	URL        string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("source returned HTTP %d; nothing was archived", e.StatusCode)
}

// ErrRejected matches a refusal of the caller's document, URL or selection,
// which only a different one can overcome. Every other failure remains the
// server's, and so does a fetch that failed rather than being refused.
var ErrRejected = errors.New("source rejected")

type rejection string

func (r rejection) Error() string      { return string(r) }
func (rejection) Is(target error) bool { return target == ErrRejected }

// refused marks a failure of work that reads nothing but the document.
func refused(err error) error { return rejection(err.Error()) }

// documentOnly says a format's extraction reads nothing but the document's
// bytes, so any failure that is not cancellation is the document's. The other
// formats also run tools and write scratch files, which fail on their own
// account, and mark only their work on the document.
func documentOnly(extension string) bool {
	switch extension {
	case ".html", ".txt", ".csv", ".tsv", ".docx", ".xlsx", ".epub", ".pptx", ".odt", ".ods", ".odp", ".eml", ".msg":
		return true
	}
	return false
}

func Fetch(ctx context.Context, rawURL string) ([]byte, string, string, error) {
	if err := ValidateURL(rawURL); err != nil {
		return nil, "", "", err
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 64 << 10, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, errors.New("source hostname could not be resolved")
		}
		if len(ips) == 0 {
			return nil, errors.New("source hostname has no address")
		}
		for _, ip := range ips {
			if !PublicAddress(ip) {
				return nil, rejection("source capture cannot access private or special-use addresses")
			}
		}
		dialer := net.Dialer{Timeout: 8 * time.Second}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 25 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return rejection("too many source redirects")
		}
		return ValidateURL(req.URL.String())
	}}
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("User-Agent", "ForgetMeNote/1.0 SourceCapture")
	var accepted []string
	for _, format := range workspace.SourceFormats() {
		accepted = append(accepted, format.MIME)
	}
	req.Header.Set("Accept", strings.Join(accepted, ","))
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", "", fmt.Errorf("source capture failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", "", &HTTPStatusError{StatusCode: resp.StatusCode, URL: resp.Request.URL.String()}
	}
	if resp.ContentLength > int64(workspace.MaxSourceBytes()) {
		return nil, "", "", rejection("source exceeds FMN_LIMIT_SOURCE_BYTES")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(workspace.MaxSourceBytes())+1))
	if err != nil {
		return nil, "", "", errors.New("source download was interrupted")
	}
	if len(data) > workspace.MaxSourceBytes() {
		return nil, "", "", rejection("source exceeds FMN_LIMIT_SOURCE_BYTES")
	}
	return data, resp.Header.Get("Content-Type"), resp.Request.URL.String(), nil
}

type Options struct {
	DescribeImages bool `json:"describeImages,omitempty"`
	ExternalOCR    bool `json:"externalOCR,omitempty"`
	Transcribe     bool `json:"transcribe,omitempty"`
	FirstPage      int  `json:"firstPage,omitempty"`
	LastPage       int  `json:"lastPage,omitempty"`
	hosted         *Hosted
	completedPages []PDFPage
	pageDone       func(PDFPage) error
	pageStart      func(int) error
}

func Prepare(ctx context.Context, filename, originalURL string, data []byte) (workspace.SourceCapture, error) {
	return PrepareWithOptions(ctx, filename, originalURL, data, Options{})
}

func PrepareWithOptions(ctx context.Context, filename, originalURL string, data []byte, options Options) (workspace.SourceCapture, error) {
	var out workspace.SourceCapture
	if options.external() && options.hosted == nil {
		return out, errors.New("hosted source processing requires explicit instance policy and a configured processor")
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if !workspace.SourceURL(originalURL) {
		return out, errors.New("invalid original URL")
	}
	contentType := ""
	method := "uploaded"
	finalURL := ""
	if len(data) == 0 {
		var err error
		data, contentType, finalURL, err = Fetch(ctx, originalURL)
		if err != nil {
			return out, err
		}
		method = "server-http"
	}
	if len(data) == 0 || len(data) > workspace.MaxSourceBytes() {
		return out, errors.New("source must be nonempty and fit FMN_LIMIT_SOURCE_BYTES")
	}
	b := make([]byte, 16)
	rand.Read(b)
	if filename == "" && finalURL != "" {
		if u, err := url.Parse(finalURL); err == nil {
			filename = u.Path
		}
	}
	out.Original = data
	out.Title = filepath.Base(filename)
	out.Record = workspace.SourceRecord{ID: hex.EncodeToString(b), URL: originalURL, FinalURL: finalURL, Method: method, CapturedAt: time.Now().UTC().Format(time.RFC3339Nano), SHA256: workspace.ContentHash(data)}
	if bytes.HasPrefix(data, []byte("%PDF-")) {
		out.Record.MIME = "application/pdf"
		out.Text, out.Record.Extractor, out.Record.Extraction, out.Record.Warning = extractPDF(ctx, data, options)
	} else {
		format := workspace.SourceFormatForName(filename)
		// A URL may be a viewer, so its actual response type takes precedence.
		if fetched := workspace.SourceFormatForMIME(strings.TrimSpace(strings.Split(contentType, ";")[0])); fetched.MIME != "" {
			format = fetched
		}
		if format.MIME == "" || format.MIME == "application/pdf" {
			return out, rejection("unsupported or invalid source format; use a supported original file rather than a viewer URL")
		}
		if options.DescribeImages && !format.DescribeImages {
			return out, rejection("image descriptions are available for PDFs, images, and PowerPoint files")
		}
		out.Record.MIME = format.MIME
		out.Record.Extractor = "ForgetMeNote " + format.Name + " text v1"
		out.Record.Extraction = "extracted"
		var err error
		switch format.Extensions[0] {
		case ".html":
			var title string
			title, out.Text, err = htmlTextContext(ctx, data, contentType)
			if title != "" {
				out.Title = title
			}
			out.Record.Warning = "Single-page HTML capture; scripts, linked assets, and interactive content are not captured. Completeness has not been verified."
		case ".txt":
			out.Text, err = plainText(data)
			out.Text = workspace.EscapeSourceText(out.Text)
		case ".csv", ".tsv":
			delimiter := ','
			if format.Extensions[0] == ".tsv" {
				delimiter = '\t'
			}
			out.Text, err = delimitedText(data, delimiter)
			out.Record.Warning = "Records are numbered; the first record is preserved as data. Formula-like values remain literal text."
		case ".docx", ".xlsx", ".epub":
			var document *documentPackage
			document, err = openPackage(data)
			if document != nil {
				document.ctx = ctx
			}
			if err != nil {
				break
			}
			switch format.Extensions[0] {
			case ".docx":
				out.Text, err = docxText(document)
				out.Record.Warning = "Main document text, headings, and simple tables only. Headers, footnotes, drawings, nested tables, and exact page layout are omitted. Tracked deletions are excluded."
			case ".xlsx":
				out.Text, err = xlsxText(document)
				out.Record.Warning = "Stored cells include hidden sheets, rows, and columns. Formulas are never run; saved results may be stale. Dates and number formats may appear as raw values. Charts and workbook layout are not extracted."
			case ".epub":
				var title string
				title, out.Text, err = epubText(document)
				if title != "" {
					out.Title = title
				}
				out.Record.Warning = "Chapter text follows the book's reading order. Media, interactive content, and exact pagination are omitted. Encrypted chapters are not supported."
			}
		default:
			if mediaExtension(format.Extensions[0]) {
				if !options.Transcribe {
					out.Record.Warning, err = mediaOriginal(ctx, format.Extensions[0], data)
					out.Record.Extractor = "FFmpeg media validation v1"
				} else {
					out.Record.Extractor = "Hosted transcription pending"
				}
			} else {
				out.Text, out.Record.Extractor, out.Record.Extraction, err = extractDocument(ctx, format.Extensions[0], data, options)
			}
			if !mediaExtension(format.Extensions[0]) {
				out.Record.Warning = format.Description
			}
			if options.DescribeImages {
				out.Record.Warning += " AI image descriptions may contain errors."
			}
			if out.Record.Extraction == "partial" {
				out.Record.Warning = "Extraction is incomplete. " + out.Record.Warning
			}
		}
		if err != nil {
			// Parsers report a read stopped by cancellation as a bad document.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return out, ctxErr
			}
			if documentOnly(format.Extensions[0]) {
				err = refused(err)
			}
			return out, err
		}
		if len(out.Text) > workspace.MaxSourceText() {
			return out, rejection("extracted source text exceeds FMN_LIMIT_SOURCE_TEXT_BYTES")
		}
		if out.Record.Extraction == "no-text" || strings.TrimSpace(out.Text) == "" {
			out.Record.Extraction = "no-text"
			out.Record.Warning = "No readable text found. " + out.Record.Warning
		}
	}
	if out.Title == "" || out.Title == "." {
		out.Title = "Archived source"
		if u, e := url.Parse(originalURL); e == nil && u.Hostname() != "" {
			out.Title = u.Hostname()
		}
	}
	if len(out.Title) > 300 {
		out.Title = string([]rune(out.Title)[:min(len([]rune(out.Title)), 75)])
	}
	if err := ctx.Err(); err != nil {
		return workspace.SourceCapture{}, err
	}
	if options.external() {
		if err := options.hosted.Process(ctx, &out, options); err != nil {
			return workspace.SourceCapture{}, err
		}
	}
	out.Record.TextSHA256 = workspace.ContentHash([]byte(out.Text))
	return out, nil
}
func htmlTextContext(ctx context.Context, data []byte, contentType string) (string, string, error) {
	if !utf8.Valid(data) {
		return "", "", errors.New("HTML text must be UTF-8; original encoding is not supported")
	}
	// Reject excessive node work before allocating a DOM, including empty markup.
	tokenizer := html.NewTokenizer(contextReader{ctx, bytes.NewReader(data)})
	tokenizer.SetMaxBuf(workspace.MaxSourceText())
	for tokens := 0; ; tokens++ {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		if tokens > 100000 {
			return "", "", errors.New("HTML exceeds 100000 tokens")
		}
		if tokenizer.Next() == html.ErrorToken {
			err := tokenizer.Err()
			if errors.Is(err, html.ErrBufferExceeded) {
				return "", "", errors.New("HTML token exceeds FMN_LIMIT_SOURCE_TEXT_BYTES")
			}
			if err != io.EOF {
				return "", "", err
			}
			break
		}
	}
	tree, err := html.Parse(contextReader{ctx, bytes.NewReader(data)})
	if err != nil {
		return "", "", errors.New("HTML could not be parsed")
	}
	var text, title strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if ctx.Err() != nil || text.Len() > workspace.MaxSourceText() {
			return
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "svg", "template", "nav", "footer", "iframe", "form":
				return
			}
			for _, a := range n.Attr {
				if a.Key == "hidden" || a.Key == "aria-hidden" && a.Val == "true" {
					return
				}
			}
			if n.Data == "title" {
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					if c.Type == html.TextNode {
						title.WriteString(c.Data)
					}
				}
				return
			}
			if n.Data == "head" {
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					if c.Data == "title" {
						walk(c)
					}
				}
				return
			}
			if strings.Contains("|p|div|article|section|main|li|tr|blockquote|", "|"+n.Data+"|") {
				text.WriteString("\n\n")
			}
			if len(n.Data) == 2 && n.Data[0] == 'h' && n.Data[1] >= '1' && n.Data[1] <= '6' {
				text.WriteString("\n\n" + strings.Repeat("#", min(6, int(n.Data[1]-'0')+1)) + " ")
			}
			if n.Data == "br" {
				text.WriteString("\n\n")
			}
		}
		if n.Type == html.TextNode {
			v := strings.Join(strings.Fields(n.Data), " ")
			if v != "" {
				text.WriteString(workspace.EscapeSourceText(v) + " ")
			}
		}
		if text.Len() > workspace.MaxSourceText() {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && len(n.Data) == 2 && n.Data[0] == 'h' {
			text.WriteString("\n\n")
		}
	}
	walk(tree)
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if text.Len() > workspace.MaxSourceText() {
		return "", "", errors.New("extracted source text exceeds FMN_LIMIT_SOURCE_TEXT_BYTES")
	}
	return strings.Join(strings.Fields(title.String()), " "), strings.TrimSpace(text.String()) + "\n", nil
}

type boundedText struct{ bytes.Buffer }

func (b *boundedText) Write(p []byte) (int, error) {
	if b.Len()+len(p) > workspace.MaxSourceText() {
		return 0, rejection("extraction exceeds FMN_LIMIT_SOURCE_TEXT_BYTES")
	}
	return b.Buffer.Write(p)
}

// Parsers check cancellation while consuming their bounded input.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if r.ctx != nil {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
	}
	return r.reader.Read(p)
}
