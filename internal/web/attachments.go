package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
)

func attachmentType(path string) string {
	if workspace.SourceOriginalPath(path) {
		return workspace.SourceFormatForName(path).MIME
	}
	if !strings.HasPrefix(path, "assets/") {
		return ""
	}
	return mediaTypes[strings.ToLower(filepath.Ext(path))]
}
func saveAttachment(ctx context.Context, store *workspace.Store, path string, content []byte, revision, actor string) (workspace.Document, error) {
	if attachmentType(path) == "" {
		return workspace.Document{}, errors.New("attachment path must be under assets/ with a supported media extension")
	}
	return store.Save(ctx, path, content, revision, actor)
}

type binaryChunk struct {
	Data       string `json:"data"`
	Encoding   string `json:"encoding"`
	MIMEType   string `json:"mimeType"`
	Revision   string `json:"revision,omitempty"`
	SHA256     string `json:"sha256"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"nextOffset"`
	Total      int64  `json:"total"`
}

func chunk(reader io.ReaderAt, total, offset int64, limit int) (binaryChunk, error) {
	if offset < 0 || offset > total || limit < 0 || limit > 262144 {
		return binaryChunk{}, errors.New("invalid offset or limit; maximum chunk size is 262144 bytes")
	}
	if limit == 0 {
		limit = 65536
	}
	if int64(limit) > total-offset {
		limit = int(total - offset)
	}
	raw := make([]byte, limit)
	if _, err := reader.ReadAt(raw, offset); err != nil && !(len(raw) == 0 && errors.Is(err, io.EOF)) {
		return binaryChunk{}, err
	}
	return binaryChunk{Data: base64.StdEncoding.EncodeToString(raw), Encoding: "base64", Offset: offset, NextOffset: offset + int64(len(raw)), Total: total}, nil
}
func readAttachment(ctx context.Context, store *workspace.Store, path, revision string, offset int64, limit int) (binaryChunk, error) {
	typ := attachmentType(path)
	if typ == "" || offset > 0 && revision == "" {
		return binaryChunk{}, errors.New("provide a supported assets/ path and pin revision after the first chunk")
	}
	doc, err := store.Read(ctx, path, revision)
	if err != nil {
		return binaryChunk{}, err
	}
	out, err := chunk(bytes.NewReader(doc.Content), int64(len(doc.Content)), offset, limit)
	out.Revision = doc.Revision
	out.MIMEType = typ
	out.SHA256 = workspace.ContentHash(doc.Content)
	return out, err
}

// Browser downloads stream a temporary bundle through the export path's bounded
// Git backup. At most liveExports run at once on a gateway, each under the MCP
// export's size cap.
var (
	errBackupBusy     = webcore.Error{Status: 429, Code: "capacity", Message: fmt.Sprintf("%d notebook backups are already being prepared; retry shortly", liveExports)}
	errBackupTooLarge = webcore.Error{Status: 413, Code: "too_large", Message: "Notebook backup exceeds 1 GiB; use the operator backup"}
	errBackupPurged   = webcore.Error{Status: 409, Code: "conflict", Message: "A purge ran while this backup was prepared; download it again."}
)

func (s *Server) admitBackup() (func(), error) {
	select {
	case s.backupDownloads <- struct{}{}:
		return func() { <-s.backupDownloads }, nil
	default:
		return nil, errBackupBusy
	}
}

// createBackup writes store's bundle to a temporary file and returns it
// open; the caller closes and removes it. A backup that a purge overtook
// while it was prepared holds the notebook as it was, so it is refused, as a
// bundle export ends itself (beginExport): the purge removed its file
// (removeTemporaryBundles), or rewrote a commit its bundle names.
func (s *Server) createBackup(ctx context.Context, store *workspace.Store) (*os.File, error) {
	dir := filepath.Join(s.data, "cache", "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, "backup-*.bundle")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	if err = f.Close(); err != nil {
		os.Remove(name)
		return nil, err
	}
	if err = store.BackupLimited(ctx, name, s.backupBytes); err != nil {
		// A local bundle stops at the cap; its owner reports the cause otherwise.
		if info, statErr := os.Stat(name); statErr == nil && info.Size() >= s.backupBytes {
			err = errBackupTooLarge
		}
		os.Remove(name)
		return nil, err
	}
	bundle, err := os.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errBackupPurged
	}
	if err != nil {
		os.Remove(name)
		return nil, err
	}
	heads, err := bundleHeads(bundle)
	purged := false
	if err == nil {
		purged, err = purgedBundle(ctx, store, heads)
	}
	if err == nil && purged {
		err = errBackupPurged
	}
	if err == nil {
		_, err = bundle.Seek(0, io.SeekStart)
	}
	if err != nil {
		bundle.Close()
		os.Remove(name)
		return nil, err
	}
	return bundle, nil
}
