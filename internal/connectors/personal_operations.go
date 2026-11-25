package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/capacity"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// PersonalOperation is the fixed, provider-neutral host contract. A caller
// cannot supply a URL, token, mailbox identity, or arbitrary HTTP operation.
type PersonalOperation struct {
	Provider   string         `json:"provider"`
	Generation uint64         `json:"generation"`
	Operation  string         `json:"operation"`
	Arguments  map[string]any `json:"arguments"`
}

func PersonalRead(operation string) bool {
	return slices.Contains([]string{"email.status", "email.messages", "email.message", "email.thread", "email.attachment", "email.changes", "email.draft", "calendar.list", "calendar.events", "calendar.event", "documents.search", "documents.read", "sheets.read", "slack.conversations", "slack.history", "slack.thread", "slack.permalink"}, operation)
}

func PersonalFeature(operation string) string {
	part, _, _ := strings.Cut(operation, ".")
	if part == "sheets" {
		return "documents"
	}
	return part
}

func (p *PersonalConnections) Call(ctx context.Context, owner string, in PersonalOperation, guard func(context.Context) error) (Result, error) {
	if err := in.Validate(); err != nil {
		return Result{}, err
	}
	if guard == nil {
		return Result{}, ErrInvalid
	}
	if err := guard(ctx); err != nil {
		return Result{}, err
	}
	read := PersonalRead(in.Operation)
	if !read && ctx.Value(submissionKey{}) != true {
		return Result{}, Rejected("personal writes require durable submission")
	}
	started := time.Now()
	c, err := p.token(ctx, owner, in.Provider, in.Generation)
	if err != nil {
		return Result{}, err
	}
	check := func(ctx context.Context) error {
		if err := guard(ctx); err != nil {
			return err
		}
		return p.Check(owner, in.Provider, in.Generation)
	}
	ctx = WithAuthority(ctx, check)
	var data any
	if in.Provider == "google" && strings.HasPrefix(in.Operation, "email.") {
		if err = p.scope(owner, "google", in.Generation, "https://www.googleapis.com/auth/gmail.modify"); err != nil {
			return Result{}, err
		}
		return p.email(ctx, owner, c, in, check)
	}
	if in.Provider == "google" {
		data, err = p.googlePersonal(ctx, c, in, check)
	} else if in.Provider == "slack" {
		if in.Operation == "slack.thread" {
			data, err = p.slackThread(ctx, c, in, check)
		} else {
			data, err = p.slackPersonal(ctx, c, in, check)
		}
	} else {
		err = ErrInvalid
	}
	if err != nil {
		return Result{}, err
	}
	if err = check(ctx); err != nil {
		return Result{}, err
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return Result{}, err
	}
	for _, secret := range []string{c.AccessToken, c.RefreshToken} {
		if secret != "" {
			escaped, _ := json.Marshal(secret)
			raw = bytes.ReplaceAll(raw, escaped[1:len(escaped)-1], []byte("[redacted]"))
		}
	}
	if json.Unmarshal(raw, &data) != nil {
		return Result{}, ErrInvalid
	}
	receipt := Receipt{Connection: "personal-" + in.Provider, Operation: in.Operation, InputHash: hash(in), OutputHash: hash(data), Time: started.UTC().Format(time.RFC3339Nano), DurationMS: time.Since(started).Milliseconds(), Complete: true, Verification: "executed"}
	receipt.ID = hash(receipt)
	return Result{Data: data, Receipt: receipt}, nil
}

