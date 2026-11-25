package gitobj

import (
	"bytes"
	"errors"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Tree entry modes Git writes. Other modes found in old repositories are kept
// as parsed.
const (
	ModeFile       uint32 = 0o100644
	ModeExecutable uint32 = 0o100755
	ModeTree       uint32 = 0o040000
	ModeSymlink    uint32 = 0o120000
	ModeGitlink    uint32 = 0o160000
)

// A TreeEntry names one object in a tree.
type TreeEntry struct {
	Name string
	Mode uint32
	ID   ID
}

// IsTree reports whether the entry is a subdirectory.
func (e TreeEntry) IsTree() bool { return e.Mode == ModeTree }

// ValidName reports whether name can be a tree entry: not empty, ".", "..",
// or any spelling of ".git" git fsck refuses, and without a slash or NUL.
func ValidName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00") && !isDotGit(name)
}

// isDotGit reports whether git fsck refuses a tree entry named name as a
// spelling of ".git" (hasDotgit). It checks the name as NTFS and HFS+ read
// it, whatever the platform, so a repository holding one cannot be checked,
// backed up or restored anywhere.
func isDotGit(name string) bool {
	if isHFSDotGit(name) {
		return true
	}
	// NTFS also reads a backslash as a separator.
	for {
		if isNTFSDotGit(name) {
			return true
		}
		i := strings.IndexByte(name, '\\')
		if i < 0 {
			return false
		}
		name = name[i+1:]
	}
}

// isNTFSDotGit is Git's is_ntfs_dotgit: ".git" or its short name "git~1",
// in any case, followed only by spaces and dots, which NTFS drops, up to the
// end, a separator, or a colon, which starts a stream name.
func isNTFSDotGit(name string) bool {
	var rest string
	switch {
	case len(name) >= 4 && name[0] == '.' && foldsTo(name[1:4], "git"):
		rest = name[4:]
	case len(name) >= 5 && foldsTo(name[:3], "git") && name[3:5] == "~1":
		rest = name[5:]
	default:
		return false
	}
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '/', '\\', ':':
			return true
		case ' ', '.':
		default:
			return false
		}
	}
	return true
}

// isHFSDotGit is Git's is_hfs_dotgit: ".git" in any case once the code
// points HFS+ ignores are left out. Git stops at malformed UTF-8 as at the
// end of the name.
func isHFSDotGit(name string) bool {
	next := func() rune {
		for name != "" {
			r, size := utf8.DecodeRuneInString(name)
			// Git's decoder also refuses the noncharacters U+FFFE and U+FFFF.
			if r == utf8.RuneError && size == 1 || r == 0xfffe || r == 0xffff {
				name = ""
				return 0
			}
			name = name[size:]
			switch {
			case r >= 0x200c && r <= 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x206a && r <= 0x206f, r == 0xfeff:
			default:
				return r
			}
		}
		return 0
	}
	if next() != '.' {
		return false
	}
	for _, want := range []byte("git") {
		r := next()
		if r > 127 || foldASCII(byte(r)) != want {
			return false
		}
	}
	r := next()
	return r == 0 || r == '/'
}

// foldsTo reports whether s is lower, a lowercase ASCII word, in any case.
func foldsTo(s, lower string) bool {
	if len(s) != len(lower) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if foldASCII(s[i]) != lower[i] {
			return false
		}
	}
	return true
}

func foldASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// CompareEntries orders entries as Git sorts a tree: by name, with a
// subdirectory compared as if its name ended in a slash.
func CompareEntries(a, b TreeEntry) int {
	return strings.Compare(sortName(a), sortName(b))
}

func sortName(e TreeEntry) string {
	if e.IsTree() {
		return e.Name + "/"
	}
	return e.Name
}

// EncodeTree returns the body of the tree holding entries, sorted as Git sorts
// them. Names must be valid and distinct.
func EncodeTree(entries []TreeEntry) []byte {
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, CompareEntries)
	size := 0
	for _, e := range sorted {
		size += len(e.Name) + 28
	}
	b := make([]byte, 0, size)
	for _, e := range sorted {
		b = strconv.AppendUint(b, uint64(e.Mode), 8)
		b = append(b, ' ')
		b = append(b, e.Name...)
		b = append(b, 0)
		b = append(b, e.ID[:]...)
	}
	return b
}

// ParseTree reads a tree body. Modes are kept as written, so an entry with an
// unusual mode survives a round trip; a zero-padded mode does not, because Git
// itself never writes one.
func ParseTree(body []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	for len(body) > 0 {
		space := bytes.IndexByte(body, ' ')
		if space < 1 {
			return nil, errors.New("tree entry has no mode")
		}
		mode, err := strconv.ParseUint(string(body[:space]), 8, 32)
		if err != nil {
			return nil, errors.New("tree entry mode is not octal")
		}
		body = body[space+1:]
		nul := bytes.IndexByte(body, 0)
		if nul < 1 || len(body) < nul+21 {
			return nil, errors.New("tree entry is truncated")
		}
		e := TreeEntry{Name: string(body[:nul]), Mode: uint32(mode)}
		copy(e.ID[:], body[nul+1:nul+21])
		entries = append(entries, e)
		body = body[nul+21:]
	}
	return entries, nil
}
