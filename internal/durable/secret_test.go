package durable

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadSecretAcceptsOneBoundedLineFromARegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	write := func(raw string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("  secret-value\n")
	if value, err := ReadSecret(path, 15); err != nil || value != "secret-value" {
		t.Fatalf("value = %q, %v", value, err)
	}
	if _, err := ReadSecret(path, 14); !errors.Is(err, ErrTooLarge) {
		t.Fatal("oversized secret:", err)
	}
	link := filepath.Join(dir, "mounted")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if value, err := ReadSecret(link, 64); err != nil || value != "secret-value" {
		t.Fatal("mounted secrets are symlinks:", value, err)
	}
	for _, raw := range []string{"", " \n\t", "two\nlines", "carriage\rreturn", "nul\x00byte"} {
		write(raw)
		if _, err := ReadSecret(path, 64); !errors.Is(err, ErrInvalidSecret) {
			t.Fatalf("%q accepted: %v", raw, err)
		}
	}
	if _, err := ReadSecret(filepath.Join(dir, "missing"), 64); !errors.Is(err, ErrOpen) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("missing secret:", err)
	}
	if _, err := ReadSecret(dir, 64); !errors.Is(err, ErrNotRegular) {
		t.Fatal("directory secret:", err)
	}
}

func TestReadSecretRejectsAFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Skip("mkfifo unavailable:", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ReadSecret(path, 64)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Fatal("FIFO secret:", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO secret blocked")
	}
}