func (p *PersonalConnections) email(ctx context.Context, owner string, c personalCredential, in PersonalOperation, guard func(context.Context) error) (Result, error) {
	operation := strings.TrimPrefix(in.Operation, "email.")
	if !PersonalRead(in.Operation) && !slices.Contains([]string{"draft-create", "draft-update", "draft-send", "send"}, operation) {
		return Result{}, ErrInvalid
	}
	recipients := []string{}
	for _, v := range array(in.Arguments["to"]) {
		recipients = append(recipients, str(v))
	}
	// User-directed recipients are checked by the submission owner. Replies are
	// additionally checked against the actual source sender in gmailWrite.
	if operation == "draft-send" {
		draft, err := p.email(ctx, owner, c, PersonalOperation{Provider: "google", Generation: in.Generation, Operation: "email.draft", Arguments: map[string]any{"id": in.Arguments["id"]}}, guard)
		if err != nil {
			return Result{}, err
		}
		message := object(object(draft.Data)["message"])
		for _, v := range array(message["recipients"]) {
			recipients = append(recipients, str(v))
		}
	}
	endpoint := "https://gmail.googleapis.com"
	if p.googleAPI != "https://www.googleapis.com" {
		endpoint = p.googleAPI
	}
	config := Connection{ID: "personal-email", Kind: "email", Endpoint: endpoint, Email: &EmailConfig{Provider: "gmail", Mailbox: c.Email, Workflow: "personal-" + owner, Recipients: recipients}, Operations: []Operation{{Name: operation, ReadOnly: PersonalRead(in.Operation)}}, personalToken: func(ctx context.Context) (string, []string, error) {
		v, err := p.token(ctx, owner, "google", in.Generation)
		return v.AccessToken, []string{v.AccessToken, v.RefreshToken}, err
	}}
	manager, err := New(Config{Version: 1, Connections: []Connection{config}})
	if err != nil {
		return Result{}, err
	}
	defer manager.Close()
	args, _ := json.Marshal(in.Arguments)
	result, err := manager.Call(WithAuthority(ctx, guard), "personal-email", operation, args, "")
	if err == nil {
		err = guard(ctx)
	}
	return result, err
}

func checkedPersonalID(value any) (string, error) {
	id := str(value)
	if id == "" || id == "." || id == ".." || len(id) > 1024 || strings.ContainsAny(id, "\x00\r\n/\\") {
		return "", ErrInvalid
	}
	return url.PathEscape(id), nil
}

func (p *PersonalConnections) googlePersonal(ctx context.Context, c personalCredential, in PersonalOperation, guard func(context.Context) error) (any, error) {
	a := in.Arguments
	query := url.Values{}
	base, path := p.googleAPI, ""
	scope := ""
	switch in.Operation {
	case "calendar.list":
		path = "/calendar/v3/users/me/calendarList"
		scope = "https://www.googleapis.com/auth/calendar.calendarlist.readonly"
	case "calendar.events", "calendar.event", "calendar.create", "calendar.update":
		scope = "https://www.googleapis.com/auth/calendar.events"
		calendar := a["calendar"]
		if calendar == nil || calendar == "" {
			calendar = "primary"
		}
		id, err := checkedPersonalID(calendar)
		if err != nil {
			return nil, err
		}
		path = "/calendar/v3/calendars/" + id + "/events"
		if in.Operation == "calendar.event" || in.Operation == "calendar.update" {
			event, err := checkedPersonalID(a["id"])
			if err != nil {
				return nil, err
			}
			path += "/" + event
		}
		if in.Operation == "calendar.events" {
			if cursor := str(a["syncToken"]); cursor != "" {
				query.Set("syncToken", cursor)
			} else {
				for _, name := range []string{"timeMin", "timeMax"} {
					if value := str(a[name]); value != "" {
						if _, err := time.Parse(time.RFC3339, value); err != nil {
							return nil, ErrInvalid
						}
						query.Set(name, value)
					}
				}
				query.Set("singleEvents", "true")
				query.Set("orderBy", "startTime")
			}
			query.Set("showDeleted", "true")
		}
	case "documents.search":
		scope = "https://www.googleapis.com/auth/drive.readonly"
		path = "/drive/v3/files"
		query.Set("fields", "nextPageToken,files(id,name,mimeType,modifiedTime,webViewLink,description)")
		query.Set("q", "trashed = false")
		if search := str(a["query"]); search != "" {
			search = strings.ReplaceAll(strings.ReplaceAll(search, "\\", "\\\\"), "'", "\\'")
			query.Set("q", "trashed = false and fullText contains '"+search+"'")
		}
	case "documents.read":
		scope = "https://www.googleapis.com/auth/documents"
		id, err := checkedPersonalID(a["id"])
		if err != nil {
			return nil, err
		}
		path = "/v1/documents/" + id
		if base == "https://www.googleapis.com" {
			base = "https://docs.googleapis.com"
		}
	case "sheets.read":
		scope = "https://www.googleapis.com/auth/spreadsheets"
		id, err := checkedPersonalID(a["id"])
		if err != nil {
			return nil, err
		}
		path = "/v4/spreadsheets/" + id + "/values:batchGet"
		for _, r := range array(a["ranges"]) {
			if str(r) == "" {
				return nil, ErrInvalid
			}
			query.Add("ranges", str(r))
		}
		if len(query["ranges"]) == 0 {
			return nil, ErrInvalid
		}
		if base == "https://www.googleapis.com" {
			base = "https://sheets.googleapis.com"
		}
	default:
		return nil, ErrInvalid
	}
	if err := p.scope(c.Owner, c.Provider, c.Generation, scope); err != nil {
		return nil, err
	}
	if page := str(a["pageToken"]); page != "" {
		query.Set("pageToken", page)
	}
	if in.Operation == "calendar.create" || in.Operation == "calendar.update" {
		body, ok := a["event"].(map[string]any)
		if !ok {
			return nil, Rejected("provide an event object")
		}
		body = cloneObject(body)
		method := "POST"
		if in.Operation == "calendar.create" {
			action := str(a["actionID"])
			if action == "" {
				return nil, Rejected("stable actionID required")
			}
			body["id"] = hash([]string{c.Owner, action})
		} else {
			method = "PATCH"
			if str(a["etag"]) == "" {
				return nil, Rejected("current event etag required")
			}
		}
		query.Set("sendUpdates", "none")
		raw, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, method, base+path+"?"+query.Encode(), bytes.NewReader(raw))
		if err != nil {
			return nil, Rejected("invalid event request")
		}
		req.Header.Set("Authorization", "Bearer "+c.AccessToken)
		req.Header.Set("Content-Type", "application/json")
		if method == "PATCH" {
			req.Header.Set("If-Match", str(a["etag"]))
		}
		if err = guard(ctx); err != nil {
			return nil, Rejected(err.Error())
		}
		result, err := p.response(req)
		if err != nil {
			return nil, Unknown(err.Error())
		}
		return result, nil
	}
	if err := guard(ctx); err != nil {
		return nil, err
	}
	return p.requestJSON(ctx, "GET", base+path+"?"+query.Encode(), c.AccessToken, nil)
}

