package client

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
)

func (c *Client) Export(ctx context.Context, project, destination string) error {
	f, err := os.CreateTemp(filepath.Dir(destination), ".fmn-export-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	hash := sha256.New()
	writer := io.MultiWriter(f, hash)
	var offset, total int64
	digest := ""
	for {
		var chunk struct {
			Data, Encoding, SHA256    string
			Offset, NextOffset, Total int64
		}
		if err = c.Call(ctx, "export_project_backup", map[string]any{"project": project, "offset": offset, "sha256": digest, "limit": 262144}, &chunk); err != nil {
			return err
		}
		if offset == 0 {
			digest, total = chunk.SHA256, chunk.Total
		}
		data, e := base64.StdEncoding.DecodeString(chunk.Data)
		if e != nil || chunk.Encoding != "base64" || len(digest) != 64 || chunk.SHA256 != digest || chunk.Offset != offset || chunk.Total != total || chunk.NextOffset != offset+int64(len(data)) || chunk.NextOffset > total || (len(data) == 0 && offset < total) {
			return errors.New("invalid or changed export chunk")
		}
		if _, err = writer.Write(data); err != nil {
			return err
		}
		offset = chunk.NextOffset
		if offset == total {
			break
		}
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("export checksum mismatch")
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	// Link only into a nonexistent destination; existing backups are untouched.
	return os.Link(f.Name(), destination)
}
