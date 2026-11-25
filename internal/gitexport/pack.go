package gitexport

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"forgetmenote/internal/durable"
	"forgetmenote/internal/gitobj"
	"forgetmenote/internal/notestore"
)

// packTypes are the pack entry types of the object kinds a notebook holds.
var packTypes = map[string]byte{"commit": 1, "tree": 2, "blob": 3}

// writePack writes the objects of nb's revisions after seq after into the
// repository at path as one pack and returns the last commit, calling commit
// with each in order. git index-pack checks each object, indexes the pack
// and makes it durable with one fsync. The objects are whole, so the caller
// computes their deltas once the branch reaches them (repackDeltas). As
// loose objects, each waiting for its own two fsyncs, writing F2's 20,000
// revisions took 50 s to over 2 minutes on a busy disk, 67 s of an 87 s
// purge rebuild.
func writePack(ctx context.Context, nb *notestore.Notebook, path string, after int64, commit func(id gitobj.ID, body []byte)) (gitobj.ID, error) {
	dir := filepath.Join(path, "objects", "pack")
	if err := durable.MkdirAll(dir, 0o755, nil); err != nil {
		return gitobj.ID{}, err
	}
	// The pack is staged under a name Git ignores, in case this process
	// stops before it is gone; the next maintenance or rebuild deletes it
	// (removeTemporaries).
	f, err := os.CreateTemp(dir, "tmp_rebuild_")
	if err != nil {
		return gitobj.ID{}, err
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()
	count, last, err := packObjects(ctx, nb, f, after, commit)
	if err != nil {
		return gitobj.ID{}, err
	}
	if err := finishPack(f, count); err != nil {
		return gitobj.ID{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, repackTimeout)
	defer cancel()
	cmd := gitCommand(ctx, path, "index-pack", "--stdin")
	errOut := &tail{limit: 4 << 10}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = f, io.Discard, errOut
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w: %w", err, ctx.Err())
		}
		return gitobj.ID{}, &gitError{args: []string{"index-pack", "--stdin"}, err: err, stderr: strings.TrimSpace(errOut.String())}
	}
	return last, nil
}

// packObjects writes the pack header and an entry for every distinct object
// of nb's revisions after seq after to w, compressing on looseWriters
// goroutines, and returns how many entries it wrote and the last commit.
// Entries are whole objects, so their order is free.
func packObjects(ctx context.Context, nb *notestore.Notebook, w io.Writer, after int64, commit func(id gitobj.ID, body []byte)) (uint32, gitobj.ID, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	out := bufio.NewWriterSize(w, 1<<20)
	if _, err := out.WriteString("PACK\x00\x00\x00\x02\x00\x00\x00\x00"); err != nil {
		return 0, gitobj.ID{}, err
	}
	type object struct {
		kind string
		body []byte
	}
	work := make(chan object, 2*looseWriters)
	entries := make(chan []byte, 2*looseWriters)
	var compressors sync.WaitGroup
	for range looseWriters {
		compressors.Go(func() {
			var buf bytes.Buffer
			z := zlib.NewWriter(&buf)
			for o := range work {
				buf.Reset()
				buf.Write(packEntryHeader(packTypes[o.kind], len(o.body)))
				z.Reset(&buf)
				z.Write(o.body)
				if err := z.Close(); err != nil {
					cancel(err)
					continue
				}
				entries <- bytes.Clone(buf.Bytes())
			}
		})
	}
	written := make(chan error, 1)
	go func() {
		var err error
		for entry := range entries {
			if err == nil {
				if _, err = out.Write(entry); err != nil {
					cancel(err)
				}
			}
		}
		written <- err
	}()
	seen := map[gitobj.ID]bool{}
	var last gitobj.ID
	err := nb.Objects(ctx, after, func(kind string, id gitobj.ID, body []byte) error {
		if kind == "commit" {
			last = id
			if commit != nil {
				commit(id, body)
			}
		}
		if packTypes[kind] == 0 {
			return fmt.Errorf("object %s of unknown kind %q", id, kind)
		}
		if seen[id] {
			return nil // a revision brought back an object an earlier one had
		}
		seen[id] = true
		select {
		case work <- object{kind, body}:
			return nil
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	})
	close(work)
	compressors.Wait()
	close(entries)
	if writeErr := <-written; err == nil {
		err = writeErr
	}
	if cause := context.Cause(ctx); err == nil && cause != nil {
		err = cause
	}
	if err == nil && len(seen) > math.MaxUint32 {
		err = fmt.Errorf("%d objects do not fit in one pack", len(seen))
	}
	if err == nil {
		err = out.Flush()
	}
	return uint32(len(seen)), last, err
}

// packEntryHeader is the type and size header of a pack entry: the type in
// bits 4 to 6 of the first byte, the size in its low 4 bits and 7 bits of
// each following byte, the high bit set on every byte but the last.
func packEntryHeader(kind byte, size int) []byte {
	h := []byte{kind<<4 | byte(size&0x0f)}
	for size >>= 4; size > 0; size >>= 7 {
		h[len(h)-1] |= 0x80
		h = append(h, byte(size&0x7f))
	}
	return h
}

// finishPack writes the object count into the header of the pack in f and
// appends its trailer, the SHA-1 of everything before it, then rewinds f.
func finishPack(f *os.File, count uint32) error {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], count)
	if _, err := f.WriteAt(n[:], 8); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if _, err := f.Write(h.Sum(nil)); err != nil {
		return err
	}
	_, err := f.Seek(0, io.SeekStart)
	return err
}
