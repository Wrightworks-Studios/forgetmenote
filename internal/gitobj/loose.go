package gitobj

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"forgetmenote/internal/durable"
)

// WriteLoose stores an object of kind with body as a zlib-compressed loose
// object under gitDir/objects, as Git would, and returns its ID. An object
// already present is left alone. The file is synced before it is renamed into
// place, and every directory it lands in is synced, so a crash leaves either no
// object or a complete one.
func WriteLoose(gitDir, kind string, body []byte) (ID, error) {
	id := HashObject(kind, body)
	name := id.String()
	dir := filepath.Join(gitDir, "objects", name[:2])
	path := filepath.Join(dir, name[2:])
	if _, err := os.Lstat(path); err == nil {
		return id, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return id, err
	}
	var compressed bytes.Buffer
	z := zlib.NewWriter(&compressed)
	z.Write(header(kind, len(body)))
	z.Write(body)
	if err := z.Close(); err != nil {
		return id, err
	}
	if err := durable.MkdirAll(dir, 0o755, nil); err != nil {
		return id, err
	}
	suffix := make([]byte, 6)
	rand.Read(suffix)
	temporary := filepath.Join(dir, "tmp_obj_"+hex.EncodeToString(suffix))
	f, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		return id, err
	}
	_, err = f.Write(compressed.Bytes())
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		// Another writer of the same object writes the same bytes.
		err = os.Rename(temporary, path)
	}
	if err != nil {
		os.Remove(temporary)
		return id, err
	}
	return id, durable.SyncDir(dir)
}
