package sources

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubPDFText installs a pdftotext whose shell body computes one page's words.
func stubPDFText(t *testing.T, words string) {
	t.Helper()
	dir := t.TempDir()
	body := "#!/bin/sh\nprintf '<html><body><doc><page width=\"612\" height=\"792\"><flow><block><line><word xMin=\"1\" yMin=\"1\" xMax=\"2\" yMax=\"2\">%s</word></line></block></flow></page></doc></body></html>\\n' \"" + words + "\"\n"
	for name, script := range map[string]string{"pdftotext": body, "pdfimages": "#!/bin/sh\nexit 0\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// scratchFiles finds marker files that stub tools leave in their working
// directory: a sandboxed tool may write only beneath its scratch directory.
func scratchFiles(t *testing.T, pattern string) []string {
	t.Helper()
	found, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestLocalExtractorsRunWithResourceLimits(t *testing.T) {
	stubPDFText(t, "cpu-$(ulimit -t)-core-$(ulimit -c)")
	got, err := Prepare(t.Context(), "limits.pdf", "", []byte("%PDF-1.4\nfixture"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Text, "cpu-50-core-0") {
		t.Fatalf("extractor ran without its CPU and core-dump limits: %q", got.Text)
	}
}
