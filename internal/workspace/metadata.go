package workspace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	pathpkg "path"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Metadata struct {
	ID             string        `json:"id,omitempty"`
	Container      string        `json:"container,omitempty"`
	ContainerError string        `json:"containerError,omitempty"`
	Source         *SourceRecord `json:"source,omitempty"`
	Cover          *string       `json:"cover,omitempty"`
	Order          *float64      `json:"order,omitempty"`
	Title          string        `json:"title"`
	Kind           string        `json:"kind"`
	Status         string        `json:"status"`
	Tags           []string      `json:"tags"`
}
type fieldRange struct{ start, end int }

// Properties deliberately support flat text fields and string tag lists, not a
// second general-purpose YAML editor. Unknown fields retain their original bytes.
func frontMatter(content []byte) (lines []string, body []byte, newline string, err error) {
	newline = "\n"
	if bytes.HasPrefix(content, []byte("---\r\n")) {
		newline = "\r\n"
	} else if !bytes.HasPrefix(content, []byte("---\n")) {
		return nil, content, newline, nil
	}
	// Only the header needs line storage. Splitting a whole large note here
	// copies and indexes its body again for every metadata/identity read.
	lines = []string{}
	offset := len("---") + len(newline)
	for offset < len(content) {
		line := content[offset:]
		if end := bytes.IndexByte(line, '\n'); end >= 0 {
			line = line[:end+1]
		}
		offset += len(line)
		if bytes.Equal(bytes.TrimRight(line, "\r\n"), []byte("---")) {
			return lines, content[offset:], newline, nil
		}
		if offset > 65536 {
			break
		}
		lines = append(lines, string(line))
	}
	return nil, content, newline, invalidInput("front matter needs a closing --- line within 64 KiB")
}

// Reserved fields must still trigger validation in malformed or oversized
// headers. Scan their actual extent without allocating a copy of the note body.
func hasFrontMatterField(content []byte, field string) bool {
	if !bytes.HasPrefix(content, []byte("---\n")) && !bytes.HasPrefix(content, []byte("---\r\n")) {
		return false
	}
	_, remaining, _ := bytes.Cut(content, []byte("\n"))
	for len(remaining) > 0 {
		var line []byte
		line, remaining, _ = bytes.Cut(remaining, []byte("\n"))
		if bytes.Equal(bytes.TrimSuffix(line, []byte("\r")), []byte("---")) {
			break
		}
		key, _, ok := bytes.Cut(line, []byte(":"))
		if ok {
			value, err := scalar(string(key))
			if err == nil && value == field {
				return true
			}
		}
	}
	return false
}

