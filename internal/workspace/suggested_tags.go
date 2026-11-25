package workspace

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// WithAdditionalTags prepares a reviewable edit without writing a note. It uses
// the same restricted metadata parser as SetMetadata and preserves every other
// property and the body byte-for-byte.
func WithAdditionalTags(content []byte, additional []string) ([]byte, error) {
	metadata, ranges, lines, body, newline, err := metadataFields(content)
	if err != nil {
		return nil, err
	}
	tags := append([]string(nil), metadata.Tags...)
	seen := map[string]bool{}
	for _, tag := range tags {
		seen[strings.ToLower(tag)] = true
	}
	for _, tag := range additional {
		tag = strings.TrimSpace(tag)
		if tag == "" || len(tag) > 80 || !utf8.ValidString(tag) || strings.ContainsAny(tag, "\r\n\x00") {
			return nil, invalidInput("tags must be single-line text of at most 80 bytes")
		}
		if !seen[strings.ToLower(tag)] {
			tags = append(tags, tag)
			seen[strings.ToLower(tag)] = true
		}
	}
	if len(tags) > 50 {
		return nil, invalidInput("use at most 50 tags per note")
	}
	if len(tags) == len(metadata.Tags) {
		return append([]byte(nil), content...), nil
	}
	raw, _ := json.Marshal(tags)
	replacement := "tags: " + string(raw) + newline
	field, exists := ranges["tags"]
	var out strings.Builder
	out.WriteString("---" + newline)
	for i, line := range lines {
		if exists && i >= field.start && i < field.end {
			if i == field.start {
				out.WriteString(replacement)
			}
			continue
		}
		out.WriteString(line)
	}
	if !exists {
		out.WriteString(replacement)
	}
	out.WriteString("---" + newline)
	out.Write(body)
	return []byte(out.String()), nil
}
