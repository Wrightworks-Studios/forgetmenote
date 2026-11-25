package sources

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNativePDFCancellationDrainsDescendantProcesses(t *testing.T) {
	dir, scratch := t.TempDir(), t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Standalone extraction scratch comes from TMPDIR; the sandboxed stub records
	// its descendant there, in the only directory it may write.
	t.Setenv("TMPDIR", scratch)
	pidFiles := filepath.Join(scratch, "forgetmenote-pdf-*", "child.pid")
	helper := "#!/bin/sh\nif [ \"$1\" = -v ]; then echo 'Poppler fixture'; exit 0; fi\n/bin/sleep 30 &\necho $! > child.pid\nwait\n"
	if err := os.WriteFile(filepath.Join(dir, "pdftotext"), []byte(helper), 0700); err != nil {
		t.Fatal(err)
	}
	// Cleanup the test's own child even when exercising the old broken boundary.
	child := 0
	defer func() {
		if child > 0 {
			_ = syscall.Kill(child, syscall.SIGKILL)
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Prepare(ctx, "canceled.pdf", "", []byte("%PDF-1.4\nfixture"))
		done <- err
	}()
	started := time.Now()
	for {
		if found := scratchFiles(t, pidFiles); len(found) > 0 {
			raw, _ := os.ReadFile(found[0])
			if child, _ = strconv.Atoi(strings.TrimSpace(string(raw))); child > 0 {
				break
			}
		}
		if time.Since(started) > 3*time.Second {
			t.Fatal("native extraction fixture did not start its descendant")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("caller cancellation was not propagated", err)
		}
	case <-time.After(time.Second):
		t.Error("canceled PDF extraction waited for a descendant holding its output pipe")
	}
}

func TestPDFNativeFailurePreservesOriginal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pdftotext"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	raw := []byte("%PDF-invalid")
	got, err := Prepare(t.Context(), "bad.pdf", "", raw)
	if err != nil || got.Record.Extraction != "failed" || string(got.Original) != string(raw) || !strings.Contains(got.Record.Warning, "Original retained") {
		t.Fatalf("failed extraction lost original: %+v %v", got, err)
	}
}

func TestPDFCheckpointReuseAvoidsRepeatedOCR(t *testing.T) {
	dir := t.TempDir()
	bbox := `<html><body><doc><page width="612" height="792"><flow><block><line><word xMin="10" yMin="10" xMax="30" yMax="20">Native</word></line></block></flow></page></doc></body></html>`
	for name, body := range map[string]string{"pdftotext": "cat <<'XML'\n" + bbox + "\nXML\n", "pdfimages": "printf '1 0 image\\n'\n", "pdftoppm": "exit 9\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := PrepareWithOptions(t.Context(), "resume.pdf", "", []byte("%PDF-fixture"), Options{completedPages: []PDFPage{{Number: 1, Text: "Completed amber scan"}}})
	if err != nil || got.Record.Extraction != "extracted" || !strings.Contains(got.Text, "Completed amber scan") || strings.Contains(got.Text, "Native") {
		t.Fatalf("checkpoint reran extraction: %+v %v", got, err)
	}
}
