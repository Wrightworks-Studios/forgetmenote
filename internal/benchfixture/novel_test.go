package benchfixture

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// The expected output is what scripts/mobydick_md.py printed for this source
// (python3 mobydick_md.py src.txt out.md), covering its BOM, universal newline,
// Unicode whitespace, title-casing and two-line heading behaviour.
const (
	pythonEdgeSource = "\xef\xbb\xbfHeader junk\r\n*** START OF THE PROJECT GUTENBERG EBOOK 2701 ***\r\n\r\nCONTENTS\r\n\r\nCHAPTER 1. Loomings.\r\nCHAPTER 2. The Carpet-Bag.\r\n\r\n\r\nCHAPTER 1. Loomings.\r\n\r\nCall me Ishmael.  Some years\r\nago.\r\n \t\x0b\r\nSecond  para\xc2\xa0\r\n\r\nCHAPTER 2. AHAB'S Leg\xe2\x80\x99s end\xe2\x80\x94again.\r\n\r\n\xc2\xa0\xe3\x80\x80\r\nLine one\rline two\r\n\r\nCHAPTER 3. Two\r\nLines heading\r\n\r\nCHAPTER 4. Too\r\nmany\r\nlines\r\n\r\nCHAPTER \xd9\xa3. Arabic-indic digit\r\n\r\nEpilogue\r\n\r\nThe end.\x1c\r\n\x1c\r\nLast\xe2\x80\xa8line\r\n*** END OF THE PROJECT GUTENBERG EBOOK 2701 ***\r\nfooter"
	pythonEdgeOutput = "---\nfmn-id: 6d6f62792d6469636b2d6e6f76656c31\ntitle: Moby-Dick; or, The Whale\nkind: manuscript\n---\n\n# Moby-Dick; or, The Whale\n\n## Chapter 1. Loomings.\n\nCall me Ishmael.  Some years ago.\n\nSecond  para\n\n## Chapter 2. Ahab's Leg\u2019S End\u2014Again.\n\nLine one line two\n\n## Chapter 3. Two Lines Heading\n\nCHAPTER 4. Too many lines\n\n## Chapter \u0663. Arabic-Indic Digit\n\n## Epilogue\n\nThe end.\n\nLast\u2028line\n"
)

func TestConvertGutenbergMatchesPythonScript(t *testing.T) {
	got, err := ConvertGutenberg([]byte(pythonEdgeSource))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != pythonEdgeOutput {
		t.Fatalf("converted\n%q\nwant\n%q", got, pythonEdgeOutput)
	}
	if _, err = ConvertGutenberg([]byte("no markers")); err == nil {
		t.Fatal("source without a start marker converted")
	}
	if _, err = ConvertGutenberg([]byte(gutenbergStart + "\n\nCHAPTER 1. STRASSE GRÜßE.\n")); err == nil {
		t.Fatal("heading needing full Unicode case mapping converted")
	}
}

func cachedSourceOrSkip(t *testing.T) (string, []byte) {
	t.Helper()
	dir, err := CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := readPinned(filepath.Join(dir, NovelSourceFile), NovelSourceSHA256)
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("F1 source not in FMN_FIXTURE_CACHE")
	}
	if err != nil {
		t.Fatal(err)
	}
	return dir, raw
}

func TestNovelMatchesPinnedHash(t *testing.T) {
	dir, raw := cachedSourceOrSkip(t)
	md, err := ConvertGutenberg(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := sha256Hex(md); got != NovelSHA256 {
		t.Fatalf("F1 SHA-256 %s, want %s", got, NovelSHA256)
	}
	if n := bytes.Count(md, []byte("\n## ")); n != 136 {
		t.Fatalf("F1 has %d chapters, want 136", n)
	}
	if cached, err := os.ReadFile(filepath.Join(dir, NovelFile)); err == nil && !bytes.Equal(cached, md) {
		t.Fatal("F1 differs from the coordinator's converted copy")
	}
	viaNovel, err := Novel(t.Context())
	if err != nil || !bytes.Equal(viaNovel, md) {
		t.Fatalf("Novel() = %d bytes, %v", len(viaNovel), err)
	}
}

func TestNovelCacheFallbacks(t *testing.T) {
	t.Setenv("FMN_FIXTURE_FETCH", "")
	empty := t.TempDir()
	t.Setenv("FMN_FIXTURE_CACHE", empty)
	if _, err := Novel(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("empty cache: %v, want ErrUnavailable", err)
	}
	// A converted note alone is enough, but only with the pinned hash.
	if err := os.WriteFile(filepath.Join(empty, NovelFile), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Novel(t.Context()); err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("tampered note: %v", err)
	}
}

func TestFetchNovelSourceVerifiesBeforeCaching(t *testing.T) {
	var body atomic.Pointer[[]byte]
	body.Store(&[]byte{'x'})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(*body.Load()) }))
	defer server.Close()
	dir := t.TempDir()
	if _, err := FetchNovelSource(t.Context(), server.Client(), server.URL, dir); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("mismatched download: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, NovelSourceFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatched download was cached: %v", err)
	}

	_, raw := cachedSourceOrSkip(t)
	body.Store(&raw)
	got, err := FetchNovelSource(t.Context(), server.Client(), server.URL, dir)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("download: %d bytes, %v", len(got), err)
	}
	t.Setenv("FMN_FIXTURE_CACHE", dir)
	md, err := Novel(t.Context())
	if err != nil || sha256Hex(md) != NovelSHA256 {
		t.Fatalf("Novel from downloaded cache: %v", err)
	}
}