func scalar(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") {
		return "", nil
	}
	if raw[0] == '"' {
		// Find the closing quote without treating a quoted # as a comment.
		end := 1
		for end < len(raw) {
			if raw[end] == '\\' {
				end += 2
				continue
			}
			if raw[end] == '"' {
				break
			}
			end++
		}
		if end >= len(raw) {
			return "", invalidInput("unclosed quoted value")
		}
		rest := strings.TrimSpace(raw[end+1:])
		if rest != "" && !strings.HasPrefix(rest, "#") {
			return "", invalidInput("unexpected text after value")
		}
		return strconv.Unquote(raw[:end+1])
	}
	if raw[0] == '\'' {
		var value strings.Builder
		for i := 1; i < len(raw); i++ {
			if raw[i] != '\'' {
				value.WriteByte(raw[i])
				continue
			}
			if i+1 < len(raw) && raw[i+1] == '\'' {
				value.WriteByte('\'')
				i++
				continue
			}
			rest := strings.TrimSpace(raw[i+1:])
			if rest != "" && !strings.HasPrefix(rest, "#") {
				break
			}
			return value.String(), nil
		}
		return "", invalidInput("unclosed quoted value")
	}
	if i := strings.Index(raw, " #"); i >= 0 {
		raw = strings.TrimSpace(raw[:i])
	}
	if strings.ContainsAny(raw, "\r\n") || strings.ContainsAny(raw[:1], "[{|>&*!%@`}") || strings.Contains(raw, ": ") {
		return "", invalidInput("use a simple text value or edit this field in Markdown")
	}
	return raw, nil
}
func tagList(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{}, nil
	}
	if !strings.HasPrefix(raw, "[") {
		return nil, invalidInput("tags must be a list")
	}
	tags := []string{}
	start := 1
	quote := byte(0)
	for i := 1; i < len(raw); i++ {
		c := raw[i]
		if quote != 0 {
			if quote == '"' && c == '\\' {
				i++
				continue
			}
			if c == quote {
				if quote == '\'' && i+1 < len(raw) && raw[i+1] == '\'' {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c != ',' && c != ']' {
			continue
		}
		part := strings.TrimSpace(raw[start:i])
		if part != "" {
			v, err := scalar(part)
			if err != nil {
				return nil, err
			}
			tags = append(tags, v)
		}
		start = i + 1
		if c == ']' {
			rest := strings.TrimSpace(raw[i+1:])
			if rest != "" && !strings.HasPrefix(rest, "#") {
				return nil, invalidInput("unexpected text after tags")
			}
			return tags, nil
		}
	}
	return nil, invalidInput("unclosed tag list")
}
func metadataFields(content []byte) (Metadata, map[string]fieldRange, []string, []byte, string, error) {
	m := Metadata{Tags: []string{}}
	ranges := map[string]fieldRange{}
	lines, body, newline, err := frontMatter(content)
	if err != nil {
		return m, ranges, lines, body, newline, err
	}
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r\n")
		key, value, ok := strings.Cut(line, ":")
		if ok && key == "order" {
			// Wiki navigation order is optional reading guidance: anything but
			// one plain finite number stays in the Markdown and is never an error.
			if v, e := scalar(value); e == nil && m.Order == nil {
				if n, e := strconv.ParseFloat(v, 64); e == nil && !math.IsInf(n, 0) && !math.IsNaN(n) {
					m.Order = &n
				}
			}
			continue
		}
		if !ok || !(key == "title" || key == "kind" || key == "status" || key == "tags" || key == "cover") {
			continue
		}
		if _, exists := ranges[key]; exists {
			return m, ranges, lines, body, newline, invalidInput(fmt.Sprintf("duplicate %s field; edit it in Markdown", key))
		}
		r := fieldRange{i, i + 1}
		if key == "tags" {
			m.Tags, err = tagList(value)
			if strings.TrimSpace(value) == "" {
				for j := i + 1; j < len(lines); j++ {
					trimmed := strings.TrimSpace(lines[j])
					if !strings.HasPrefix(trimmed, "- ") {
						if trimmed == "" || strings.HasPrefix(trimmed, "#") {
							for k := j + 1; k < len(lines); k++ {
								tail := strings.TrimSpace(lines[k])
								if tail == "" || strings.HasPrefix(tail, "#") {
									continue
								}
								if strings.HasPrefix(tail, "- ") {
									return m, ranges, lines, body, newline, invalidInput("tag lists with interleaved comments or blank lines must be edited in Markdown")
								}
								break
							}
						}
						break
					}
					var v string
					v, err = scalar(strings.TrimPrefix(trimmed, "- "))
					if err != nil {
						break
					}
					m.Tags = append(m.Tags, v)
					r.end = j + 1
				}
			}
		} else {
			var v string
			v, err = scalar(value)
			switch key {
			case "cover":
				m.Cover = &v
			case "title":
				m.Title = v
			case "kind":
				m.Kind = v
			case "status":
				m.Status = v
			}
		}
		if err != nil {
			return m, ranges, lines, body, newline, fmt.Errorf("%s: %w", key, err)
		}
		if r.end < len(lines) {
			next := lines[r.end]
			trimmed := strings.TrimSpace(next)
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") && (strings.HasPrefix(next, " ") || strings.HasPrefix(next, "\t")) {
				return m, ranges, lines, body, newline, invalidInput(fmt.Sprintf("complex %s value; edit it in Markdown", key))
			}
		}
		ranges[key] = r
		i = r.end - 1
	}
	return m, ranges, lines, body, newline, nil
}
func ParseMetadata(content []byte) (Metadata, error) {
	m, _, _, _, _, err := metadataFields(content)
	var idErr error
	m.ID, idErr = NoteID(content)
	m.Source = ParseSource(content)
	var containerErr error
	m.Container, containerErr = ParseContainer(content)
	if containerErr != nil {
		m.ContainerError = containerErr.Error()
	}
	return m, errors.Join(err, containerErr, idErr)
}
func MarkdownBody(content []byte) []byte {
	_, body, _, err := frontMatter(content)
	if err != nil {
		return content
	}
	return body
}

// Published metadata cache values are immutable. Return private nested values
// so transport and AI callers cannot alter later reads by editing their result.
func cloneEntry(entry Entry) Entry {
	if entry.Metadata != nil {
		metadata := *entry.Metadata
		metadata.Tags = slices.Clone(metadata.Tags)
		if metadata.Cover != nil {
			value := *metadata.Cover
			metadata.Cover = &value
		}
		if metadata.Order != nil {
			value := *metadata.Order
			metadata.Order = &value
		}
		if metadata.Source != nil {
			value := *metadata.Source
			metadata.Source = &value
		}
		entry.Metadata = &metadata
	}
	return entry
}

// Covers use project-root paths, independent of the note's folder.
func ValidCoverPath(value string) bool {
	if len(value) > 512 || !utf8.ValidString(value) || !strings.HasPrefix(value, "assets/") || pathpkg.Clean(value) != value || strings.ContainsAny(value, "\\\x00\r\n?#%") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	switch strings.ToLower(pathpkg.Ext(value)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".avif":
		return true
	}
	return false
}

func rewriteCover(source []byte, from, to string) []byte {
	m, ranges, lines, body, newline, err := metadataFields(source)
	if err != nil || m.Cover == nil || !ValidCoverPath(*m.Cover) {
		return source
	}
	next := movedPath(*m.Cover, from, to)
	if next == *m.Cover {
		return source
	}
	raw, _ := json.Marshal(next)
	lines[ranges["cover"].start] = "cover: " + string(raw) + newline
	return []byte("---" + newline + strings.Join(lines, "") + "---" + newline + string(body))
}
