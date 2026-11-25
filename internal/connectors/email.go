package connectors

import (
	"net/mail"
	"slices"
	"strings"
	"time"
)

// EmailConfig owns mailbox and workflow policy; credentials stay in host secrets.
type EmailConfig struct {
	Provider   string   `json:"provider"`
	Mailbox    string   `json:"mailbox"`
	Workflow   string   `json:"workflow"`
	Recipients []string `json:"recipients,omitempty"`
}

type mailText struct {
	Text   string `json:"text"`
	Quoted bool   `json:"quoted"`
	Part   string `json:"part"`
}
type mailAttachment struct {
	ID       string `json:"id"`
	Part     string `json:"part"`
	Filename string `json:"filename"`
	MIME     string `json:"mimeType"`
	Size     int64  `json:"size"`
}
type messageView struct {
	Account     string            `json:"account"`
	ID          string            `json:"id"`
	ThreadID    string            `json:"threadId"`
	Revision    string            `json:"revision"`
	Headers     map[string]string `json:"headers"`
	Flags       []string          `json:"flags"`
	Subject     string            `json:"subject"`
	Sender      string            `json:"sender"`
	Recipients  []string          `json:"recipients"`
	Timestamp   string            `json:"timestamp"`
	Text        []mailText        `json:"text"`
	Attachments []mailAttachment  `json:"attachments"`
	Unsupported []string          `json:"unsupported"`
	Reference   string            `json:"reference"`
	RetrievedAt time.Time         `json:"retrievedAt"`
	Complete    bool              `json:"complete"`
	Fingerprint string            `json:"fingerprint"`
}

func emailAddress(value string) (string, error) {
	if strings.ContainsAny(value, "\r\n\x00") {
		return "", Rejected("invalid email address")
	}
	a, err := mail.ParseAddress(value)
	if err != nil || a.Address != value || !strings.Contains(value, "@") {
		return "", Rejected("expected one plain email address")
	}
	return strings.ToLower(a.Address), nil
}
func (r *runtime) checkRecipients(values []string) error {
	if len(values) == 0 {
		return Rejected("recipients required")
	}
	for _, v := range values {
		a, err := emailAddress(v)
		if err != nil || !slices.Contains(r.config.Email.Recipients, a) {
			return Rejected("recipient is outside configured allowlist")
		}
	}
	return nil
}

// Check the latest observed conversation; providers may still lack atomic
// preconditions. Their capability report must disclose that residual race.
func eligibleReply(thread any, sourceID, sourceHeader, excludedDraft string) (messageView, error) {
	view := object(thread)
	messages, _ := view["messages"].([]messageView)
	var source messageView
	for _, m := range messages {
		if (sourceID != "" && m.ID == sourceID) || (sourceHeader != "" && m.Headers["message-id"] == sourceHeader) {
			source = m
		}
	}
	if source.ID == "" || !source.Complete || source.Timestamp == "" || source.Sender == source.Account || slices.Contains(source.Flags, "draft") || slices.Contains(source.Flags, "sent") || source.Headers["auto-submitted"] != "" && source.Headers["auto-submitted"] != "no" || source.Headers["list-id"] != "" || slices.Contains([]string{"bulk", "list", "junk"}, strings.ToLower(source.Headers["precedence"])) {
		return source, Rejected("reply source is missing, incomplete or automated")
	}
	at, _ := time.Parse(time.RFC3339Nano, source.Timestamp)
	for _, m := range messages {
		if m.ID == source.ID || m.ID == excludedDraft {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, m.Timestamp)
		if err != nil || !t.Before(at) || slices.Contains(m.Flags, "draft") {
			return source, Rejected("thread has a newer message or another draft; reconcile before replying")
		}
	}
	return source, nil
}

func emailFilterSchema() map[string]any {
	properties := map[string]any{}
	for _, name := range []string{"after", "before", "from", "to", "subject"} {
		properties[name] = map[string]any{"type": "string"}
	}
	for _, name := range []string{"flags", "excludeFlags"} {
		properties[name] = map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"inbox", "sent", "draft", "trash", "spam", "unread", "starred"}}}
	}
	properties["hasAttachment"] = map[string]any{"type": "boolean"}
	return map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
}
