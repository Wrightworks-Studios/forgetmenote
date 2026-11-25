package gitobj

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A Signature is an author or committer line: who, and when in seconds since
// the epoch with the zone offset Git records, such as "+0100".
type Signature struct {
	Name, Email string
	When        int64
	TZ          string
}

// NewSignature records name and email at t in t's zone, as Git does.
func NewSignature(name, email string, t time.Time) Signature {
	_, offset := t.Zone()
	sign := '+'
	if offset < 0 {
		sign, offset = '-', -offset
	}
	return Signature{Name: name, Email: email, When: t.Unix(), TZ: fmt.Sprintf("%c%02d%02d", sign, offset/3600, offset/60%60)}
}

// Time returns the signature's moment in its recorded zone.
func (s Signature) Time() time.Time {
	t := time.Unix(s.When, 0)
	if len(s.TZ) == 5 && (s.TZ[0] == '+' || s.TZ[0] == '-') {
		hours, errH := strconv.Atoi(s.TZ[1:3])
		minutes, errM := strconv.Atoi(s.TZ[3:5])
		if errH == nil && errM == nil {
			offset := hours*3600 + minutes*60
			if s.TZ[0] == '-' {
				offset = -offset
			}
			return t.In(time.FixedZone(s.TZ, offset))
		}
	}
	return t.UTC()
}

func (s Signature) append(b []byte) []byte {
	b = append(b, s.Name...)
	b = append(b, " <"...)
	b = append(b, s.Email...)
	b = append(b, "> "...)
	b = strconv.AppendInt(b, s.When, 10)
	b = append(b, ' ')
	return append(b, s.TZ...)
}

// CleanIdent returns a name or email as Git records it in a signature: without
// surrounding spaces and punctuation Git treats as crud, and without the line
// delimiters newline, '<' and '>'.
func CleanIdent(s string) string {
	start, end := 0, len(s)
	for start < end && crud(s[start]) {
		start++
	}
	for end > start && crud(s[end-1]) {
		end--
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '<' || r == '>' {
			return -1
		}
		return r
	}, s[start:end])
}

func crud(c byte) bool {
	return c <= 32 || c == ',' || c == ':' || c == ';' || c == '<' || c == '>' || c == '"' || c == '\\' || c == '\''
}

// A Commit is a commit object's fields. Extra holds the header lines after the
// committer, such as encoding, mergetag or gpgsig, verbatim with their
// newlines.
type Commit struct {
	Tree              ID
	Parents           []ID
	Author, Committer Signature
	Message           string
	Extra             []byte
}

// EncodeCommit returns the commit's object body.
func EncodeCommit(c Commit) []byte {
	b := make([]byte, 0, 160+len(c.Parents)*48+len(c.Extra)+len(c.Message)+len(c.Author.Name)+len(c.Committer.Name))
	b = append(b, "tree "...)
	b = append(b, c.Tree.String()...)
	b = append(b, '\n')
	for _, p := range c.Parents {
		b = append(b, "parent "...)
		b = append(b, p.String()...)
		b = append(b, '\n')
	}
	b = append(b, "author "...)
	b = c.Author.append(b)
	b = append(b, "\ncommitter "...)
	b = c.Committer.append(b)
	b = append(b, '\n')
	b = append(b, c.Extra...)
	b = append(b, '\n')
	return append(b, c.Message...)
}

// ErrNonCanonical is a commit body that EncodeCommit would not reproduce, which
// Git itself never writes.
var ErrNonCanonical = errors.New("commit is not in the form Git writes")

// ParseCommit reads a commit body. It refuses a body that EncodeCommit would
// not reproduce byte for byte, so a parsed commit always re-encodes to the
// object it came from.
func ParseCommit(body []byte) (Commit, error) {
	var c Commit
	rest := body
	line := func(prefix string) (string, bool) {
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 || !bytes.HasPrefix(rest, []byte(prefix)) {
			return "", false
		}
		value := string(rest[len(prefix):nl])
		rest = rest[nl+1:]
		return value, true
	}
	tree, ok := line("tree ")
	if !ok {
		return c, fmt.Errorf("%w: no tree", ErrNonCanonical)
	}
	var err error
	if c.Tree, err = ParseID(tree); err != nil {
		return c, fmt.Errorf("%w: tree %v", ErrNonCanonical, err)
	}
	for bytes.HasPrefix(rest, []byte("parent ")) {
		value, _ := line("parent ")
		parent, err := ParseID(value)
		if err != nil {
			return c, fmt.Errorf("%w: parent %v", ErrNonCanonical, err)
		}
		c.Parents = append(c.Parents, parent)
	}
	author, ok := line("author ")
	if !ok {
		return c, fmt.Errorf("%w: no author", ErrNonCanonical)
	}
	if c.Author, err = parseSignature(author); err != nil {
		return c, fmt.Errorf("%w: author %v", ErrNonCanonical, err)
	}
	committer, ok := line("committer ")
	if !ok {
		return c, fmt.Errorf("%w: no committer", ErrNonCanonical)
	}
	if c.Committer, err = parseSignature(committer); err != nil {
		return c, fmt.Errorf("%w: committer %v", ErrNonCanonical, err)
	}
	end := 0
	for end < len(rest) && rest[end] != '\n' {
		nl := bytes.IndexByte(rest[end:], '\n')
		if nl < 0 {
			return c, fmt.Errorf("%w: unterminated header", ErrNonCanonical)
		}
		end += nl + 1
	}
	if end == len(rest) {
		return c, fmt.Errorf("%w: no blank line before the message", ErrNonCanonical)
	}
	if end > 0 {
		c.Extra = bytes.Clone(rest[:end])
	}
	c.Message = string(rest[end+1:])
	if !bytes.Equal(EncodeCommit(c), body) {
		return c, ErrNonCanonical
	}
	return c, nil
}

func parseSignature(s string) (Signature, error) {
	var sig Signature
	lt := strings.IndexByte(s, '<')
	if lt < 1 || s[lt-1] != ' ' {
		return sig, errors.New("no space before the email")
	}
	sig.Name = s[:lt-1]
	gt := strings.IndexByte(s[lt:], '>')
	if gt < 0 {
		return sig, errors.New("unterminated email")
	}
	sig.Email = s[lt+1 : lt+gt]
	rest, ok := strings.CutPrefix(s[lt+gt+1:], " ")
	if !ok {
		return sig, errors.New("no space before the date")
	}
	date, zone, ok := strings.Cut(rest, " ")
	if !ok || zone == "" {
		return sig, errors.New("no time zone")
	}
	when, err := strconv.ParseInt(date, 10, 64)
	if err != nil || when < 0 || strconv.FormatInt(when, 10) != date {
		return sig, errors.New("bad date")
	}
	sig.When, sig.TZ = when, zone
	return sig, nil
}
