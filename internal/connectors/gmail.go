package connectors

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

type mailHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type mailBody struct {
	Data         string `json:"data"`
	AttachmentID string `json:"attachmentId"`
	Size         int64  `json:"size"`
}
type mailPart struct {
	ID       string       `json:"partId"`
	MIME     string       `json:"mimeType"`
	Filename string       `json:"filename"`
	Headers  []mailHeader `json:"headers"`
	Body     mailBody     `json:"body"`
	Parts    []mailPart   `json:"parts"`
}
type mailMessage struct {
	ID           string   `json:"id"`
	ThreadID     string   `json:"threadId"`
	HistoryID    string   `json:"historyId"`
	InternalDate string   `json:"internalDate"`
	Labels       []string `json:"labelIds"`
	Payload      mailPart `json:"payload"`
}

func gmailCapabilities() map[string]any {
	return map[string]any{
		"incrementalChanges": true, "threads": true, "guardedReplies": true,
		"drafts": true, "send": true, "atomicThreadPreconditions": false,
		"maxPageSize": 500, "filters": []string{"after", "before", "from", "to", "subject", "flags", "excludeFlags", "hasAttachment"},
		"providerQuery": true,
	}
}

func gmailFlags(labels []string) []string {
	flags := []string{}
	for _, label := range labels {
		if slices.Contains([]string{"INBOX", "SENT", "DRAFT", "TRASH", "SPAM", "UNREAD", "STARRED"}, label) {
			flags = append(flags, strings.ToLower(label))
		}
	}
	return flags
}

func gmailSelection(in map[string]any) (string, error) {
	parts := []string{}
	if native := object(in["providerQuery"]); native != nil {
		if str(native["provider"]) != "gmail" {
			return "", Rejected("providerQuery does not match the email provider")
		}
		if q := strings.TrimSpace(str(native["query"])); q != "" {
			parts = append(parts, "("+q+")")
		}
	}
	filter := object(in["filter"])
	for _, name := range []string{"after", "before"} {
		if v, ok := filter[name]; ok {
			date, err := time.Parse(time.RFC3339, str(v))
			if err != nil || date.Unix() < 0 || date.Nanosecond() != 0 {
				return "", Rejected("email date filters require RFC3339 timestamps at whole-second precision")
			}
			parts = append(parts, name+":"+strconv.FormatInt(date.Unix(), 10))
		}
	}
	for _, name := range []string{"from", "to"} {
		if v, ok := filter[name]; ok {
			email, err := emailAddress(str(v))
			if err != nil {
				return "", err
			}
			// The structured filter accepts a mailbox address, never Gmail operators.
			if strings.ContainsAny(email, "\"\\(){} \t") {
				return "", Rejected("email address cannot be represented as a search filter")
			}
			parts = append(parts, name+":"+email)
		}
	}
	if v, ok := filter["subject"]; ok {
		subject := str(v)
		if subject == "" || strings.ContainsAny(subject, "\"\\\r\n\x00") {
			return "", Rejected("subject filter requires a nonempty phrase without quotes or control characters")
		}
		parts = append(parts, "subject:\""+subject+"\"")
	}
	for _, name := range []string{"flags", "excludeFlags"} {
		for _, v := range array(filter[name]) {
			flag := str(v)
			prefix := "in:"
			if flag == "unread" || flag == "starred" {
				prefix = "is:"
			}
			if name == "excludeFlags" {
				prefix = "-" + prefix
			}
			parts = append(parts, prefix+flag)
		}
	}
	if v, ok := filter["hasAttachment"].(bool); ok {
		term := "has:attachment"
		if !v {
			term = "-" + term
		}
		parts = append(parts, term)
	}
	return strings.Join(parts, " "), nil
}

// Omitted collections are empty; malformed collections must never masquerade
// as an exhausted page and advance an ingestion cursor.
func gmailItems(value map[string]any, name string) ([]any, error) {
	raw, exists := value[name]
	if !exists {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, errors.New("malformed email collection: " + name)
	}
	return items, nil
}
func gmailNext(value map[string]any) (string, error) {
	raw, exists := value["nextPageToken"]
	if !exists {
		return "", nil
	}
	next, ok := raw.(string)
	if !ok {
		return "", errors.New("malformed email page token")
	}
	return next, nil
}

