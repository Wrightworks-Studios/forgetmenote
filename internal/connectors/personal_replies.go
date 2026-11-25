package connectors

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// SlackWaiting is a Slack message someone else sent a person that they have
// not answered: a direct or group message, or one that mentions them. It
// names where and when, never what the message says.
type SlackWaiting struct {
	Channel   string    `json:"channel"`
	TS        string    `json:"ts"`
	Kind      string    `json:"kind"`
	Author    string    `json:"author"`
	Sent      time.Time `json:"sent"`
	Permalink string    `json:"permalink"`
}

const (
	// Slack search returns at most 20 messages a page and allows about ten
	// searches a minute, so a person's morning reads at most this many pages:
	// 100 direct and group messages and 60 mentions.
	slackWaitingDMPages, slackWaitingMentionPages = 5, 3
	// Each candidate costs one more read to see whether they replied or
	// reacted; the rest are reported as unchecked.
	SlackWaitingChecks = 10
)

// The search scopes find candidates; the history scopes check each one.
var slackWaitingScopes = []string{"search:read.im", "search:read.mpim", "search:read.public", "search:read.private", "im:history", "mpim:history", "channels:history", "groups:history"}

type slackCandidate struct {
	SlackWaiting
	thread string
}

// SlackWaitingFor finds the messages a person has not answered that arrived
// between two times, with their own Slack grant. It searches their direct and
// group messages, and messages that mention them, then reads each candidate's
// conversation or thread, up to SlackWaitingChecks of them, for a later
// message or a reaction from them. It returns the unanswered ones, direct and
// group messages first and then the oldest, and how many candidates were left
// unchecked. A conversation or thread whose latest message from someone else
// the person dismissed is left out before any check. ErrProviderBusy means
// Slack is limiting requests; try later.
func (p *PersonalConnections) SlackWaitingFor(ctx context.Context, owner string, generation uint64, from, to time.Time, dismissed func(channel, ts string) bool, guard func(context.Context) error) ([]SlackWaiting, int, error) {
	if guard == nil || !from.Before(to) {
		return nil, 0, ErrInvalid
	}
	if err := guard(ctx); err != nil {
		return nil, 0, err
	}
	c, err := p.token(ctx, owner, "slack", generation)
	if err != nil {
		return nil, 0, err
	}
	for _, scope := range slackWaitingScopes {
		if err = p.scope(owner, "slack", generation, scope); err != nil {
			return nil, 0, err
		}
	}
	self := c.Subject
	if !identifier.MatchString(self) {
		return nil, 0, errors.New("Slack grant has no member ID")
	}
	check := func(ctx context.Context) error {
		if err := guard(ctx); err != nil {
			return err
		}
		return p.Check(owner, "slack", generation)
	}
	// One candidate per conversation or thread: the latest message in it from
	// someone else.
	seen := map[string]bool{}
	candidates := []slackCandidate{}
	groups := map[string]bool{}
	add := func(m map[string]any, kind string) {
		channel, ts, author := str(m["channel_id"]), str(m["message_ts"]), str(m["author_user_id"])
		sent, ok := slackTime(ts)
		if !identifier.MatchString(channel) || !ok || !identifier.MatchString(author) || author == self || m["is_author_bot"] == true || !sent.Before(to) || sent.Before(from) {
			return
		}
		thread := ""
		if link, err := url.Parse(str(m["permalink"])); err == nil {
			thread = link.Query().Get("thread_ts")
		}
		if kind == "mention" && !strings.Contains(str(m["content"]), "<@"+self) {
			return
		}
		key := channel + "/" + thread
		if seen[key] {
			return
		}
		seen[key] = true
		if dismissed != nil && dismissed(channel, ts) {
			return
		}
		if kind == "mention" && strings.HasPrefix(channel, "D") {
			kind = "dm"
		} else if kind == "mention" && groups[channel] {
			kind = "group"
		}
		candidates = append(candidates, slackCandidate{SlackWaiting: SlackWaiting{Channel: channel, TS: ts, Kind: kind, Author: author, Sent: sent, Permalink: str(m["permalink"])}, thread: thread})
	}
	search := func(query string, types []string, pages int, kind string) error {
		cursor := ""
		for range pages {
			body := map[string]any{"query": query, "channel_types": types, "content_types": []string{"messages"}, "after": from.Unix(), "before": to.Unix(), "sort": "timestamp", "sort_dir": "desc", "limit": 20, "include_bots": false, "disable_semantic_search": true}
			if cursor != "" {
				body["cursor"] = cursor
			}
			raw, _ := json.Marshal(body)
			if err := check(ctx); err != nil {
				return err
			}
			result, err := p.requestJSON(ctx, "POST", p.slackAPI+"/assistant.search.context", c.AccessToken, bytes.NewReader(raw))
			if err != nil {
				return err
			}
			if err = slackOK(result, "Slack search"); err != nil {
				return err
			}
			for _, v := range slackList(object(result["results"])["messages"]) {
				m := object(v)
				if kind == "dm" && !strings.HasPrefix(str(m["channel_id"]), "D") {
					groups[str(m["channel_id"])] = true
					add(m, "group")
					continue
				}
				add(m, kind)
			}
			if cursor = str(object(result["response_metadata"])["next_cursor"]); cursor == "" {
				return nil
			}
		}
		return nil
	}
	if err = search("-from:<@"+self+">", []string{"im", "mpim"}, slackWaitingDMPages, "dm"); err != nil {
		return nil, 0, err
	}
	if err = search("<@"+self+">", []string{"public_channel", "private_channel", "mpim", "im"}, slackWaitingMentionPages, "mention"); err != nil {
		return nil, 0, err
	}
	slices.SortStableFunc(candidates, func(a, b slackCandidate) int {
		return cmp.Or(cmp.Compare(slackKindOrder(a.Kind), slackKindOrder(b.Kind)), a.Sent.Compare(b.Sent))
	})
	waiting := []SlackWaiting{}
	checked := min(len(candidates), SlackWaitingChecks)
	for _, candidate := range candidates[:checked] {
		answered, err := p.slackAnswered(ctx, c, self, candidate, check)
		if err != nil {
			return nil, 0, err
		}
		if !answered {
			waiting = append(waiting, candidate.SlackWaiting)
		}
	}
	return waiting, len(candidates) - checked, nil
}

