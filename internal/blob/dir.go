package blob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"forgetmenote/internal/durable"
)

// Dir keeps each object as a private file at its key under a directory, so
// the data directory's own files are its objects. Writes go to a dot-named
// temporary beside the object, which no key can name, and are synced before
// they replace it; each directory a write creates is synced into its parent.
type Dir struct{ root *os.Root }

// OpenDir returns the store over the existing directory dir.
func OpenDir(dir string) (*Dir, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("blob directory: %w", err)
	}
	return &Dir{root: root}, nil
}

// Close releases the directory.
func (d *Dir) Close() error { return d.root.Close() }

// Path is the directory whose files are the store's objects, as OpenDir
// received it.
func (d *Dir) Path() string { return d.root.Name() }

// temporaryPrefix begins the name of the temporary file Put writes beside an
// object before renaming it into place; no key names one.
const temporaryPrefix = ".blob-"

// RemoveTemporaries removes the temporary files that Puts a crash stopped
// left in dir, the directory of the keys dir/NAME, once unchanged for
// olderThan, so a Put still writing one keeps it, and reports how many it
// removed. It does not look into dir's subdirectories.
func (d *Dir) RemoveTemporaries(dir string, olderThan time.Duration) (int, error) {
	if !ValidKey(dir) {
		return 0, errors.New("invalid blob prefix")
	}
	entries, err := fs.ReadDir(d.root.FS(), dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	var errs []error
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), temporaryPrefix) || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err == nil && time.Since(info.ModTime()) < olderThan {
			continue
		}
		if err == nil {
			err = d.root.Remove(path.Join(dir, entry.Name()))
		}
		if err == nil {
			removed++
		} else if !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return removed, errors.Join(errs...)
}

func (d *Dir) Put(ctx context.Context, key string, body io.ReadSeeker) error {
	if !ValidKey(key) {
		return errors.New("invalid blob key")
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return err
	}
	dir := path.Dir(key)
	if err := durable.MkdirAllIn(d.root, dir, 0o700); err != nil {
		return err
	}
	suffix := make([]byte, 8)
	rand.Read(suffix)
	temporary := path.Join(dir, temporaryPrefix+hex.EncodeToString(suffix))
	f, err := d.root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, readerContext{ctx, body})
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = d.root.Rename(temporary, key)
	}
	if err != nil {
		d.root.Remove(temporary)
		return err
	}
	return d.syncDir(dir)
}

func (d *Dir) syncDir(dir string) error {
	f, err := d.root.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (d *Dir) Get(_ context.Context, key string) (io.ReadCloser, error) {
	if !ValidKey(key) {
		return nil, errors.New("invalid blob key")
	}
	info, err := d.root.Lstat(key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, notFound(key)
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", key)
	}
	return d.root.Open(key)
}

func (d *Dir) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 || length < 1 {
		return nil, errors.New("invalid blob range")
	}
	r, err := d.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	f := r.(*os.File)
	info, err := f.Stat()
	if err == nil && offset+length > info.Size() {
		err = fmt.Errorf("%s: range beyond the object", key)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.NewSectionReader(f, offset, length), f}, nil
}

// Delete removes key's file and then any directories it leaves empty, short
// of the key's first component.
func (d *Dir) Delete(_ context.Context, key string) error {
	if !ValidKey(key) {
		return errors.New("invalid blob key")
	}
	if err := d.root.Remove(key); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for dir := path.Dir(key); strings.Contains(dir, "/"); dir = path.Dir(dir) {
		if d.root.Remove(dir) != nil {
			break
		}
	}
	return nil
}

func (d *Dir) List(_ context.Context, prefix string) ([]string, error) {
	if !validPrefix(prefix) {
		return nil, errors.New("invalid blob prefix")
	}
	var keys []string
	err := fs.WalkDir(d.root.FS(), strings.TrimSuffix(prefix, "/"), func(name string, entry fs.DirEntry, err error) error {
		// A missing prefix holds no objects. Delete removes the directories it
		// empties, so a directory gone since its parent was read held none either.
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() && ValidKey(name) {
			keys = append(keys, name)
		}
		return nil
	})
	sort.Strings(keys)
	return keys, err
}

// readerContext stops a copy once its context ends.
type readerContext struct {
	ctx context.Context
	r   io.Reader
}

func (r readerContext) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
