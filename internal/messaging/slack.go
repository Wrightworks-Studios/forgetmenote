//go:build !fmn_omit_slack

package messaging

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"forgetmenote/internal/capacity"
	"forgetmenote/internal/workspace"
)

// Slack is the Slack Web API transport for one workspace.
type Slack struct {
	Team string
	// Endpoint is the chat.postEphemeral URL; other method URLs derive from it.
	// Only tests point it at a local server.
	Endpoint string
	Client   *http.Client
	// Allow reports whether the instance may call Slack now. Every request
	// asks it first, so a refusal sends nothing.
	Allow func() error
	// Token reads the bot token for each request.
	Token func() (string, error)
	// Extract converts an attachment shared with the destination into text and
	// an extraction warning.
	Extract func(ctx context.Context, name, url string, data []byte) (text, warning string, err error)
}

// memberAnswerTTL is how long one invocation reuses a member's
// verification, and verifiedLimit the most answers it keeps.
const (
	memberAnswerTTL = 30 * time.Second
	verifiedLimit   = 1024
)

type verifiedKey struct{}

// verifiedMembers remembers whether Slack confirmed or refused each member,
// by workspace and member, for memberAnswerTTL.
type verifiedMembers struct {
	mu      sync.Mutex
	answers map[string]verifiedAnswer
	// now is the clock, which only tests change.
	now func() time.Time
}

type verifiedAnswer struct {
	err   error
	until time.Time
}

// RememberMembers returns a context in which VerifyMember reuses each
// member's answer for memberAnswerTTL, so the several checks one answer makes
// ask Slack once. Each request or run starts its own, so a member Slack has
// deactivated is refused at their next message.
func RememberMembers(ctx context.Context) context.Context {
	if _, ok := ctx.Value(verifiedKey{}).(*verifiedMembers); ok {
		return ctx
	}
	return context.WithValue(ctx, verifiedKey{}, &verifiedMembers{})
}

func (v *verifiedMembers) clock() time.Time {
	if v.now != nil {
		return v.now()
	}
	return time.Now()
}

func (v *verifiedMembers) get(key string) (verifiedAnswer, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	a, ok := v.answers[key]
	return a, ok && v.clock().Before(a.until)
}

func (v *verifiedMembers) put(key string, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.answers == nil || len(v.answers) >= verifiedLimit {
		v.answers = map[string]verifiedAnswer{}
	}
	v.answers[key] = verifiedAnswer{err: err, until: v.clock().Add(memberAnswerTTL)}
}

// VerifyMember checks that d.User is an active, human, non-guest member of the
// workspace. Within a context from RememberMembers, an answer is reused: a
// refusal as well as a confirmation, never a failure to ask.
func (s *Slack) VerifyMember(ctx context.Context, d Address) error {
	if d.Workspace != s.Team || !ValidID(d.User) {
		return workspace.ErrForbidden
	}
	if err := s.allowed(); err != nil {
		return err
	}
	verified, remembers := ctx.Value(verifiedKey{}).(*verifiedMembers)
	key := s.Team + "/" + d.User
	if remembers {
		if known, ok := verified.get(key); ok {
			return known.err
		}
	}
	err := s.verifyMember(ctx, d.User)
	if remembers && (err == nil || errors.Is(err, workspace.ErrForbidden)) {
		verified.put(key, err)
	}
	return err
}

func (s *Slack) verifyMember(ctx context.Context, user string) error {
	var response struct {
		User struct {
			ID       string `json:"id"`
			Team     string `json:"team_id"`
			Deleted  bool   `json:"deleted"`
			Bot      bool   `json:"is_bot"`
			App      bool   `json:"is_app_user"`
			Stranger bool   `json:"is_stranger"`
			// Multi-channel guests are restricted; single-channel guests are
			// also ultra-restricted. Both share the workspace's team ID.
			Guest              bool `json:"is_restricted"`
			SingleChannelGuest bool `json:"is_ultra_restricted"`
		} `json:"user"`
	}
	if err := s.api(ctx, "users.info", url.Values{"user": {user}}, &response); err != nil {
		return err
	}
	u := response.User
	if u.ID != user || u.Team != s.Team || u.Deleted || u.Bot || u.App || u.Stranger || u.Guest || u.SingleChannelGuest {
		return workspace.ErrForbidden
	}
	return nil
}