func gmailChanges(value map[string]any) ([]any, error) {
	if !googleID.MatchString(str(value["historyId"])) {
		return nil, errors.New("email change page omitted its opaque cursor")
	}
	changes := []any{}
	events, err := gmailItems(value, "history")
	if err != nil {
		return nil, err
	}
	for _, raw := range events {
		event := object(raw)
		if event == nil {
			return nil, errors.New("malformed email change record")
		}
		seen := map[string]bool{}
		for _, name := range []string{"messagesAdded", "labelsAdded", "labelsRemoved", "messagesDeleted"} {
			items, err := gmailItems(event, name)
			if err != nil {
				return nil, err
			}
			for _, item := range items {
				id := str(object(object(item)["message"])["id"])
				if !googleID.MatchString(id) {
					return nil, errors.New("email change omitted message identity")
				}
				kind := "upsert"
				if name == "messagesDeleted" {
					kind = "deleted"
				}
				changes = append(changes, map[string]any{"id": id, "kind": kind})
				seen[id] = true
			}
		}
		// Gmail also provides a combined message list. Use it only for IDs not
		// already described by a specific event, preserving deletion semantics.
		items, err := gmailItems(event, "messages")
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			id := str(object(item)["id"])
			if !googleID.MatchString(id) {
				return nil, errors.New("email change omitted message identity")
			}
			if !seen[id] {
				changes = append(changes, map[string]any{"id": id, "kind": "upsert"})
			}
		}
	}
	return changes, nil
}
func decodeURL64(value string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(value, "="))
}
func headerMap(headers []mailHeader) map[string]string {
	out := map[string]string{}
	decoder := mime.WordDecoder{CharsetReader: func(label string, r io.Reader) (io.Reader, error) { return charset.NewReaderLabel(label, r) }}
	for _, h := range headers {
		key := strings.ToLower(h.Name)
		v, err := decoder.DecodeHeader(h.Value)
		if err != nil {
			v = h.Value
		}
		if old, ok := out[key]; ok {
			out[key] = old + ", " + v
		} else {
			out[key] = v
		}
	}
	return out
}
func addresses(value string) []string {
	out := []string{}
	parsed, err := mail.ParseAddressList(value)
	if err != nil {
		return out
	}
	for _, a := range parsed {
		out = append(out, strings.ToLower(a.Address))
	}
	return out
}
func visibleHTML(text string) (string, error) {
	root, err := html.Parse(strings.NewReader(text))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	var walk func(*html.Node, bool)
	walk = func(n *html.Node, quoted bool) {
		if n.Type == html.ElementNode && slices.Contains([]string{"script", "style", "head", "img", "iframe", "object"}, n.Data) {
			return
		}
		if n.Data == "blockquote" {
			quoted = true
		}
		if n.Type == html.TextNode {
			if quoted {
				b.WriteString("\n> ")
			}
			b.WriteString(n.Data)
		}
		if n.Type == html.ElementNode && slices.Contains([]string{"p", "div", "br", "li", "blockquote"}, n.Data) {
			b.WriteByte('\n')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, quoted)
		}
	}
	walk(root, false)
	return b.String(), nil
}
func bodyText(p mailPart) (string, error) {
	if p.Body.Data == "" && p.Body.Size > 0 {
		return "", errors.New("body is omitted; retrieve the selected attachment")
	}
	raw, err := decodeURL64(p.Body.Data)
	if err != nil {
		return "", errors.New("invalid base64url text")
	}
	label := "utf-8"
	contentType := headerMap(p.Headers)["content-type"]
	if contentType != "" {
		_, params, err := mime.ParseMediaType(contentType)
		if err != nil {
			return "", errors.New("unsupported Content-Type")
		}
		if params["charset"] != "" {
			label = params["charset"]
		}
	}
	if strings.EqualFold(label, "utf-8") || strings.EqualFold(label, "us-ascii") {
		if !utf8.Valid(raw) {
			return "", errors.New("invalid UTF-8 text")
		}
	} else {
		reader, err := charset.NewReaderLabel(label, bytes.NewReader(raw))
		if err != nil {
			return "", errors.New("unsupported character encoding")
		}
		raw, err = io.ReadAll(reader)
		if err != nil {
			return "", err
		}
	}
	text := string(raw)
	if p.MIME == "text/html" {
		return visibleHTML(text)
	}
	return text, nil
}
func parseMail(value map[string]any, account string) (messageView, error) {
	var m mailMessage
	raw, _ := json.Marshal(value)
	if err := json.Unmarshal(raw, &m); err != nil || m.ID == "" || m.ThreadID == "" {
		return messageView{}, errors.New("malformed Gmail message")
	}
	headers := headerMap(m.Payload.Headers)
	result := messageView{Account: account, ID: m.ID, ThreadID: m.ThreadID, Revision: m.HistoryID, Headers: headers, Flags: gmailFlags(m.Labels), Subject: headers["subject"], Recipients: append(append(addresses(headers["to"]), addresses(headers["cc"])...), addresses(headers["bcc"])...), Reference: "gmail:" + account + "/messages/" + m.ID, RetrievedAt: time.Now().UTC(), Complete: true, Text: []mailText{}, Attachments: []mailAttachment{}, Unsupported: []string{}}
	senders := addresses(headers["from"])
	if len(senders) == 1 {
		result.Sender = senders[0]
	}
	at, err := strconv.ParseInt(m.InternalDate, 10, 64)
	if err == nil {
		result.Timestamp = time.UnixMilli(at).UTC().Format(time.RFC3339Nano)
	} else {
		result.Unsupported = append(result.Unsupported, "missing or invalid internalDate")
	}
	var visit func(mailPart)
	visit = func(p mailPart) {
		if p.Filename != "" || p.Body.AttachmentID != "" {
			result.Attachments = append(result.Attachments, mailAttachment{p.Body.AttachmentID, p.ID, p.Filename, p.MIME, p.Body.Size})
			if p.Body.AttachmentID == "" {
				result.Unsupported = append(result.Unsupported, p.ID+": inline attachment has no retrievable provider attachment ID")
			}
			return
		}
		if p.MIME == "multipart/alternative" {
			for _, part := range p.Parts {
				if part.MIME == "text/plain" {
					visit(part)
					return
				}
			}
			for _, part := range p.Parts {
				if part.MIME == "text/html" {
					visit(part)
					return
				}
			}
			if len(p.Parts) > 0 {
				visit(p.Parts[0])
			}
			return
		}
		if strings.HasPrefix(p.MIME, "multipart/") {
			for _, part := range p.Parts {
				visit(part)
			}
			return
		}
		if p.MIME != "text/plain" && p.MIME != "text/html" {
			result.Unsupported = append(result.Unsupported, p.ID+": unsupported "+p.MIME)
			return
		}
		text, err := bodyText(p)
		if err != nil {
			result.Unsupported = append(result.Unsupported, p.ID+": "+err.Error())
			return
		}
		quoted := false
		for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
			trim := strings.TrimSpace(line)
			if strings.HasPrefix(trim, "On ") && strings.HasSuffix(trim, " wrote:") || strings.Contains(trim, "Original Message") {
				quoted = true
			}
			q := quoted || strings.HasPrefix(trim, ">")
			if len(result.Text) > 0 && result.Text[len(result.Text)-1].Quoted == q && result.Text[len(result.Text)-1].Part == p.ID {
				result.Text[len(result.Text)-1].Text += "\n" + line
			} else {
				result.Text = append(result.Text, mailText{line, q, p.ID})
			}
		}
	}
	visit(m.Payload)
	result.Complete = len(result.Unsupported) == 0
	result.Fingerprint = hash([]any{m.ID, m.ThreadID, m.HistoryID, m.InternalDate, m.Labels, m.Payload})
	return result, nil
}
func gmailPath(kind, id string) (string, error) {
	if !googleID.MatchString(id) {
		return "", Rejected("invalid Gmail ID")
	}
	return "/gmail/v1/users/me/" + kind + "/" + url.PathEscape(id), nil
}
func (r *runtime) getMail(ctx context.Context, token, id, account string) (messageView, error) {
	p, err := gmailPath("messages", id)
	if err != nil {
		return messageView{}, err
	}
	value, err := r.googleJSON(ctx, token, "GET", p, url.Values{"format": {"full"}}, nil)
	if err != nil {
		return messageView{}, err
	}
	view, err := parseMail(value, account)
	if err == nil && view.ID != id {
		err = errors.New("Gmail message identity mismatch")
	}
	return view, err
}
func (r *runtime) gmailCall(ctx context.Context, token, name string, in map[string]any, account string) (any, error) {
	query := url.Values{}
	if v := str(in["pageToken"]); v != "" {
		query.Set("pageToken", v)
	}
	if size, ok := in["pageSize"].(float64); ok {
		if size < 1 || size > 500 {
			return nil, Rejected("pageSize must be 1–500 per Gmail API")
		}
		query.Set("maxResults", strconv.Itoa(int(size)))
	}
	switch name {
	case "messages", "changes":
		path := "/gmail/v1/users/me/messages"
		if name == "messages" {
			selection, err := gmailSelection(in)
			if err != nil {
				return nil, err
			}
			query.Set("q", selection)
			query.Set("includeSpamTrash", "true")
		} else {
			path = "/gmail/v1/users/me/history"
			if !googleID.MatchString(str(in["cursor"])) {
				return nil, Rejected("change cursor must be an opaque provider ID")
			}
			query.Set("startHistoryId", str(in["cursor"]))
		}
		value, err := r.googleJSON(ctx, token, "GET", path, query, nil)
		if name == "changes" && googleStatus(err, 404) {
			return map[string]any{"account": account, "expired": true, "complete": false, "coverage": "Change cursor expired; selected-scope rescan required"}, nil
		}
		if err != nil {
			return nil, err
		}
		next, err := gmailNext(value)
		if err != nil {
			return nil, err
		}
		result := map[string]any{"account": account, "retrievedAt": time.Now().UTC(), "complete": true, "hasMore": next != "", "nextPageToken": next}
		if name == "messages" {
			messages := []any{}
			items, err := gmailItems(value, "messages")
			if err != nil {
				return nil, err
			}
			for _, raw := range items {
				m := object(raw)
				if !googleID.MatchString(str(m["id"])) {
					return nil, errors.New("email list omitted message identity")
				}
				messages = append(messages, map[string]any{"id": m["id"], "threadId": m["threadId"]})
			}
			result["messages"] = messages
		} else {
			changes, err := gmailChanges(value)
			if err != nil {
				return nil, err
			}
			result["changes"], result["cursor"] = changes, value["historyId"]
		}
		return result, nil
	case "message":
		value, err := r.getMail(ctx, token, str(in["id"]), account)
		if googleStatus(err, 404) {
			return map[string]any{"account": account, "id": in["id"], "unavailable": true, "complete": false}, nil
		}
		return value, err
	case "thread":
		path, err := gmailPath("threads", str(in["id"]))
		if err != nil {
			return nil, err
		}
		value, err := r.googleJSON(ctx, token, "GET", path, url.Values{"format": {"full"}}, nil)
		if err != nil {
			return nil, err
		}
		messages := []messageView{}
		complete := true
		for _, raw := range array(value["messages"]) {
			v, err := parseMail(object(raw), account)
			if err != nil {
				return nil, err
			}
			if v.ThreadID != str(in["id"]) {
				return nil, errors.New("Gmail thread contains a mismatched message")
			}
			messages = append(messages, v)
			complete = complete && v.Complete
		}
		if len(messages) == 0 || str(value["id"]) != str(in["id"]) {
			return nil, errors.New("Gmail thread omitted messages")
		}
		return map[string]any{"account": account, "id": value["id"], "revision": value["historyId"], "messages": messages, "complete": complete, "retrievedAt": time.Now().UTC()}, nil
	case "selection":
		message, err := r.getMail(ctx, token, str(in["id"]), account)
		if googleStatus(err, 404) {
			return map[string]any{"selected": false, "unavailable": true}, nil
		}
		if err != nil {
			return nil, err
		}
		id := message.Headers["message-id"]
		if !messageIDPattern.MatchString(id) {
			return map[string]any{"selected": false, "complete": false, "reason": "message has no searchable RFC Message-ID"}, nil
		}
		selection, err := gmailSelection(in)
		if err != nil {
			return nil, err
		}
		if selection != "" {
			selection = "(" + selection + ") "
		}
		query.Set("q", selection+"rfc822msgid:"+strings.Trim(id, "<>"))
		query.Set("maxResults", "500")
		query.Set("includeSpamTrash", "true")
		seen := map[string]bool{}
		for {
			value, err := r.googleJSON(ctx, token, "GET", "/gmail/v1/users/me/messages", query, nil)
			if err != nil {
				return nil, err
			}
			items, err := gmailItems(value, "messages")
			if err != nil {
				return nil, err
			}
			for _, item := range items {
				if str(object(item)["id"]) == message.ID {
					return map[string]any{"selected": true, "complete": true, "account": account}, nil
				}
			}
			next, err := gmailNext(value)
			if err != nil {
				return nil, err
			}
			if next == "" {
				break
			}
			if seen[next] {
				return nil, errors.New("Gmail selection pagination repeated")
			}
			seen[next] = true
			query.Set("pageToken", next)
		}
		return map[string]any{"selected": false, "complete": true, "account": account}, nil
	case "attachment":
		message, err := r.getMail(ctx, token, str(in["messageId"]), account)
		if err != nil {
			return nil, err
		}
		id := str(in["attachmentId"])
		for _, a := range message.Attachments {
			if a.ID == id && id != "" {
				if !googleID.MatchString(id) {
					return nil, Rejected("invalid attachment ID")
				}
				path, _ := gmailPath("messages", message.ID)
				value, err := r.googleJSON(ctx, token, "GET", path+"/attachments/"+url.PathEscape(id), nil, nil)
				if err != nil {
					return nil, err
				}
				b, err := decodeURL64(str(value["data"]))
				if err != nil || int64(len(b)) != a.Size {
					return nil, errors.New("attachment body is incomplete")
				}
				return map[string]any{"account": account, "messageId": message.ID, "attachment": a, "content": base64.StdEncoding.EncodeToString(b), "sha256": fmt.Sprintf("%x", sha256.Sum256(b)), "reference": message.Reference + "/attachments/" + id, "retrievedAt": time.Now().UTC(), "complete": true}, nil
			}
		}
		return nil, Rejected("attachment is not part of the selected message")
	case "draft":
		path, err := gmailPath("drafts", str(in["id"]))
		if err != nil {
			return nil, err
		}
		value, err := r.googleJSON(ctx, token, "GET", path, url.Values{"format": {"full"}}, nil)
		if err != nil {
			return nil, err
		}
		if str(value["id"]) != str(in["id"]) {
			return nil, errors.New("Gmail draft identity mismatch")
		}
		message, err := parseMail(object(value["message"]), account)
		if err != nil {
			return nil, err
		}
		if message.Headers["x-forgetmenote-workflow"] != r.config.Email.Workflow {
			return nil, Rejected("draft is not owned by this configured workflow")
		}
		return map[string]any{"id": value["id"], "message": message, "fingerprint": message.Fingerprint, "complete": message.Complete, "account": account}, nil
	default:
		return r.gmailWrite(ctx, token, name, in, account)
	}
}

