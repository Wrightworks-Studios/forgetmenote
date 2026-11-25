package knowledge

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The names of the temporary files the knowledge caches are written through
// begin with these: persist's JSON files and writeVectorCache's semantic.bin.
// A write removes its own, unless its process stops first.
const (
	knowledgeTemporary = ".knowledge-"
	vectorsTemporary   = ".vectors-"
)

// RemoveTemporaries removes the temporary files that writes a crash stopped
// left among the knowledge caches of the notebook whose state directory is
// stateDir (DATA/projects/ID): there and in its cache directory. They may
// hold the notebook's content. A file changed within olderThan stays, as a
// write in progress may own it.
func RemoveTemporaries(stateDir string, olderThan time.Duration) error {
	var errs []error
	for _, dir := range []string{stateDir, filepath.Join(stateDir, "cache")} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.Type().IsRegular() || !strings.HasPrefix(name, knowledgeTemporary) && !strings.HasPrefix(name, vectorsTemporary) {
				continue
			}
			info, err := entry.Info()
			if err == nil && time.Since(info.ModTime()) < olderThan {
				continue
			}
			if err == nil {
				err = os.Remove(filepath.Join(dir, name))
			}
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
