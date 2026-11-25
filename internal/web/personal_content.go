package web

import (
	"encoding/json"
	"slices"
	"strings"

	"forgetmenote/internal/automation"
)

// Mailbox labels and provider history counters do not change what an employee
// was asked. Keep the full original evidence for guarded writes, but key paid
// analysis by message content and disposition rather than transport metadata.
func personalContentRevision(kind string, raw json.RawMessage) string {
	if kind != "email" && kind != "slack" {
		return automation.Hash(raw)
	}
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil {
		return automation.Hash(raw)
	}
	if kind == "email" {
		if messages, ok := data["messages"].([]any); ok {
			data["messages"] = slices.DeleteFunc(messages, func(v any) bool { return slices.Contains(personalArray(personalObject(v)["flags"]), any("draft")) })
		}
	}
	var clean func(any)
	clean = func(v any) {
		switch value := v.(type) {
		case map[string]any:
			for _, key := range []string{"revision", "fingerprint", "historyId", "retrievedAt", "response_metadata", "reactions", "last_read", "unread_count", "unread_count_display"} {
				delete(value, key)
			}
			if flags, ok := value["flags"].([]any); ok {
				value["flags"] = slices.DeleteFunc(flags, func(v any) bool {
					return !slices.Contains([]string{"spam", "trash", "draft", "sent"}, personalString(v))
				})
			}
			for _, child := range value {
				clean(child)
			}
		case []any:
			for _, child := range value {
				clean(child)
			}
		}
	}
	clean(data)
	return automation.Hash(data)
}

func personalExcerpt(o personalObservation, limit int) string {
	var data map[string]any
	_ = json.Unmarshal(o.Content, &data)
	text := string(o.Content)
	if messages := personalArray(data["messages"]); len(messages) > 0 {
		last := personalObject(messages[len(messages)-1])
		text = personalString(last["subject"]) + "\n" + personalString(last["sender"]) + "\n"
		if o.Kind == "slack" {
			text += personalString(last["text"])
		} else {
			for _, part := range personalArray(last["text"]) {
				segment := personalObject(part)
				if segment["quoted"] != true {
					text += personalString(segment["text"]) + "\n"
				}
			}
		}
	} else if o.Kind == "calendar" {
		text = personalString(data["summary"]) + "\n" + personalString(data["description"])
		start, _ := json.Marshal(data["start"])
		text += "\n" + string(start)
	}
	runes := []rune(strings.TrimSpace(text))
	if len(runes) > limit {
		runes = append(runes[:limit], []rune("… [excerpt]")...)
	}
	return string(runes)
}
