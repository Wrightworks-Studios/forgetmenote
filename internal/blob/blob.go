// Package blob keeps the files an instance stores outside Postgres: source job
// originals, pages and results, repository source generations and retained
// evidence, and export bundles. One host keeps them in a directory, by default
// the data directory itself in the file-based release's layout; gateways that
// share an instance keep them in S3.
package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
)

// A Store holds objects by key. A key is a slash-separated path whose
// components are letters, digits, '.', '_' and '-' and never start with a dot.
type Store interface {
	// Put stores body, read from its start, under key and replaces any object
	// there. A reader sees the old object or the new one, never part of one.
	Put(ctx context.Context, key string, body io.ReadSeeker) error
	// Get opens the object under key, or returns an fs.ErrNotExist error.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// GetRange opens length bytes of the object under key from offset, which
	// must lie within it.
	GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)
	// Delete removes the object under key; a missing object is not an error.
	Delete(ctx context.Context, key string) error
	// List returns every key under prefix, which ends in a slash, in order.
	List(ctx context.Context, prefix string) ([]string, error)
}

// ErrTooLarge is a stored object larger than its reader accepts.
var ErrTooLarge = errors.New("stored object exceeds its limit")

var component = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*$`)

// ValidKey reports whether key may name an object.
func ValidKey(key string) bool {
	if key == "" || len(key) > 512 {
		return false
	}
	for _, part := range strings.Split(key, "/") {
		if !component.MatchString(part) {
			return false
		}
	}
	return true
}

func validPrefix(prefix string) bool {
	return strings.HasSuffix(prefix, "/") && ValidKey(strings.TrimSuffix(prefix, "/"))
}

// Read returns the object under key, refusing one larger than limit bytes.
func Read(ctx context.Context, s Store, key string, limit int64) ([]byte, error) {
	r, err := s.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%s: %w", key, ErrTooLarge)
	}
	return raw, nil
}

// Write stores raw under key.
func Write(ctx context.Context, s Store, key string, raw []byte) error {
	return s.Put(ctx, key, bytes.NewReader(raw))
}

// RemoveAll deletes every object under prefix.
func RemoveAll(ctx context.Context, s Store, prefix string) error {
	keys, err := s.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err = s.Delete(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

// Config names an instance's blob store: S3 when it names a bucket, otherwise
// Directory, which defaults to the data directory.
type Config struct {
	Directory string    `json:"directory,omitempty"`
	S3        *S3Config `json:"s3,omitempty"`
}

// Validate checks c without opening anything.
func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	if c.S3 != nil {
		if c.Directory != "" {
			return errors.New("blobs name either a directory or S3, not both")
		}
		return c.S3.validate()
	}
	if c.Directory != "" && !filepath.IsAbs(c.Directory) {
		return errors.New("blobs directory requires an absolute path")
	}
	return nil
}

// Open returns the store c names, with data as the default directory.
func Open(ctx context.Context, c *Config, data string) (Store, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if c != nil && c.S3 != nil {
		return OpenS3(ctx, *c.S3)
	}
	dir := data
	if c != nil && c.Directory != "" {
		dir = c.Directory
	}
	return OpenDir(dir)
}

func notFound(key string) error {
	return fmt.Errorf("%s: %w", key, fs.ErrNotExist)
}
