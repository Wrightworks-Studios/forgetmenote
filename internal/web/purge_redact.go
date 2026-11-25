package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"forgetmenote/internal/workspace"
)

// A purge removes content from the notebook's history; what quoted that
// content outside the notebook, such as private chat transcripts, Knowledge
// proposals and Jev records, is redacted by the system as the same purge,
// without anyone reading it. A purgeCleaner rewrites one stored JSON
// document: every string value that holds the purged content is redacted,
// strings that are JSON documents themselves included, and, after the purge,
// every revision the purge rewrote is mapped to the revision that stands for
// it. Keys, and the strings that identify, date or type a record, are not
// redacted, so the record still reads as itself; a revision among those
// strings is mapped all the same.

var revisionToken = regexp.MustCompile(`\b[0-9a-f]{40}\b`)

// structuralKeys name the string fields of stored records that identify,
// date or type a record rather than hold what someone wrote.
var structuralKeys = map[string]bool{
	"id": true, "owner": true, "account": true, "project": true, "notebook": true,
	"createdAt": true, "updatedAt": true, "created": true, "updated": true, "expires": true,
	"hash": true, "noteHash": true, "type": true, "kind": true, "status": true, "role": true, "run": true,
}

type purgeCleaner struct {
	// text redacts what a text purge erases (PurgeQuotes, or an Admin's text
	// or pattern); lines, for a note or versions purge, are the erased
	// versions' lines, each of which is redacted where a stored string has it
	// as a whole line.
	text  *workspace.PurgeRedactor
	lines map[string]bool
	// remap, after the purge, returns the revision standing for one the
	// purge rewrote, or false for a revision it did not rewrite.
	remap func(revision string) (string, bool)
	// owner, when set, restricts a count to the records it reports as the
	// caller's own: any other counts as unchanged, unread. Only counts use
	// it; a purge redacts everyone's records.
	owner func(raw []byte) bool
}

func (c *purgeCleaner) empty() bool { return c.text == nil && len(c.lines) == 0 && c.remap == nil }

// redactString returns s without the purged content.
func (c *purgeCleaner) redactString(s string) (string, bool) {
	changed := false
	if c.text != nil && c.text.Holds(s) {
		s, changed = c.text.Redact(s), true
	}
	if len(c.lines) > 0 && len(s) >= workspace.MinQuotedLine {
		lines, quoted := strings.Split(s, "\n"), false
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if len(trimmed) >= workspace.MinQuotedLine && c.lines[trimmed] {
				lines[i] = line[:strings.Index(line, trimmed)] + workspace.PurgedText
				quoted = true
			}
		}
		if quoted {
			s, changed = strings.Join(lines, "\n"), true
		}
	}
	if mapped, ok := c.remapString(s); ok {
		s, changed = mapped, true
	}
	return s, changed
}

// remapString returns s with every revision the purge rewrote mapped to the
// revision that stands for it.
func (c *purgeCleaner) remapString(s string) (string, bool) {
	if c.remap == nil || len(s) < 40 {
		return s, false
	}
	mapped := revisionToken.ReplaceAllStringFunc(s, func(token string) string {
		if now, ok := c.remap(token); ok {
			return now
		}
		return token
	})
	return mapped, mapped != s
}

// document rewrites one stored JSON document, returning it unchanged when
// nothing in it holds the purged content.
func (c *purgeCleaner) document(raw []byte) ([]byte, bool, error) {
	if c.owner != nil && !c.owner(raw) {
		return raw, false, nil
	}
	value, err := decodeJSONValue(raw)
	if err != nil {
		return nil, false, err
	}
	value, changed := c.value(value, 0)
	if !changed {
		return raw, false, nil
	}
	out, err := json.Marshal(value)
	return out, err == nil, err
}

func decodeJSONValue(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	if d.More() {
		return nil, errors.New("a stored document has trailing data")
	}
	return value, nil
}

func (c *purgeCleaner) value(v any, depth int) (any, bool) {
	switch t := v.(type) {
	case string:
		// A string holding a JSON document, such as a tool result, is cleaned
		// as a document, so its escaped strings are found too.
		if depth < 8 && len(t) > 1 && (t[0] == '{' || t[0] == '[') && json.Valid([]byte(t)) {
			if nested, err := decodeJSONValue([]byte(t)); err == nil {
				if out, changed := c.value(nested, depth+1); changed {
					if raw, err := json.Marshal(out); err == nil {
						return string(raw), true
					}
				}
			}
		}
		return c.redactString(t)
	case []any:
		changed := false
		for i, item := range t {
			if next, ok := c.value(item, depth); ok {
				t[i], changed = next, true
			}
		}
		return t, changed
	case map[string]any:
		changed := false
		for key, item := range t {
			if text, ok := item.(string); ok && structuralKeys[key] {
				if mapped, ok := c.remapString(text); ok {
					t[key], changed = mapped, true
				}
				continue
			}
			if next, ok := c.value(item, depth); ok {
				t[key], changed = next, true
			}
		}
		return t, changed
	}
	return v, false
}