// Members lists the workspace's active human members who are not guests, with
// the address Slack holds for each. Slack returns addresses only to apps with
// the users:read.email scope, and an address Slack marks unconfirmed is left out.
func (s *Slack) Members(ctx context.Context) ([]Member, error) {
	out := []Member{}
	cursor := ""
	for range 100 {
		var response struct {
			Members  []slackUser `json:"members"`
			Metadata struct {
				Next string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		query := url.Values{"limit": {"200"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		if err := s.api(ctx, "users.list", query, &response); err != nil {
			return nil, err
		}
		for _, u := range response.Members {
			if m, ok := u.member(s.Team); ok {
				out = append(out, m)
			}
		}
		if cursor = response.Metadata.Next; cursor == "" {
			return out, nil
		}
	}
	return nil, errors.New("Slack member list exceeds 100 pages")
}

// ChannelMembers lists a channel's members. A private channel is visible only
// once the app has been added to it.
func (s *Slack) ChannelMembers(ctx context.Context, channel string) ([]string, error) {
	if !ValidID(channel) || !strings.HasPrefix(channel, "C") && !strings.HasPrefix(channel, "G") {
		return nil, ErrAudienceHidden
	}
	out := []string{}
	cursor := ""
	for range 25 {
		var response struct {
			Members  []string `json:"members"`
			Metadata struct {
				Next string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		query := url.Values{"channel": {channel}, "limit": {"200"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		if err := s.api(ctx, "conversations.members", query, &response); err != nil {
			return nil, audienceError(err)
		}
		out = append(out, response.Members...)
		if cursor = response.Metadata.Next; cursor == "" {
			return out, nil
		}
	}
	return nil, errors.New("Slack channel has more than 5,000 members")
}

// Permalink returns the link to a message, which opens its thread for those
// who can see the channel.
func (s *Slack) Permalink(ctx context.Context, channel, ts string) (string, error) {
	if !ValidID(channel) || ts == "" || len(ts) > 32 {
		return "", ErrAudienceHidden
	}
	var response struct {
		Permalink string `json:"permalink"`
	}
	if err := s.api(ctx, "chat.getPermalink", url.Values{"channel": {channel}, "message_ts": {ts}}, &response); err != nil {
		return "", audienceError(err)
	}
	return response.Permalink, nil
}

// GroupMembers lists the members of a user group.
func (s *Slack) GroupMembers(ctx context.Context, group string) ([]string, error) {
	if !ValidID(group) || !strings.HasPrefix(group, "S") {
		return nil, ErrAudienceHidden
	}
	var response struct {
		Users []string `json:"users"`
	}
	if err := s.api(ctx, "usergroups.users.list", url.Values{"usergroup": {group}}, &response); err != nil {
		return nil, audienceError(err)
	}
	return response.Users, nil
}

// audienceError names the refusals a member of the workspace can act on.
func audienceError(err error) error {
	switch {
	case slackRefused(err, "channel_not_found", "no_such_subteam", "subteam_not_found", "not_in_channel"):
		return fmt.Errorf("%w: %v", ErrAudienceHidden, err)
	case slackRefused(err, "missing_scope", "not_allowed_token_type"):
		return fmt.Errorf("%w: %v", ErrAudienceScope, err)
	}
	return err
}

type slackUser struct {
	ID                 string `json:"id"`
	Team               string `json:"team_id"`
	Deleted            bool   `json:"deleted"`
	Bot                bool   `json:"is_bot"`
	App                bool   `json:"is_app_user"`
	Stranger           bool   `json:"is_stranger"`
	Guest              bool   `json:"is_restricted"`
	SingleChannelGuest bool   `json:"is_ultra_restricted"`
	Confirmed          *bool  `json:"is_email_confirmed"`
	TimeZone           string `json:"tz"`
	Profile            struct {
		Email            string `json:"email"`
		DisplayName      string `json:"display_name"`
		RealName         string `json:"real_name"`
		StatusText       string `json:"status_text"`
		StatusEmoji      string `json:"status_emoji"`
		StatusExpiration int64  `json:"status_expiration"`
	} `json:"profile"`
}

// member reports whether u is an active, human, non-guest member of team.
func (u slackUser) member(team string) (Member, bool) {
	// Slackbot is reported as a human user.
	if !ValidID(u.ID) || u.ID == "USLACKBOT" || u.Team != team || u.Deleted || u.Bot || u.App || u.Stranger || u.Guest || u.SingleChannelGuest {
		return Member{User: u.ID}, false
	}
	m := Member{User: u.ID, TimeZone: u.TimeZone, Name: cmp.Or(strings.TrimSpace(u.Profile.DisplayName), strings.TrimSpace(u.Profile.RealName))}
	if u.Confirmed == nil || *u.Confirmed {
		m.Email = u.Profile.Email
	}
	// Slack clears an expired status shortly after it expires.
	if p := u.Profile; p.StatusExpiration == 0 || time.Unix(p.StatusExpiration, 0).After(time.Now()) {
		m.Status, m.StatusEmoji = p.StatusText, p.StatusEmoji
	}
	return m, true
}

// EventMember reads the user object of a team_join or user_change event. The
// bool reports whether it is an active, human, non-guest member, as Members
// would list it.
func (s *Slack) EventMember(raw json.RawMessage) (Member, bool, error) {
	var u slackUser
	if err := json.Unmarshal(raw, &u); err != nil || !ValidID(u.ID) {
		return Member{}, false, errors.New("invalid Slack user")
	}
	m, active := u.member(s.Team)
	return m, active, nil
}

func (s *Slack) Status(ctx context.Context, d Address, status string) error {
	if d.Thread != "" && (d.Direct || d.Shared) {
		return s.api(ctx, "assistant.threads.setStatus", map[string]string{"channel_id": d.Channel, "thread_ts": d.Thread, "status": status}, nil)
	}
	// Slack's native status opens a thread. Keep flat DMs and private ephemeral
	// replies in their original conversation; a reaction supplies visible progress.
	if d.Message == "" {
		return nil
	}
	method := "reactions.add"
	if status == "" {
		method = "reactions.remove"
	}
	err := s.api(ctx, method, map[string]string{"channel": d.Channel, "timestamp": d.Message, "name": "eyes"}, nil)
	if slackRefused(err, "already_reacted") || status == "" && slackRefused(err, "no_reaction") {
		return nil
	}
	return err
}

func (s *Slack) apiURL(method string) string {
	u, _ := url.Parse(s.Endpoint)
	if strings.HasSuffix(u.Path, "chat.postEphemeral") {
		u.Path = strings.TrimSuffix(u.Path, "chat.postEphemeral") + method
	} else if method != "chat.postEphemeral" {
		u.Path = strings.TrimRight(u.Path, "/") + "/" + method
	}
	return u.String()
}

// slackError is a failure the Slack Web API answered one method call with.
type slackError struct {
	Method string
	// Status is the HTTP status, and Code Slack's error code, or
	// request_failed when it gave none.
	Status int
	Code   string
	// Reasons are Slack's explanations of a refusal such as invalid_blocks.
	Reasons string
	// RetryAfter is the wait in seconds Slack asked for when it rate limited
	// the call.
	RetryAfter int
}

func (e *slackError) Error() string {
	if e.Status == http.StatusTooManyRequests {
		return fmt.Sprintf("Slack %s rate limited (HTTP 429, Retry-After %d seconds); request was not retried", e.Method, e.RetryAfter)
	}
	code := e.Code
	if e.Reasons != "" {
		code += " (" + e.Reasons + ")"
	}
	return fmt.Sprintf("Slack %s failed: HTTP %d, %s", e.Method, e.Status, code)
}

// slackRefused reports whether err is Slack's answer with one of the codes.
func slackRefused(err error, codes ...string) bool {
	var e *slackError
	return errors.As(err, &e) && slices.Contains(codes, e.Code)
}

// allowed asks Allow whether the instance may call Slack now.
func (s *Slack) allowed() error {
	if s.Allow == nil {
		return nil
	}
	if err := s.Allow(); err != nil {
		return notPosted{err}
	}
	return nil
}

func (s *Slack) api(ctx context.Context, method string, fields any, out any) error {
	if err := s.allowed(); err != nil {
		return err
	}
	// A request whose context has already ended is never sent.
	if err := ctx.Err(); err != nil {
		return notPosted{err}
	}
	token, err := s.Token()
	if err != nil {
		return notPosted{err}
	}
	endpoint := s.apiURL(method)
	var req *http.Request
	contentType := "application/json"
	if values, ok := fields.(url.Values); ok {
		if method == "files.getUploadURLExternal" {
			// Slack's upload admission rejects JSON fields as missing arguments.
			contentType = "application/x-www-form-urlencoded"
			req, err = http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(values.Encode()))
		} else {
			req, err = http.NewRequestWithContext(ctx, "GET", endpoint+"?"+values.Encode(), nil)
		}
	} else {
		raw, _ := json.Marshal(fields)
		req, err = http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(raw))
	}
	if err != nil {
		return notPosted{err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	resp, err := s.Client.Do(req)
	if err != nil {
		return errors.New("Slack request outcome is unknown")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return errors.New("Slack response could not be read")
	}
	var status struct {
		OK       bool     `json:"ok"`
		Error    string   `json:"error"`
		Errors   []string `json:"errors"`
		Metadata struct {
			Messages []string `json:"messages"`
		} `json:"response_metadata"`
	}
	if json.Unmarshal(raw, &status) != nil {
		return fmt.Errorf("Slack %s returned HTTP %d with an invalid response", method, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || !status.OK {
		refusal := &slackError{Method: method, Status: resp.StatusCode, Code: "request_failed"}
		if ValidID(status.Error) {
			refusal.Code = status.Error
		}
		// Slack explains a refusal such as invalid_blocks only in these lists.
		reasons := status.Metadata.Messages
		if len(reasons) == 0 {
			reasons = status.Errors
		}
		refusal.Reasons = slackReasons(reasons)
		if resp.StatusCode == http.StatusTooManyRequests {
			refusal.RetryAfter, _ = strconv.Atoi(resp.Header.Get("Retry-After"))
			return notPosted{refusal}
		}
		// Slack reports refusals as HTTP 200 with ok:false. Other statuses come
		// from infrastructure in front of the API and leave the outcome unknown.
		if resp.StatusCode == http.StatusOK {
			return notPosted{refusal}
		}
		return refusal
	}
	if out != nil && json.Unmarshal(raw, out) != nil {
		return errors.New("invalid Slack response")
	}
	return nil
}

// slackReasons keeps the first few of Slack's explanations, each on one short
// line, so an error stays readable in run records and logs.
func slackReasons(reasons []string) string {
	kept := []string{}
	for _, reason := range reasons {
		reason = strings.Join(strings.FieldsFunc(reason, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
		if reason == "" {
			continue
		}
		if chars := []rune(reason); len(chars) > 300 {
			reason = string(chars[:300]) + "…"
		}
		if kept = append(kept, reason); len(kept) == 3 {
			break
		}
	}
	return strings.Join(kept, "; ")
}

// unprocessedUpload reports Slack's refusal of a slack_file it cannot use yet,
// which Slack names only in its explanation of invalid_blocks.
func unprocessedUpload(err error) bool {
	var e *slackError
	return errors.Is(err, ErrNotPosted) && errors.As(err, &e) && e.Code == "invalid_blocks" && strings.Contains(e.Reasons, "invalid file type") && strings.Contains(e.Reasons, "slack_file")
}

func (s *Slack) Context(ctx context.Context, d Address, b Binding) (Context, error) {
	var result Context
	if b.ThreadHistory && d.Thread != "" && d.Thread != d.Message {
		cursor := ""
		seen := map[string]bool{}
		bytes := 0
		for {
			var history struct {
				Messages []struct {
					TS, User, Text string
					BotID          string `json:"bot_id"`
				}
				More     bool `json:"has_more"`
				Metadata struct {
					Next string `json:"next_cursor"`
				} `json:"response_metadata"`
			}
			values := url.Values{"channel": {d.Channel}, "ts": {d.Thread}, "latest": {d.Message}, "inclusive": {"false"}, "limit": {"100"}}
			if cursor != "" {
				values.Set("cursor", cursor)
			}
			if err := s.api(ctx, "conversations.replies", values, &history); err != nil {
				return result, err
			}
			for _, m := range history.Messages {
				if m.BotID != "" || m.TS == d.Message {
					continue
				}
				bytes += len(m.Text)
				if bytes > capacity.StateBytes.Limit() {
					return result, capacity.StateBytes.Error()
				}
				result.Messages = append(result.Messages, ContextMessage{ID: m.TS, User: m.User, Text: m.Text})
			}
			if !history.More && history.Metadata.Next == "" {
				break
			}
			cursor = history.Metadata.Next
			if cursor == "" || seen[cursor] {
				return result, errors.New("Slack returned incomplete thread pagination")
			}
			seen[cursor] = true
		}
	}
	if len(d.Files) > 0 && !b.Attachments {
		return result, errors.New("files are disabled for this destination")
	}
	total := 0
	for _, ref := range d.Files {
		var response struct {
			File struct {
				ID, Name, User, Permalink string
				URL                       string `json:"url_private_download"`
				Size                      int
				External                  bool `json:"is_external"`
				Channels, Groups, IMs     []string
				Shares                    map[string]map[string]json.RawMessage
			}
		}
		if err := s.api(ctx, "files.info", url.Values{"file": {ref.ID}}, &response); err != nil {
			return result, err
		}
		f := response.File
		shared := slices.Contains(f.Channels, d.Channel) || slices.Contains(f.Groups, d.Channel) || slices.Contains(f.IMs, d.Channel)
		for _, channels := range f.Shares {
			if _, ok := channels[d.Channel]; ok {
				shared = true
			}
		}
		if f.ID != ref.ID || f.External || !shared || f.Size <= 0 || f.Size > workspace.MaxSourceBytes() {
			return result, errors.New("attachment is unavailable in this conversation or exceeds FMN_LIMIT_SOURCE_BYTES")
		}
		data, err := s.download(ctx, f.URL, true, f.Size)
		if err != nil {
			return result, err
		}
		total += len(data)
		if total > workspace.MaxSourceBytes() {
			return result, capacity.SourceBytes.Error()
		}
		if !workspace.SourceURL(f.Permalink) {
			return result, errors.New("attachment source link is invalid")
		}
		text, warning, err := s.Extract(ctx, filepath.Base(f.Name), f.Permalink, data)
		if err != nil {
			return result, err
		}
		result.Documents = append(result.Documents, Document{Name: filepath.Base(f.Name), URL: f.Permalink, Text: text, Hash: workspace.ContentHash(data), Warning: warning})
	}
	return result, nil
}

// Private-file credentials are sent only to Slack's file service, never to an
// event-supplied URL or a redirect. The API origin override exists only in tests.
func (s *Slack) fileURL(raw string) bool {
	return s.trustedURL(raw, "files.slack.com", "files-origin.slack.com")
}
func (s *Slack) trustedURL(raw string, hosts ...string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" && u.Port() == "" && slices.Contains(hosts, u.Hostname()) {
		return true
	}
	api, _ := url.Parse(s.Endpoint)
	return api.Scheme == "http" && (api.Hostname() == "127.0.0.1" || api.Hostname() == "localhost") && u.Scheme == api.Scheme && u.Host == api.Host
}
func (s *Slack) download(ctx context.Context, raw string, auth bool, size int) ([]byte, error) {
	if !s.fileURL(raw) {
		return nil, errors.New("untrusted Slack file URL")
	}
	if err := s.allowed(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return nil, err
	}
	if auth {
		token, err := s.Token()
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, errors.New("Slack file download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Slack file download returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(size)+1))
	if err != nil || len(data) != size {
		return nil, errors.New("Slack file size changed or download was incomplete")
	}
	return data, nil
}

// upload sends one file's bytes to Slack and returns its ID. Nobody can see
// the file until files.completeUploadExternal shares it or keeps it private.
func (s *Slack) upload(ctx context.Context, name string, data []byte) (string, error) {
	var target struct {
		URL string `json:"upload_url"`
		ID  string `json:"file_id"`
	}
	if err := s.api(ctx, "files.getUploadURLExternal", url.Values{"filename": {name}, "length": {strconv.Itoa(len(data))}}, &target); err != nil {
		return "", err
	}
	if target.ID == "" || !s.fileURL(target.URL) {
		return "", errors.New("invalid Slack upload target")
	}
	if err := s.allowed(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", target.URL, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := s.Client.Do(req)
	if err != nil {
		return "", errors.New("Slack file upload outcome is unknown")
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("Slack file upload returned HTTP %d", resp.StatusCode)
	}
	return target.ID, nil
}

// Send posts a reply as one or more messages. An error matching ErrNotPosted
// sent nothing; once one message has reached the conversation, any later
// failure leaves the outcome unknown, since sending again would repeat it.
func (s *Slack) Send(ctx context.Context, d Address, reply Reply) (err error) {
	if len(reply.Artifacts) > 0 && !d.Direct && !d.Shared {
		return errors.New("file replies require a DM or an approved shared channel")
	}
	posted := false
	defer func() {
		if err != nil && posted {
			err = partlyPosted(err)
		}
	}()
	text := escapeMentions(reply.Text)
	parts := messageParts(text, messageLimit)
	if len(reply.Artifacts) > 0 {
		files := []map[string]string{}
		for _, a := range reply.Artifacts {
			id, err := s.upload(ctx, a.Name, a.Data)
			if err != nil {
				return err
			}
			files = append(files, map[string]string{"id": id, "title": a.Name})
		}
		fields := map[string]any{"files": files, "channel_id": d.Channel}
		if d.Thread != "" {
			fields["thread_ts"] = d.Thread
		}
		if len(parts) > 0 {
			fields["initial_comment"] = parts[0]
			parts = parts[1:]
		}
		if err := s.api(ctx, "files.completeUploadExternal", fields, nil); err != nil {
			return err
		}
		posted = true
	}
	for n, text := range parts {
		blocks := []map[string]any{{"type": "markdown", "text": text}}
		if n == len(parts)-1 && d.Direct {
			blocks = append(blocks, buttonBlocks(reply.Sections, reply.Buttons)...)
		}
		fields := map[string]any{"channel": d.Channel, "text": text, "blocks": blocks, "unfurl_links": false, "unfurl_media": false, "parse": "none"}
		if d.Thread != "" {
			fields["thread_ts"] = d.Thread
		}
		method := "chat.postMessage"
		if !d.Direct && !d.Shared {
			method = "chat.postEphemeral"
			fields["user"] = d.User
		}
		if err := s.api(ctx, method, fields, nil); err != nil {
			return err
		}
		posted = true
	}
	// Buttons go only to the requester, the one person who can confirm: as a
	// private message outside a DM. Each proposal gets its own message, so a
	// click replaces just its buttons.
	for _, c := range reply.Confirmations {
		value, _ := json.Marshal(ButtonValue{Code: c.Code, Thread: d.Thread})
		button := func(action, label, style string) map[string]any {
			b := map[string]any{"type": "button", "action_id": action, "text": map[string]string{"type": "plain_text", "text": label}, "value": string(value)}
			if style != "" {
				b["style"] = style
			}
			return b
		}
		text := escapeMentions("Run " + c.Label + "?")
		fields := map[string]any{"channel": d.Channel, "user": d.User, "text": text, "unfurl_links": false, "unfurl_media": false, "blocks": []map[string]any{
			{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": text}},
			{"type": "actions", "block_id": "write-" + c.Code, "elements": []map[string]any{button(ConfirmAction, "Confirm", "primary"), button(CancelAction, "Cancel", "")}},
		}}
		if d.Thread != "" {
			fields["thread_ts"] = d.Thread
		}
		method := "chat.postEphemeral"
		if d.Direct {
			method = "chat.postMessage"
			delete(fields, "user")
		}
		if err := s.api(ctx, method, fields, nil); err != nil {
			return err
		}
		posted = true
	}
	return nil
}

// partlyPosted reports a failure after part of a reply reached the
// conversation, which no longer matches ErrNotPosted.
func partlyPosted(err error) error {
	var refused notPosted
	for errors.As(err, &refused) {
		err = refused.error
	}
	return fmt.Errorf("Slack reply was only partly posted: %w", err)
}

// OpenView opens a modal for the user whose interaction carried trigger.
func (s *Slack) OpenView(ctx context.Context, trigger string, view any) error {
	if trigger == "" || len(trigger) > 256 {
		return errors.New("invalid Slack trigger")
	}
	return s.api(ctx, "views.open", map[string]any{"trigger_id": trigger, "view": view}, nil)
}

// Respond replaces the message whose button was pressed. The response URL comes
// from a signed interaction and is only ever sent to Slack's webhook host.
func (s *Slack) Respond(ctx context.Context, responseURL, text string) error {
	return s.respond(ctx, responseURL, map[string]any{"replace_original": true, "text": escapeMentions(text)})
}

// RespondAside answers a button press with a note only the presser sees,
// leaving the pressed message as it was.
func (s *Slack) RespondAside(ctx context.Context, responseURL, text string) error {
	return s.respond(ctx, responseURL, map[string]any{"replace_original": false, "response_type": "ephemeral", "text": escapeMentions(text)})
}

func (s *Slack) respond(ctx context.Context, responseURL string, body map[string]any) error {
	if !s.trustedURL(responseURL, "hooks.slack.com") {
		return errors.New("untrusted Slack response URL")
	}
	if err := s.allowed(); err != nil {
		return err
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", responseURL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.Client.Do(req)
	if err != nil {
		return errors.New("Slack response outcome is unknown")
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Slack response URL returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// carousel uploads the cards' data pictures privately, with no channel, and
// shows them by Slack file ID. Card text is plain text, so it cannot mention
// anyone.
func (s *Slack) carousel(ctx context.Context, cards []Card) (map[string]any, error) {
	elements := make([]map[string]any, 0, len(cards))
	uploaded := []map[string]string{}
	for i, c := range cards {
		card := map[string]any{"type": "card"}
		for key, text := range map[string]string{"title": c.Title, "subtitle": c.Subtitle, "body": c.Body} {
			if strings.TrimSpace(text) != "" {
				card[key] = map[string]string{"type": "plain_text", "text": text}
			}
		}
		if c.Image != nil {
			hero := map[string]any{"type": "image", "alt_text": c.Image.Alt}
			if c.Image.URL != "" {
				hero["image_url"] = c.Image.URL
			} else {
				data, ext, err := c.Image.decoded()
				if err != nil {
					return nil, err
				}
				name := fmt.Sprintf("card-%d.%s", i+1, ext)
				id, err := s.upload(ctx, name, data)
				if err != nil {
					return nil, err
				}
				uploaded = append(uploaded, map[string]string{"id": id, "title": name})
				hero["slack_file"] = map[string]string{"id": id}
			}
			card["hero_image"] = hero
		}
		elements = append(elements, card)
	}
	if len(uploaded) > 0 {
		if err := s.api(ctx, "files.completeUploadExternal", map[string]any{"files": uploaded}, nil); err != nil {
			return nil, err
		}
	}
	return map[string]any{"type": "carousel", "elements": elements}, nil
}

// Post sends one message to a channel and returns its Slack timestamp, the ID
// a later Post uses as Thread. An error matching ErrNotPosted sent nothing.
func (s *Slack) Post(ctx context.Context, channel string, p Post) (string, error) {
	if err := p.Validate(); err != nil {
		return "", notPosted{err}
	}
	if !ValidID(channel) {
		return "", notPosted{errors.New("invalid Slack channel")}
	}
	text := escapeMentions(p.Text)
	blocks := []map[string]any{{"type": "markdown", "text": text}}
	for _, image := range p.images() {
		blocks = append(blocks, map[string]any{"type": "image", "image_url": image.URL, "alt_text": image.Alt})
	}
	if len(p.Cards) > 0 {
		// Uploads alone show nobody anything: a failure here posted nothing.
		carousel, err := s.carousel(ctx, p.Cards)
		if err != nil {
			return "", notPosted{err}
		}
		blocks = append(blocks, carousel)
	}
	fields := map[string]any{"channel": channel, "text": text, "blocks": blocks, "unfurl_links": false, "unfurl_media": false, "parse": "none"}
	if p.Thread != "" {
		fields["thread_ts"] = p.Thread
	}
	var response struct {
		TS string `json:"ts"`
	}
	err := s.api(ctx, "chat.postMessage", fields, &response)
	// Slack refuses a just-uploaded card picture as an invalid file type until
	// it has processed the upload, a few seconds later. The refusal posted
	// nothing, so posting again cannot duplicate the message.
	for wait := time.Second; err != nil && unprocessedUpload(err) && wait <= 8*time.Second; wait *= 2 {
		select {
		case <-ctx.Done():
			return "", err
		case <-time.After(wait):
		}
		err = s.api(ctx, "chat.postMessage", fields, &response)
	}
	if err != nil {
		return "", err
	}
	if !messageTS.MatchString(response.TS) {
		return "", errors.New("Slack response lacks the posted message ID; outcome unknown")
	}
	return response.TS, nil
}
