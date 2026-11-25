package web

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/knowledge"
)

// A gateway's requests write temporary files into its own data directory: a
// bundle export's bundle until it is stored (cache/exports), a browser
// backup's bundle while it is prepared and sent (cache/backups), and a
// notebook import's upload while it is checked and stored
// (.notebook-import-*, catalog.Import's). Each request removes its own. What
// a process that stopped mid-request left, the next one removes when it
// starts; the bundles, which hold a notebook as it was, a purge removes too.
// An import's multipart upload is spooled to the system's temporary
// directory instead, a tmpfs in the shipped containers, which no restart
// keeps.
var temporaryBundles = []string{filepath.Join("cache", "exports"), filepath.Join("cache", "backups")}

// Writes that replace a file go through a temporary one beside it: content
// blocks and export bundles in a blob directory (blob.Dir.Put), and each
// notebook's knowledge caches (knowledge.RemoveTemporaries). One a crash
// left may hold notebook content, purged content too, so a starting gateway
// and a purge remove those unchanged for leftoverAge; a write in progress
// changes its own far more often.
var (
	leftoverAge  = time.Minute
	blobPrefixes = []string{"blocks", "exports"}
)

// removeLeftovers removes the temporary files of data, before any request of
// this process runs: no request of another process writes there, as each
// gateway has a data directory of its own. Files in the data directory
// itself stay, whatever their names: an operator may keep a bundle there, as
// the backup and notebook export commands write one, and the browser backups
// earlier releases wrote there as backup-<digits>.bundle cannot be told from
// such a bundle.
func removeLeftovers(data string, blobs blob.Store) error {
	var errs []error
	for _, dir := range temporaryBundles {
		errs = append(errs, os.RemoveAll(filepath.Join(data, dir)))
	}
	entries, err := os.ReadDir(data)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".notebook-import-") && entry.IsDir() {
			errs = append(errs, os.RemoveAll(filepath.Join(data, name)))
		}
	}
	errs = append(errs, removeInterruptedWrites(data, blobs))
	return errors.Join(errs...)
}

// removeInterruptedWrites removes the temporary files that writes a crash
// stopped left in the blob directory, if blobs is one, and among the
// knowledge caches of every notebook under data.
func removeInterruptedWrites(data string, blobs blob.Store) error {
	var errs []error
	if dir, ok := blobs.(interface {
		RemoveTemporaries(string, time.Duration) (int, error)
	}); ok {
		for _, prefix := range blobPrefixes {
			_, err := dir.RemoveTemporaries(prefix, leftoverAge)
			errs = append(errs, err)
		}
	}
	projects, err := os.ReadDir(filepath.Join(data, "projects"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, entry := range projects {
		if entry.IsDir() {
			errs = append(errs, knowledge.RemoveTemporaries(filepath.Join(data, "projects", entry.Name()), leftoverAge))
		}
	}
	return errors.Join(errs...)
}

// removeTemporaryBundles removes every bundle this gateway is preparing or
// sending, or failed to remove, for a purge: each holds a notebook as it
// was, and a bundle's name does not say which notebook. A request still
// preparing one then fails and is asked to start again; one already sending
// its bundle completes from the file it opened. The temporary files writes a
// crash stopped left go too (removeInterruptedWrites).
func (s *Server) removeTemporaryBundles() error {
	errs := []error{removeInterruptedWrites(s.data, s.blobs)}
	for _, dir := range temporaryBundles {
		dir = filepath.Join(s.data, dir)
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
		for _, entry := range entries {
			errs = append(errs, os.RemoveAll(filepath.Join(dir, entry.Name())))
		}
	}
	return errors.Join(errs...)
}
