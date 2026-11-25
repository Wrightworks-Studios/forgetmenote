// Package gitobj encodes and parses Git's blob, tree and commit objects in the
// SHA-1 object format, byte for byte as Git writes them, and writes loose
// objects. It never runs Git: notebook revisions are hashed in this process
// when they are written, and the tests check every encoding against Git.
package gitobj

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"strconv"
)

// An ID names a Git object: the SHA-1 of its kind, size and body.
type ID [20]byte

// EmptyTree is the ID of the tree with no entries.
var EmptyTree = HashObject("tree", nil)

// ParseID reads 40 lowercase hexadecimal characters.
func ParseID(s string) (ID, error) {
	var id ID
	if len(s) != 40 {
		return id, errors.New("object ID must be 40 hexadecimal characters")
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return id, errors.New("object ID must be 40 lowercase hexadecimal characters")
		}
	}
	hex.Decode(id[:], []byte(s))
	return id, nil
}

// String is the 40 lowercase hexadecimal characters Git prints.
func (id ID) String() string { return hex.EncodeToString(id[:]) }

// IsZero reports whether id is the zero ID, which names no object.
func (id ID) IsZero() bool { return id == ID{} }

func (id ID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

func (id *ID) UnmarshalText(raw []byte) error {
	parsed, err := ParseID(string(raw))
	if err == nil {
		*id = parsed
	}
	return err
}

// HashObject returns the ID Git gives an object of kind (blob, tree, commit or
// tag) with body.
func HashObject(kind string, body []byte) ID {
	h := sha1.New()
	h.Write(header(kind, len(body)))
	h.Write(body)
	var id ID
	h.Sum(id[:0])
	return id
}

// BlobID is HashObject("blob", content).
func BlobID(content []byte) ID { return HashObject("blob", content) }

func header(kind string, size int) []byte {
	b := make([]byte, 0, len(kind)+22)
	b = append(b, kind...)
	b = append(b, ' ')
	b = strconv.AppendInt(b, int64(size), 10)
	return append(b, 0)
}
