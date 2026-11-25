package workspace

import (
	"encoding/hex"
	"forgetmenote/internal/capacity"
	"net/url"
	"path"
	"strconv"
	"strings"
)

func MaxSourceBytes() int { return capacity.SourceBytes.Limit() }
func MaxSourceText() int  { return capacity.SourceTextBytes.Limit() }

// SourceRecord describes a capture, not a certification of the source's claims.
// Its flat frontmatter remains readable without ForgetMeNote.
type SourceRecord struct {
	ID         string `json:"id"`
	URL        string `json:"url"`
	FinalURL   string `json:"finalURL"`
	CapturedAt string `json:"capturedAt"`
	ImportedBy string `json:"importedBy"`
	Method     string `json:"method"`
	Original   string `json:"original"`
	SHA256     string `json:"sha256"`
	MIME       string `json:"mime"`
	Extractor  string `json:"extractor"`
	Extraction string `json:"extraction"`
	Warning    string `json:"warning"`
	TextSHA256 string `json:"textSHA256"`
}
type SourceCapture struct {
	OperationID string       `json:"operationId,omitempty"`
	Record      SourceRecord `json:"record"`
	Title       string       `json:"title"`
	Original    []byte       `json:"original"`
	Text        string       `json:"text"`
}

func sourceFields(r *SourceRecord) map[string]*string {
	return map[string]*string{"id": &r.ID, "url": &r.URL, "final_url": &r.FinalURL, "captured_at": &r.CapturedAt, "imported_by": &r.ImportedBy, "method": &r.Method, "original": &r.Original, "sha256": &r.SHA256, "mime": &r.MIME, "extractor": &r.Extractor, "extraction": &r.Extraction, "warning": &r.Warning, "text_sha256": &r.TextSHA256}
}
func ParseSource(content []byte) *SourceRecord {
	lines, _, _, err := frontMatter(content)
	if err != nil {
		return nil
	}
	r := new(SourceRecord)
	fields := sourceFields(r)
	seen := map[string]bool{}
	for _, line := range lines {
		key, value, ok := strings.Cut(strings.TrimRight(line, "\r\n"), ":")
		if !ok || !strings.HasPrefix(key, "source_") {
			continue
		}
		key = strings.TrimPrefix(key, "source_")
		target, ok := fields[key]
		if !ok {
			continue
		}
		if seen[key] {
			return nil
		}
		seen[key] = true
		v, e := scalar(value)
		if e != nil {
			return nil
		}
		*target = v
	}
	if len(r.ID) != 32 || len(r.SHA256) != 64 || !SourceOriginalPath(r.Original) {
		return nil
	}
	if _, err = hex.DecodeString(r.ID); err != nil {
		return nil
	}
	if _, err = hex.DecodeString(r.SHA256); err != nil {
		return nil
	}
	return r
}
func SourceURL(value string) bool {
	if value == "" {
		return true
	}
	u, err := url.Parse(value)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && u.User == nil && len(value) <= 4096 && !strings.ContainsAny(value, "\r\n")
}
func quoteSource(value string) string { return strconv.Quote(value) }

// Escaping prevents extracted text from becoming executable markup or invented
// citations. Headings/page anchors are added by the extractor, not remote HTML.
func EscapeSourceText(value string) string {
	var b strings.Builder
	for _, r := range value {
		if strings.ContainsRune("\\`*_[]<>#|~", r) {
			b.WriteByte('\\')
		}
		if r != 0 {
			b.WriteRune(r)
		}
	}
	return b.String()
}
func SourceOriginalPath(value string) bool {
	if !strings.HasPrefix(value, "assets/") || path.Clean(value) != value || strings.ContainsAny(value, "\\\x00\r\n?#%") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || strings.HasPrefix(part, ".") {
			return false
		}
	}
	return SourceFormatForName(value).MIME != ""
}

type SourceVerification struct {
	OriginalStatus string `json:"originalStatus"`
	TextStatus     string `json:"textStatus"`
	OriginalState  string `json:"originalState"`
	TextState      string `json:"textState"`
	Revision       string `json:"revision"`
}

func rewriteSourceOriginal(content []byte, from, to string) []byte {
	record := ParseSource(content)
	if record == nil {
		return content
	}
	moved := movedPath(record.Original, from, to)
	if moved == record.Original {
		return content
	}
	lines, body, newline, err := frontMatter(content)
	if err != nil {
		return content
	}
	for i, line := range lines {
		if strings.HasPrefix(line, "source_original:") {
			lines[i] = "source_original: " + strconv.Quote(moved) + newline
		}
	}
	return []byte("---" + newline + strings.Join(lines, "") + "---" + newline + string(body))
}
