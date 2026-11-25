package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
)

func isIdentityKey(key string) bool {
	v, err := scalar(key)
	return err == nil && v == "fmn-id"
}

func NoteID(content []byte) (string, error) {
	lines, _, _, err := frontMatter(content)
	if err != nil {
		return "", err
	}
	id := ""
	seen := false
	for i, line := range lines {
		key, value, ok := strings.Cut(strings.TrimRight(line, "\r\n"), ":")
		if !ok || !isIdentityKey(key) {
			continue
		}
		if seen || key != "fmn-id" {
			return "", invalidInput("use one unquoted top-level fmn-id field")
		}
		seen = true
		id, err = scalar(value)
		if err != nil || !ValidNoteID(id) {
			return "", invalidInput("fmn-id must be 32 lowercase hexadecimal characters")
		}
		for _, next := range lines[i+1:] {
			trimmed := strings.TrimSpace(next)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.HasPrefix(next, " ") || strings.HasPrefix(next, "\t") {
				return "", invalidInput("fmn-id must be a scalar")
			}
			break
		}
	}
	return id, nil
}

// ImportEntryID is the identity a file imported from Git history keeps in
// the notebook store (notestore.Options.ImportEntryID): the fmn-id a note
// declares, as the notebook's identity index reads it, or "" for none. Every
// import of Git history uses it, so one file never gets two identities.
func ImportEntryID(path string, content []byte) string {
	if !strings.HasSuffix(path, ".md") || path == "AGENTS.md" {
		return ""
	}
	if id, err := NoteID(content); err == nil && ValidNoteID(id) {
		return id
	}
	return ""
}

var notebookID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ValidNotebookID reports whether id is a notebook ID: lowercase letters,
// digits and hyphens, starting with a letter or digit, at most 64 bytes.
func ValidNotebookID(id string) bool { return notebookID.MatchString(id) }

func ValidNoteID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func NewNoteID() (string, error) {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:]), nil
}
func WithNoteID(content []byte, id string) ([]byte, error) {
	if !ValidNoteID(id) {
		return nil, invalidInput("invalid fmn-id")
	}
	lines, body, newline, err := frontMatter(content)
	if err != nil {
		return nil, err
	}
	var out strings.Builder
	out.WriteString("---" + newline)
	out.WriteString("fmn-id: " + id + newline)
	for _, line := range lines {
		key, _, ok := strings.Cut(strings.TrimRight(line, "\r\n"), ":")
		if ok && isIdentityKey(key) {
			continue
		}
		out.WriteString(line)
	}
	out.WriteString("---" + newline)
	out.Write(body)
	return []byte(out.String()), nil
}