// slackAnswered reads the candidate's thread, or its conversation from the
// candidate on, for a later message from the person or their reaction to it.
// In a channel, a later message there or in the message's thread counts.
func (p *PersonalConnections) slackAnswered(ctx context.Context, c personalCredential, self string, m slackCandidate, check func(context.Context) error) (bool, error) {
	q := url.Values{"channel": {m.Channel}, "limit": {"100"}}
	method := "conversations.history"
	if m.thread != "" && m.thread != m.TS {
		method = "conversations.replies"
		q.Set("ts", m.thread)
	} else {
		q.Set("oldest", m.TS)
		q.Set("inclusive", "true")
	}
	if err := check(ctx); err != nil {
		return false, err
	}
	result, err := p.requestJSON(ctx, "GET", p.slackAPI+"/"+method+"?"+q.Encode(), c.AccessToken, nil)
	if err != nil {
		return false, err
	}
	if err = slackOK(result, "Slack conversation read"); err != nil {
		return false, err
	}
	for _, v := range slackList(result["messages"]) {
		reply := object(v)
		ts := str(reply["ts"])
		if ts == m.TS {
			for _, r := range slackList(reply["reactions"]) {
				if slices.Contains(slackStrings(object(r)["users"]), self) {
					return true, nil
				}
			}
			if slices.Contains(slackStrings(reply["reply_users"]), self) {
				return true, nil
			}
			continue
		}
		if str(reply["user"]) == self && slackAfter(ts, m.TS) {
			return true, nil
		}
	}
	return false, nil
}

func slackOK(result map[string]any, what string) error {
	if result["ok"] != true {
		return errors.New(what + " was not acknowledged")
	}
	return nil
}

func slackKindOrder(kind string) int {
	return map[string]int{"dm": 0, "group": 1, "mention": 2}[kind]
}

func slackList(v any) []any { list, _ := v.([]any); return list }

func slackStrings(v any) []string {
	out := []string{}
	for _, item := range slackList(v) {
		out = append(out, str(item))
	}
	return out
}

// slackTime reads a Slack message timestamp, seconds and microseconds.
func slackTime(ts string) (time.Time, bool) {
	seconds, micros, _ := strings.Cut(ts, ".")
	s, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil || s <= 0 {
		return time.Time{}, false
	}
	u, _ := strconv.ParseInt((micros + "000000")[:6], 10, 64)
	return time.Unix(s, u*1000).UTC(), true
}

// slackAfter reports whether timestamp a is later than b.
func slackAfter(a, b string) bool {
	ta, okA := slackTime(a)
	tb, okB := slackTime(b)
	return okA && okB && ta.After(tb)
}
