// Package filestate moves a stopped instance's control-plane state between the
// file-based release's data directory and Postgres. Each owner package converts
// its own files; forgetmenote import-files and export-files run them in order.
//
// Both directions report, for each area, how many files it read or wrote and a
// digest of their canonical content. An export of a fresh import reports the
// digests the import did, so an operator compares the two reports instead of
// trusting either side.
package filestate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"

	"forgetmenote/internal/blob"
)

// MaxFile bounds any one file an import reads; larger state did not fit the
// file-based release's own limits.
const MaxFile = 1 << 30

// Sum reports an area's files: how many, and a digest of their canonical content.
type Sum struct {
	Files  int    `json:"files"`
	Digest string `json:"digest"`
}

// Tally collects an area's files by the slash path the file-based release reads
// them at, relative to its data directory.
type Tally struct{ files map[string][sha256.Size]byte }

// Add records one file by its canonical content: the form in which equal state
// has equal bytes, such as JSON with sorted keys.
func (t *Tally) Add(rel string, canonical []byte) {
	if t.files == nil {
		t.files = map[string][sha256.Size]byte{}
	}
	t.files[rel] = sha256.Sum256(canonical)
}

func (t *Tally) addSum(rel string, h hash.Hash) {
	if t.files == nil {
		t.files = map[string][sha256.Size]byte{}
	}
	t.files[rel] = [sha256.Size]byte(h.Sum(nil))
}

// Sum returns the count and digest of the files added.
func (t *Tally) Sum() Sum {
	names := make([]string, 0, len(t.files))
	for rel := range t.files {
		names = append(names, rel)
	}
	slices.Sort(names)
	h := sha256.New()
	for _, rel := range names {
		sum := t.files[rel]
		fmt.Fprintf(h, "%s\x00%s\n", rel, hex.EncodeToString(sum[:]))
	}
	return Sum{Files: len(names), Digest: hex.EncodeToString(h.Sum(nil))}
}

// JSON returns raw's canonical form: decoded and encoded again, so object keys
// are sorted and formatting does not change a digest. Numbers keep their text.
func JSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after JSON value")
	}
	return json.Marshal(value)
}

// Source is the stopped instance's data directory. Reads stay inside it and
// never follow a symbolic link out of it.
type Source struct {
	root *os.Root
	lock *os.File
	// Blobs receives the files an area keeps in blob storage, under the paths
	// the file-based release kept them at. It is nil while blob storage is this
	// data directory, which already holds them there.
	Blobs blob.Store
}

// OpenSource holds the directory's catalog.lock, which every file-based process
// holds while it serves, so a running instance is refused rather than read. The
// directory may be read-only.
func OpenSource(dir string) (*Source, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	lock, err := root.OpenFile("catalog.lock", os.O_CREATE|os.O_RDWR, 0600)
	if errors.Is(err, syscall.EROFS) || errors.Is(err, fs.ErrPermission) {
		// A stopped volume mounted read-only, so its release can restart on it
		// untouched, keeps the lock file every file-based process created.
		lock, err = root.OpenFile("catalog.lock", os.O_RDONLY, 0)
	}
	if err == nil {
		if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			lock.Close()
			err = errors.New("another ForgetMeNote process owns this data directory; stop it first")
		}
	}
	if err != nil {
		root.Close()
		return nil, err
	}
	return &Source{root: root, lock: lock}, nil
}

// Dir is the directory's path, for an owner package that keeps a file there.
func (s *Source) Dir() string { return s.root.Name() }

func (s *Source) Close() error {
	s.lock.Close()
	return s.root.Close()
}

// Read returns a regular file's content, or fs.ErrNotExist.
func (s *Source) Read(rel string) ([]byte, error) {
	if err := valid(rel); err != nil {
		return nil, err
	}
	info, err := s.root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	if info.Size() > MaxFile {
		return nil, fmt.Errorf("%s exceeds %d MiB", rel, MaxFile>>20)
	}
	return s.root.ReadFile(rel)
}

// ImportBlob stores the regular file at rel in s.Blobs under the same path,
// when s.Blobs is set, and adds it to tally by its bytes. A failed import
// leaves what it stored, which the next import replaces.
func (s *Source) ImportBlob(ctx context.Context, tally *Tally, rel string) error {
	if err := valid(rel); err != nil {
		return err
	}
	info, err := s.root.Lstat(rel)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", rel)
	}
	if info.Size() > MaxFile {
		return fmt.Errorf("%s exceeds %d MiB", rel, MaxFile>>20)
	}
	f, err := s.root.Open(rel)
	if err != nil {
		return err
	}
	defer f.Close()
	if s.Blobs != nil {
		if err = s.Blobs.Put(ctx, rel, f); err != nil {
			return err
		}
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return err
	}
	tally.addSum(rel, h)
	return nil
}

// Entries lists a directory's names in byte order, or nothing when it is absent.
func (s *Source) Entries(rel string) ([]fs.DirEntry, error) {
	if err := valid(rel); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(s.root.FS(), rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return entries, err
}

// Target is a new directory an export writes the file-based release's files
// into. It never replaces a file.
type Target struct {
	root *os.Root
	// Blobs is the instance's blob storage, which ExportBlob copies from.
	Blobs blob.Store
}

// CreateTarget makes dir, which must not exist yet.
func CreateTarget(dir string) (*Target, error) {
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Target{root: root}, nil
}

func (t *Target) Close() error { return t.root.Close() }

// Write creates a private file and its directories, failing if it exists.
func (t *Target) Write(rel string, raw []byte) error {
	if err := valid(rel); err != nil {
		return err
	}
	if dir := path.Dir(rel); dir != "." {
		if err := t.root.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	f, err := t.root.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// ExportBlob writes the object under key in t.Blobs to the same path in the
// target and adds it to tally by its bytes.
func (t *Target) ExportBlob(ctx context.Context, tally *Tally, key string) error {
	return t.ExportBlobAs(ctx, tally, key, key)
}

// ExportBlobAs writes the object under key in t.Blobs to rel in the target and
// adds it to tally by its bytes, as rel.
func (t *Target) ExportBlobAs(ctx context.Context, tally *Tally, key, rel string) error {
	if err := valid(key); err != nil {
		return err
	}
	if err := valid(rel); err != nil {
		return err
	}
	r, err := t.Blobs.Get(ctx, key)
	if err != nil {
		return err
	}
	defer r.Close()
	if dir := path.Dir(rel); dir != "." {
		if err := t.root.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	f, err := t.root.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), r)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		tally.addSum(rel, h)
	}
	return err
}

func valid(rel string) error {
	if !fs.ValidPath(rel) || strings.Contains(rel, "\\") {
		return fmt.Errorf("invalid state path %q", rel)
	}
	return nil
}
