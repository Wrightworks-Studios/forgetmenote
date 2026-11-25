package benchfixture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// F1 is Moby-Dick (Project Gutenberg eBook 2701, public domain) as one
// manuscript note. The source is never committed: it is read from the fixture
// cache, or downloaded into it when FMN_FIXTURE_FETCH=1, and pinned by hash.
const (
	NovelSourceURL    = "https://gutenberg.pglaf.org/2/7/0/2701/2701-0.txt"
	NovelSourceSHA256 = "f12eb530502755ef2b8f535e2ecef3e95802eab584e60ac7992ccf856256fdbe"
	NovelSHA256       = "15bf4dd69bccc3785d705aee3f26e80726a0a4f41baf0f26a9c7053d4d1077dd"
	NovelID           = "6d6f62792d6469636b2d6e6f76656c31"
	// Cache layout, relative to CacheDir.
	NovelSourceFile = "raw/pg2701.txt"
	NovelFile       = "f1-moby-dick.md"
)

// ErrUnavailable reports that a fixture source is neither cached nor allowed
// to be downloaded. Tests skip on it.
var ErrUnavailable = errors.New("benchfixture: source not cached; set FMN_FIXTURE_FETCH=1 to download it into FMN_FIXTURE_CACHE")

// CacheDir is FMN_FIXTURE_CACHE, or forgetmenote-fixtures in the user cache
// directory.
func CacheDir() (string, error) {
	if dir := os.Getenv("FMN_FIXTURE_CACHE"); dir != "" {
		return dir, nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "forgetmenote-fixtures"), nil
}

func sha256Hex(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

// Novel returns the F1 Markdown note. It converts the cached source, falls back
// to a cached converted note, and otherwise downloads the source when
// FMN_FIXTURE_FETCH=1. Every path verifies the pinned hashes.
func Novel(ctx context.Context) ([]byte, error) {
	dir, err := CacheDir()
	if err != nil {
		return nil, err
	}
	raw, err := readPinned(filepath.Join(dir, NovelSourceFile), NovelSourceSHA256)
	if errors.Is(err, os.ErrNotExist) {
		if md, mdErr := readPinned(filepath.Join(dir, NovelFile), NovelSHA256); mdErr == nil || !errors.Is(mdErr, os.ErrNotExist) {
			return md, mdErr
		}
		if os.Getenv("FMN_FIXTURE_FETCH") != "1" {
			return nil, ErrUnavailable
		}
		raw, err = FetchNovelSource(ctx, http.DefaultClient, NovelSourceURL, dir)
	}
	if err != nil {
		return nil, err
	}
	md, err := ConvertGutenberg(raw)
	if err != nil {
		return nil, err
	}
	if got := sha256Hex(md); got != NovelSHA256 {
		return nil, fmt.Errorf("benchfixture: converted novel has SHA-256 %s, want %s", got, NovelSHA256)
	}
	return md, nil
}

// NovelOrSkip returns F1 or skips the test when its source is unavailable.
func NovelOrSkip(tb testing.TB) []byte {
	tb.Helper()
	md, err := Novel(tb.Context())
	if errors.Is(err, ErrUnavailable) {
		tb.Skip(err)
	}
	if err != nil {
		tb.Fatal(err)
	}
	return md
}

func readPinned(path, want string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if got := sha256Hex(raw); got != want {
		return nil, fmt.Errorf("benchfixture: %s has SHA-256 %s, want %s", path, got, want)
	}
	return raw, nil
}

// FetchNovelSource downloads the F1 source from url, verifies its pinned hash
// and stores it in the cache directory dir. Nothing is cached on a mismatch.
func FetchNovelSource(ctx context.Context, client *http.Client, url, dir string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("benchfixture: GET %s: %s", url, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if got := sha256Hex(raw); got != NovelSourceSHA256 {
		return nil, fmt.Errorf("benchfixture: %s has SHA-256 %s, want %s", url, got, NovelSourceSHA256)
	}
	target := filepath.Join(dir, NovelSourceFile)
	if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".pg2701-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(raw); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), target)
	}
	return raw, err
}

const (
	gutenbergStart = "*** START OF THE PROJECT GUTENBERG EBOOK 2701 ***"
	gutenbergEnd   = "*** END OF THE PROJECT GUTENBERG EBOOK 2701 ***"
	// Python's \s for str patterns: the characters str.isspace() accepts.
	pyWhitespace = `\t\n\x0b\x0c\r\x1c-\x1f \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`
)

var (
	firstChapter   = regexp.MustCompile(`(?m)^CHAPTER 1\. Loomings\.`)
	paragraphBreak = regexp.MustCompile(`\n[` + pyWhitespace + `]*\n`)
	headingLine    = regexp.MustCompile(`^(CHAPTER \p{Nd}+\..*|Epilogue)$`)
	pySpace        = regexp.MustCompile(`^[` + pyWhitespace + `]$`)
)

func pyIsSpace(r rune) bool   { return pySpace.MatchString(string(r)) }
func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// pyTitle is Python's str.title for headings without non-ASCII letters:
// uppercase a letter that follows an uncased character, lowercase the rest.
func pyTitle(s string) (string, error) {
	var b strings.Builder
	previousCased := false
	for _, r := range s {
		ascii := r < utf8.RuneSelf
		cased := ascii && ('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z')
		if !ascii && (unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r) || unicode.Is(unicode.Other_Lowercase, r) || unicode.Is(unicode.Other_Uppercase, r)) {
			// Python applies full Unicode case mappings here (ß → Ss, final
			// sigma); the pinned source has no such heading, so refuse it.
			return "", fmt.Errorf("benchfixture: heading %q has a non-ASCII letter", s)
		}
		switch {
		case previousCased && 'A' <= r && r <= 'Z':
			r += 'a' - 'A'
		case !previousCased && 'a' <= r && r <= 'z':
			r -= 'a' - 'A'
		}
		b.WriteRune(r)
		previousCased = cased
	}
	return b.String(), nil
}