var messageIDPattern = regexp.MustCompile(`^<[A-Za-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[A-Za-z0-9.-]+>$`)

func (r *runtime) gmailWrite(ctx context.Context, token, name string, in map[string]any, account string) (any, error) {
	action := str(in["actionID"])
	if !identifier.MatchString(action) {
		return nil, Rejected("stable actionID required")
	}
	var existing map[string]any
	if name == "draft-update" || name == "draft-send" {
		v, err := r.gmailCall(ctx, token, "draft", map[string]any{"id": in["id"]}, account)
		if err != nil {
			return nil, Rejected(err.Error())
		}
		existing = v.(map[string]any)
		if existing["fingerprint"] != in["expectedFingerprint"] {
			return nil, Rejected("draft changed; inspect the current draft before updating or sending")
		}
	}
	payload := map[string]any{}
	if name == "draft-send" {
		message := existing["message"].(messageView)
		if err := r.checkRecipients(message.Recipients); err != nil {
			return nil, err
		}
		if message.Sender != account || message.Headers["auto-submitted"] != "auto-replied" {
			return nil, Rejected("draft sender or workflow headers changed")
		}
		thread, err := r.gmailCall(ctx, token, "thread", map[string]any{"id": message.ThreadID}, account)
		if err != nil {
			return nil, Rejected(err.Error())
		}
		if object(thread)["revision"] != in["threadRevision"] {
			return nil, Rejected("thread changed before draft send")
		}
		source, err := eligibleReply(thread, "", message.Headers["in-reply-to"], message.ID)
		if err != nil {
			return nil, err
		}
		if len(message.Recipients) != 1 || message.Recipients[0] != source.Sender || (message.Subject != source.Subject && message.Subject != "Re: "+source.Subject) {
			return nil, Rejected("draft reply headers changed")
		}
		payload["id"] = in["id"]
	} else {
		recipients := []string{}
		for _, a := range array(in["to"]) {
			recipients = append(recipients, str(a))
		}
		if err := r.checkRecipients(recipients); err != nil {
			return nil, err
		}
		subject, text := str(in["subject"]), str(in["text"])
		if strings.ContainsAny(subject, "\r\n\x00") || subject == "" || strings.ContainsRune(text, 0) {
			return nil, Rejected("invalid message subject or body")
		}
		headers := "From: " + account + "\r\nTo: " + strings.Join(recipients, ", ") + "\r\nSubject: " + mime.QEncoding.Encode("UTF-8", subject) + "\r\nMessage-ID: <" + hash([]string{r.config.Email.Workflow, action}) + "@forgetmenote.invalid>\r\nX-ForgetMeNote-Workflow: " + r.config.Email.Workflow + "\r\nAuto-Submitted: auto-replied\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n"
		if id := str(in["replyToMessage"]); id != "" {
			original, err := r.getMail(ctx, token, id, account)
			if err != nil {
				return nil, Rejected(err.Error())
			}
			if original.Sender == account || slices.Contains(original.Flags, "sent") || slices.Contains(original.Flags, "draft") || original.Headers["auto-submitted"] != "" && original.Headers["auto-submitted"] != "no" || original.Headers["list-id"] != "" || slices.Contains([]string{"bulk", "list", "junk"}, strings.ToLower(original.Headers["precedence"])) {
				return nil, Rejected("automated notice or self-reply is not eligible")
			}
			if len(recipients) != 1 || !strings.EqualFold(recipients[0], original.Sender) {
				return nil, Rejected("reply recipient must be the original sender within the configured allowlist")
			}
			if subject != original.Subject && subject != "Re: "+original.Subject {
				return nil, Rejected("reply subject does not match thread")
			}
			mid := original.Headers["message-id"]
			if !messageIDPattern.MatchString(mid) {
				return nil, Rejected("reply requires a valid RFC Message-ID")
			}
			thread, err := r.gmailCall(ctx, token, "thread", map[string]any{"id": original.ThreadID}, account)
			if err != nil {
				return nil, Rejected(err.Error())
			}
			if str(object(thread)["revision"]) != str(in["threadRevision"]) || str(in["threadRevision"]) == "" {
				return nil, Rejected("thread changed; refresh the reply precondition")
			}
			exclude := ""
			if existing != nil {
				exclude = existing["message"].(messageView).ID
			}
			if _, err := eligibleReply(thread, original.ID, "", exclude); err != nil {
				return nil, err
			}
			refs := []string{}
			for _, ref := range strings.Fields(original.Headers["references"]) {
				if !messageIDPattern.MatchString(ref) {
					return nil, Rejected("invalid References header")
				}
				refs = append(refs, ref)
			}
			refs = append(refs, mid)
			headers += "In-Reply-To: " + mid + "\r\nReferences: " + strings.Join(refs, " ") + "\r\n"
			payload["threadId"] = original.ThreadID
		} else if name == "send" {
			return nil, Rejected("automatic sending requires a source message and fresh thread guard")
		}
		encoded := base64.StdEncoding.EncodeToString([]byte(text))
		var folded strings.Builder
		for len(encoded) > 76 {
			folded.WriteString(encoded[:76] + "\r\n")
			encoded = encoded[76:]
		}
		folded.WriteString(encoded + "\r\n")
		payload["raw"] = base64.RawURLEncoding.EncodeToString([]byte(headers + "\r\n" + folded.String()))
	}
	method, path := "POST", "/gmail/v1/users/me/drafts"
	var body any = map[string]any{"message": payload}
	switch name {
	case "draft-create":
	case "draft-update":
		var err error
		path, err = gmailPath("drafts", str(in["id"]))
		if err != nil {
			return nil, err
		}
		method = "PUT"
	case "send":
		path = "/gmail/v1/users/me/messages/send"
		body = payload
	case "draft-send":
		path = "/gmail/v1/users/me/drafts/send"
		body = payload
	default:
		return nil, Rejected("unsupported Gmail write")
	}
	value, err := r.googleJSON(ctx, token, method, path, nil, body)
	if err != nil {
		return nil, err
	}
	id := str(value["id"])
	message := value
	if strings.HasPrefix(name, "draft-") && name != "draft-send" {
		message = object(value["message"])
	}
	if !googleID.MatchString(id) || !googleID.MatchString(str(message["id"])) || !googleID.MatchString(str(message["threadId"])) {
		return nil, Unknown("Google accepted request but acknowledgment identifiers are invalid")
	}
	expectedThread := str(payload["threadId"])
	if name == "draft-send" {
		expectedThread = existing["message"].(messageView).ThreadID
	}
	if expectedThread != "" && str(message["threadId"]) != expectedThread {
		return nil, Unknown("Gmail acknowledgment thread mismatch")
	}
	return map[string]any{"account": account, "id": id, "messageId": message["id"], "threadId": message["threadId"], "accepted": true, "deliveryConfirmed": false, "actionID": action, "retrievedAt": time.Now().UTC()}, nil
}
