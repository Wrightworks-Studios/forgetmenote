// Package durable owns crash-safe local file replacement. Callers keep their
// own paths, permissions checks and error mapping.
package durable

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteFile replaces path through a synced temporary file in the same
// directory, then syncs the directory so the rename survives a crash. The
// temporary file is created with mode 0600 and the caller's prefix. A nil
// syncDir uses SyncDir.
func WriteFile(path, tempPrefix string, raw []byte, syncDir func(string) error) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, tempPrefix)
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return err
	}
	if syncDir == nil {
		syncDir = SyncDir
	}
	return syncDir(dir)
}

// SyncDir makes completed creates, renames and removals in dir durable.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// MkdirAll creates path and its missing parents like os.MkdirAll, and syncs
// the parent of each directory it creates, so a crash cannot lose a directory
// that a later durable write inside it depends on. A nil syncDir uses SyncDir.
func MkdirAll(path string, perm os.FileMode, syncDir func(string) error) error {
	if syncDir == nil {
		syncDir = SyncDir
	}
	return mkdirAll(filepath.Clean(path), perm, os.Stat, os.Mkdir, syncDir)
}

// MkdirAllIn is MkdirAll for a name inside root, which it never leaves.
func MkdirAllIn(root *os.Root, name string, perm os.FileMode) error {
	return mkdirAll(filepath.Clean(name), perm, root.Stat, root.Mkdir, func(dir string) error {
		d, err := root.Open(dir)
		if err != nil {
			return err
		}
		defer d.Close()
		return d.Sync()
	})
}

func mkdirAll(path string, perm os.FileMode, stat func(string) (fs.FileInfo, error), mkdir func(string, os.FileMode) error, syncDir func(string) error) error {
	var missing []string
	for p := path; ; {
		info, err := stat(p)
		if err == nil {
			if !info.IsDir() {
				return &fs.PathError{Op: "mkdir", Path: p, Err: errors.New("not a directory")}
			}
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		missing = append(missing, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	for i := len(missing) - 1; i >= 0; i-- {
		// Another process may create the same directory first; syncing its
		// parent again is harmless and covers a creator that did not.
		if err := mkdir(missing[i], perm); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err := syncDir(filepath.Dir(missing[i])); err != nil {
			return err
		}
	}
	return nil
}