// ConvertGutenberg is the coordinator's scripts/mobydick_md.py in Go: strip the
// Gutenberg header, footer and contents, then emit one Markdown note with a
// front matter block, the title and one "##" heading per chapter. It keeps the
// script's Python semantics (UTF-8 with BOM, universal newlines, Unicode \s,
// str.strip and str.title), including its "’S" capitalization.
func ConvertGutenberg(raw []byte) ([]byte, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("benchfixture: source is not UTF-8")
	}
	text := strings.TrimPrefix(string(raw), "\ufeff")
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	_, body, ok := strings.Cut(text, gutenbergStart)
	if !ok {
		return nil, errors.New("benchfixture: source has no Gutenberg start marker")
	}
	body, _, _ = strings.Cut(body, gutenbergEnd)
	// The first match is the table of contents.
	if first := firstChapter.FindAllStringIndex(body, 2); len(first) > 1 {
		body = body[first[1][0]:]
	}
	out := []string{"---", "fmn-id: " + NovelID, "title: Moby-Dick; or, The Whale", "kind: manuscript", "---", "", "# Moby-Dick; or, The Whale", ""}
	for _, para := range paragraphBreak.Split(body, -1) {
		var lines []string
		for _, line := range strings.Split(pyStrip(para), "\n") {
			if line = pyStrip(line); line != "" {
				lines = append(lines, line)
			}
		}
		if len(lines) == 0 {
			continue
		}
		joined := strings.Join(lines, " ")
		if headingLine.MatchString(lines[0]) && len(lines) <= 2 {
			title, err := pyTitle(joined)
			if err != nil {
				return nil, err
			}
			out = append(out, "## "+strings.ReplaceAll(title, "'S", "'s"), "")
		} else {
			out = append(out, joined, "")
		}
	}
	return []byte(strings.Join(out, "\n")), nil
}
