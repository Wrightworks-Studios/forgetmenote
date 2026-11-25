package workspace

import (
	"bytes"
	"strings"
)

const containerField = "fmn-container"

// Quoted spellings still name the reserved property. Recognize them so they
// cannot bypass classification, but keep authoring to one portable key syntax.
func isContainerKey(key string) bool {
	value, err := scalar(key)
	return err == nil && value == containerField
}

// Container is committed organization, independent of editorial status and path.
// Empty is ordinary Notes. Invalid imported values remain readable for repair.
func ParseContainer(content []byte) (string, error) {
	lines, _, _, err := frontMatter(content)
	if err != nil {
		return "", err
	}
	value, found := "", false
	for i, line := range lines {
		key, raw, ok := strings.Cut(strings.TrimRight(line, "\r\n"), ":")
		if !ok || !isContainerKey(key) {
			continue
		}
		if key != containerField || found {
			return "", invalidInput("use one unquoted top-level fmn-container key; edit it in Markdown")
		}
		found = true
		value, err = scalar(raw)
		if err != nil || value != "draft" && value != "archive" {
			return "", invalidInput("fmn-container must be draft or archive; remove the field for Notes")
		}
		for _, next := range lines[i+1:] {
			trimmed := strings.TrimSpace(next)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.HasPrefix(next, " ") || strings.HasPrefix(next, "\t") {
				return "", invalidInput("complex fmn-container; edit it in Markdown")
			}
			break
		}
	}
	return value, nil
}

func validateContainerWrite(content []byte) error {
	if hasFrontMatterField(content, containerField) {
		_, err := ParseContainer(content)
		return err
	}
	return nil
}

// WithContainer returns content with its fmn-container field set to container
// ("" for Notes), preserving all other front matter and the body. Single-note
// and folder moves share it so both produce identical Markdown.
func WithContainer(content []byte, container string) ([]byte, error) {
	if container != "" && container != "draft" && container != "archive" {
		return nil, invalidInput("choose Notes, Drafts, or Archives")
	}
	lines, body, newline, err := frontMatter(content)
	if err != nil {
		return nil, err
	}
	var output strings.Builder
	kept := []string{}
	for _, line := range lines {
		key, _, ok := strings.Cut(line, ":")
		if ok && key == containerField {
			continue
		}
		kept = append(kept, line)
	}
	if len(lines) > 0 || container != "" || bytes.HasPrefix(content, []byte("---\n")) || bytes.HasPrefix(content, []byte("---\r\n")) {
		output.WriteString("---" + newline)
		for _, line := range kept {
			output.WriteString(line)
		}
		if container != "" {
			output.WriteString(containerField + ": " + container + newline)
		}
		output.WriteString("---" + newline)
	}
	output.Write(body)
	return []byte(output.String()), nil
}

func ValidCollection(collection string) bool {
	return collection == "" || collection == "all" || collection == "notes" || collection == "drafts" || collection == "archives"
}

func InCollection(metadata *Metadata, collection string) bool {
	if collection == "" || collection == "all" {
		return true
	}
	if metadata == nil || metadata.ContainerError != "" {
		return collection == "notes"
	}
	switch collection {
	case "notes":
		return metadata.Container == ""
	case "drafts":
		return metadata.Container == "draft"
	case "archives":
		return metadata.Container == "archive"
	}
	return false
}

type Collections struct {
	Version    int    `json:"version"`
	Revision   string `json:"revision"`
	Visibility string `json:"visibility"`
	Notes      int    `json:"notes"`
	Drafts     int    `json:"drafts"`
	Archives   int    `json:"archives"`
	Invalid    int    `json:"invalid"`
}