func cloneObject(in map[string]any) map[string]any {
	raw, _ := json.Marshal(in)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func (p *PersonalConnections) slackPersonal(ctx context.Context, c personalCredential, in PersonalOperation, guard func(context.Context) error) (any, error) {
	args := in.Arguments
	q := url.Values{}
	method := ""
	scopes := []string{}
	switch in.Operation {
	case "slack.conversations":
		method = "users.conversations"
		q.Set("types", "im,mpim")
		scopes = []string{"im:read", "mpim:read"}
	case "slack.history", "slack.thread":
		method = "conversations.history"
		if in.Operation == "slack.thread" {
			method = "conversations.replies"
			q.Set("ts", str(args["ts"]))
		}
		if !identifier.MatchString(str(args["channel"])) {
			return nil, ErrInvalid
		}
		q.Set("channel", str(args["channel"]))
		if oldest := str(args["oldest"]); oldest != "" {
			q.Set("oldest", oldest)
		}
		scopes = []string{"im:history", "mpim:history", "channels:history", "groups:history"}
	case "slack.permalink":
		method = "chat.getPermalink"
		q.Set("conversation_id", str(args["channel"]))
		q.Set("channel", str(args["channel"]))
		q.Set("message_ts", str(args["ts"]))
		scopes = []string{"im:history", "mpim:history", "channels:history", "groups:history"}
	case "slack.send":
		if !identifier.MatchString(str(args["channel"])) || str(args["text"]) == "" || str(args["actionID"]) == "" {
			return nil, Rejected("channel, text and actionID required")
		}
		if err := p.scope(c.Owner, c.Provider, c.Generation, "chat:write"); err != nil {
			return nil, err
		}
		// A fresh history fingerprint prevents sending a stale suggested reply.
		if str(args["expectedRevision"]) == "" {
			return nil, Rejected("current conversation revision required")
		}
		read := PersonalOperation{Provider: "slack", Generation: c.Generation, Operation: "slack.history", Arguments: map[string]any{"channel": args["channel"]}}
		var history any
		var err error
		if thread := str(args["thread_ts"]); thread != "" {
			read.Operation = "slack.thread"
			read.Arguments["ts"] = thread
			history, err = p.slackThread(ctx, c, read, guard)
		} else {
			history, err = p.slackPersonal(ctx, c, read, guard)
		}
		if err != nil {
			return nil, Rejected(err.Error())
		}
		if str(object(history)["revision"]) != str(args["expectedRevision"]) {
			return nil, Rejected("conversation changed; refresh the draft")
		}
		body := map[string]any{"channel": args["channel"], "text": args["text"], "unfurl_links": false, "unfurl_media": false}
		if thread := str(args["thread_ts"]); thread != "" {
			body["thread_ts"] = thread
		}
		raw, _ := json.Marshal(body)
		if err = guard(ctx); err != nil {
			return nil, Rejected(err.Error())
		}
		result, err := p.requestJSON(ctx, "POST", p.slackAPI+"/chat.postMessage", c.AccessToken, bytes.NewReader(raw))
		if err != nil {
			return nil, Unknown(err.Error())
		}
		if result["ok"] != true {
			return nil, Unknown("Slack did not acknowledge the message")
		}
		return result, nil
	default:
		return nil, ErrInvalid
	}
	if err := p.scope(c.Owner, c.Provider, c.Generation, scopes...); err != nil {
		return nil, err
	}
	if cursor := str(args["cursor"]); cursor != "" {
		q.Set("cursor", cursor)
	}
	q.Set("limit", "100")
	if err := guard(ctx); err != nil {
		return nil, err
	}
	result, err := p.requestJSON(ctx, "GET", p.slackAPI+"/"+method+"?"+q.Encode(), c.AccessToken, nil)
	if err != nil {
		return nil, err
	}
	if result["ok"] != true {
		return nil, errors.New("Slack request was not acknowledged")
	}
	if in.Operation == "slack.history" || in.Operation == "slack.thread" {
		result["revision"] = hash(result["messages"])
	}
	return result, nil
}

// A reply precondition covers the entire thread, including replies on later
// pages. Partial pages must never authorize a stale external send.
func (p *PersonalConnections) slackThread(ctx context.Context, c personalCredential, in PersonalOperation, guard func(context.Context) error) (map[string]any, error) {
	all := []any{}
	seen := map[string]bool{}
	in.Arguments = cloneObject(in.Arguments)
	delete(in.Arguments, "cursor")
	for {
		value, err := p.slackPersonal(ctx, c, in, guard)
		if err != nil {
			return nil, err
		}
		data := object(value)
		messages, ok := data["messages"].([]any)
		if !ok {
			return nil, errors.New("Slack omitted thread messages")
		}
		all = append(all, messages...)
		raw, _ := json.Marshal(all)
		if len(raw) > capacity.StateBytes.Limit()/2 {
			return nil, capacity.StateBytes.Error()
		}
		cursor := str(object(data["response_metadata"])["next_cursor"])
		if cursor == "" {
			if data["has_more"] == true {
				return nil, errors.New("Slack thread truncated without continuation")
			}
			data["messages"] = all
			data["revision"] = hash(all)
			return data, nil
		}
		if seen[cursor] {
			return nil, errors.New("Slack repeated a continuation")
		}
		seen[cursor] = true
		in.Arguments["cursor"] = cursor
	}
}

func PersonalOperationNames() []string {
	return strings.Fields("email.status email.messages email.message email.thread email.attachment email.changes email.draft email.draft-create email.draft-update email.draft-send email.send calendar.list calendar.events calendar.event calendar.create calendar.update documents.search documents.read sheets.read slack.conversations slack.history slack.thread slack.permalink slack.send")
}

func (in PersonalOperation) Validate() error {
	if in.Generation == 0 || in.Arguments == nil || !slices.Contains(PersonalOperationNames(), in.Operation) || (in.Provider != "google" && in.Provider != "slack") || (in.Provider == "slack") != strings.HasPrefix(in.Operation, "slack.") {
		return fmt.Errorf("personal operation requires a connection generation and arguments")
	}
	return nil
}
