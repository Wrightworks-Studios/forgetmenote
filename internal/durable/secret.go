package durable

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ReadSecret failures, so each caller can keep its own operator message.
var (
	ErrOpen          = errors.New("secret file unavailable")
	ErrNotRegular    = errors.New("secret file is not a regular file")
	ErrTooLarge      = errors.New("secret file exceeds its byte limit")
	ErrRead          = errors.New("secret file could not be read")
	ErrInvalidSecret = errors.New("secret is empty or contains CR, LF or NUL")
)

// ReadSecret reads a one-line credential of at most max bytes from a regular
// file and trims surrounding space. Symlinks are followed, as mounted secrets
// use them; checking the type before opening keeps a FIFO from blocking.
func ReadSecret(path string, max int) (string, error) {
	if info, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("%w: %w", ErrOpen, err)
	} else if !info.Mode().IsRegular() {
		return "", ErrNotRegular
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrOpen, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", ErrNotRegular
	}
	if info.Size() > int64(max) {
		return "", ErrTooLarge
	}
	raw, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrRead, err)
	}
	if len(raw) > max {
		return "", ErrTooLarge
	}
	value := strings.TrimSpace(string(raw))
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", ErrInvalidSecret
	}
	return value, nil
}
