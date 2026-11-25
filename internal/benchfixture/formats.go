package benchfixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash/adler32"
	"hash/crc32"
)

// These mirror the committed notebook formats of internal/workspace
// (comments.go, trash.go, locks.go) field for field, so encoding/json writes
// the same bytes the app writes. The tests decode and re-encode every
// generated file with the workspace types to prove it.

const (
	locksPath      = ".gitnotes-locks.json"
	trashIndexPath = ".gitnotes-trash.json"
)

func commentPath(path string) string {
	sum := sha256.Sum256([]byte(path))
	return "comments/" + hex.EncodeToString(sum[:]) + ".json"
}

type commentAnchor struct {
	Quote    string `json:"quote"`
	Prefix   string `json:"prefix,omitempty"`
	Suffix   string `json:"suffix,omitempty"`
	Revision string `json:"revision,omitempty"`
}

type commentReply struct {
	ID      string `json:"id"`
	Author  string `json:"author"`
	Text    string `json:"text"`
	Created string `json:"created"`
}

type comment struct {
	Anchor   *commentAnchor `json:"anchor,omitempty"`
	Replies  []commentReply `json:"replies,omitempty"`
	ID       string         `json:"id"`
	Author   string         `json:"author"`
	Text     string         `json:"text"`
	Created  string         `json:"created"`
	Resolved bool           `json:"resolved"`
	Deleted  bool           `json:"deleted,omitempty"`
}

type trashEntry struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	OriginalPath string `json:"originalPath"`
	Title        string `json:"title"`
	DeletedAt    string `json:"deletedAt"`
	DeletedBy    string `json:"deletedBy"`
}

type trashRecord struct {
	Hidden bool `json:"hidden,omitempty"`
	trashEntry
	History  map[string][]string `json:"history"`
	Emptied  bool                `json:"emptied,omitempty"`
	Restored bool                `json:"restored,omitempty"`
}

type trashIndex struct {
	Version int           `json:"version"`
	Records []trashRecord `json:"records"`
}

type lockPolicy struct {
	Version int      `json:"version"`
	Paths   []string `json:"paths"`
}

func indented(v any, newline bool) []byte {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	if newline {
		raw = append(raw, '\n')
	}
	return raw
}

// pngImage returns a valid 8-bit RGB PNG of lo to hi bytes whose pixels are
// incompressible noise, like an uploaded photo. It writes stored deflate
// blocks itself so the bytes never depend on the Go release.
func pngImage(r *rng, lo, hi int) []byte {
	width := 1024
	if hi < 1<<20 {
		width = 32
	}
	stride := 1 + 3*width
	size := r.between(min(lo+2*stride+256, hi), hi)
	rows := max(1, (size-128)/(stride+1))
	raw := make([]byte, rows*stride)
	r.fill(raw)
	for i := 0; i < len(raw); i += stride {
		raw[i] = 0 // filter type None
	}
	var z bytes.Buffer
	z.Grow(len(raw) + len(raw)/65535*5 + 16)
	z.Write([]byte{0x78, 0x01})
	for off := 0; off < len(raw) || off == 0; {
		n := min(len(raw)-off, 65535)
		final := byte(0)
		if off+n == len(raw) {
			final = 1
		}
		z.Write([]byte{final, byte(n), byte(n >> 8), ^byte(n), ^byte(n >> 8)})
		z.Write(raw[off : off+n])
		off += n
		if final == 1 {
			break
		}
	}
	binary.Write(&z, binary.BigEndian, adler32.Checksum(raw))
	var out bytes.Buffer
	out.Grow(z.Len() + 64)
	out.WriteString("\x89PNG\r\n\x1a\n")
	chunk := func(kind string, data []byte) {
		binary.Write(&out, binary.BigEndian, uint32(len(data)))
		crc := crc32.NewIEEE()
		crc.Write([]byte(kind))
		crc.Write(data)
		out.WriteString(kind)
		out.Write(data)
		binary.Write(&out, binary.BigEndian, crc.Sum32())
	}
	header := make([]byte, 13)
	binary.BigEndian.PutUint32(header[0:], uint32(width))
	binary.BigEndian.PutUint32(header[4:], uint32(rows))
	header[8], header[9] = 8, 2 // 8-bit RGB
	chunk("IHDR", header)
	chunk("IDAT", z.Bytes())
	chunk("IEND", nil)
	return out.Bytes()
}
