package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSlackWaitingFindsWhatThePersonHasNotAnswered(t *testing.T) {
	requireSlack(t)
	var searches, reads atomic.Int32
	var busy atomic.Bool
	var scopes atomic.Value
	scopes.Store("im:history,chat:write")
	message := func(channel, ts, author, content string, extra ...string) map[string]any {
		m := map[string]any{"channel_id": channel, "message_ts": ts, "author_user_id": author, "content": content, "is_author_bot": false, "permalink": "https://example.slack.com/archives/" + channel + "/p" + strings.ReplaceAll(ts, ".", "")}
		if len(extra) > 0 {
			m["permalink"] = m["permalink"].(string) + "?thread_ts=" + extra[0] + "&cid=" + channel
		}
		return m
	}
	bot := message("D3", "1000.000150", "B1", "Build finished")
	bot["is_author_bot"] = true
	p, _, _ := personalFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-user-token" && r.URL.Path != "/oauth.v2.access" {
			t.Errorf("%s used another identity", r.URL.Path)
		}
		switch r.URL.Path {
		case "/oauth.v2.access":
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "team": map[string]string{"id": "T123"}, "authed_user": map[string]any{"id": "U123", "token_type": "user", "access_token": "fixture-user-token", "scope": scopes.Load()}})
		case "/auth.test":
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "user_id": "U123", "team_id": "T123"})
		case "/assistant.search.context":
			searches.Add(1)
			if busy.Load() {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			var in struct {
				Query        string   `json:"query"`
				ChannelTypes []string `json:"channel_types"`
				After        int64    `json:"after"`
				Before       int64    `json:"before"`
				Sort         string   `json:"sort"`
				Semantic     bool     `json:"disable_semantic_search"`
				Bots         bool     `json:"include_bots"`
				Cursor       string   `json:"cursor"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.After != 990 || in.Before != 1100 || in.Sort != "timestamp" || !in.Semantic || in.Bots {
				t.Errorf("unexpected search %+v %v", in, err)
			}
			var messages []any
			cursor := ""
			switch {
			case in.Query == "-from:<@U123>" && strings.Join(in.ChannelTypes, ",") == "im,mpim" && in.Cursor == "":
				messages = []any{
					message("D1", "1000.000100", "U456", "Lunch?"),
					message("D2", "1000.000200", "U789", "Can you send the deck?"),
					message("D2", "999.000100", "U789", "Hi"),
					message("G1", "1000.000300", "U456", "Group question"),
					bot,
					message("D4", "1000.000160", "U123", "My own message"),
					message("D6", "1100.000000", "U456", "Sent as the window closed"),
				}
				cursor = "dm-2"
			case in.Query == "-from:<@U123>" && in.Cursor == "dm-2":
				messages = []any{message("D5", "1000.000400", "U456", "Following up in the thread", "900.000000")}
			case in.Query == "<@U123>" && len(in.ChannelTypes) == 4:
				messages = []any{
					message("C1", "1000.000500", "U456", "<@U123|me> can you look?"),
					message("C2", "1000.000600", "U456", "<@U123> please review"),
					message("C3", "1000.000650", "U456", "a note about U123 without a mention"),
					message("D2", "1000.000700", "U789", "<@U123> again"),
					message("C4", "980.000000", "U456", "<@U123> before the window"),
				}
			default:
				t.Errorf("unexpected search %+v", in)
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "results": map[string]any{"messages": messages}, "response_metadata": map[string]string{"next_cursor": cursor}})
		case "/conversations.history", "/conversations.replies":
			reads.Add(1)
			q := r.URL.Query()
			var messages []any
			switch q.Get("channel") {
			case "D1":
				messages = []any{map[string]any{"ts": "1000.000100", "user": "U456"}, map[string]any{"ts": "1000.000900", "user": "U123"}}
			case "D2":
				messages = []any{map[string]any{"ts": "1000.000200", "user": "U789", "reactions": []any{map[string]any{"name": "eyes", "users": []string{"U789"}}}}}
			case "G1":
				messages = []any{map[string]any{"ts": "1000.000300", "user": "U456", "reactions": []any{map[string]any{"name": "white_check_mark", "users": []string{"U123"}}}}}
			case "D5":
				if r.URL.Path != "/conversations.replies" || q.Get("ts") != "900.000000" {
					t.Errorf("a thread reply was not read as a thread: %s", r.URL)
				}
				messages = []any{map[string]any{"ts": "900.000000", "user": "U123"}, map[string]any{"ts": "1000.000400", "user": "U456"}}
			case "C1":
				messages = []any{map[string]any{"ts": "1000.000500", "user": "U456", "reply_users": []string{"U123"}}}
			case "C2":
				if q.Get("oldest") != "1000.000600" || q.Get("inclusive") != "true" {
					t.Errorf("a channel mention was not read from the mention on: %s", r.URL)
				}
				messages = []any{map[string]any{"ts": "1000.000600", "user": "U456"}, map[string]any{"ts": "1000.000800", "user": "U999"}}
			default:
				t.Errorf("unexpected read %s", r.URL)
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": messages})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	connect := func() uint64 {
		t.Helper()
		target, err := p.Begin(personalTestOwner, "slack", "browser", "https://example.test/callback")
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(target)
		connection, err := p.Finish(t.Context(), personalTestOwner, "slack", "browser", u.Query().Get("state"), "code", func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		return connection.Generation
	}
	if !strings.Contains(func() string {
		u, _ := p.Begin(personalTestOwner, "slack", "browser", "https://example.test/callback")
		return u
	}(), "search%3Aread.im") {
		t.Fatal("the Slack grant does not ask for search")
	}
	guard := func(context.Context) error { return nil }
	from, to := time.Unix(990, 0), time.Unix(1100, 0)

	// An older grant without search is refused before any search.
	generation := connect()
	if _, _, err := p.SlackWaitingFor(t.Context(), personalTestOwner, generation, from, to, nil, guard); err == nil || searches.Load() != 0 {
		t.Fatalf("a grant without search scopes searched: %v", err)
	}
	scopes.Store(strings.Join(slackWaitingScopes, ","))
	generation = connect()
	if _, _, err := p.SlackWaitingFor(t.Context(), personalTestOwner, generation, from, to, nil, func(context.Context) error { return errors.New("revoked") }); err == nil || searches.Load() != 0 {
		t.Fatal("a refused guard still searched", err)
	}

	waiting, unchecked, err := p.SlackWaitingFor(t.Context(), personalTestOwner, generation, from, to, nil, guard)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, w := range waiting {
		got = append(got, w.Kind+":"+w.Channel+":"+w.Author)
	}
	if strings.Join(got, " ") != "dm:D2:U789 dm:D5:U456 mention:C2:U456" || unchecked != 0 {
		t.Fatalf("waiting = %v, unchecked %d", got, unchecked)
	}
	if waiting[0].Sent != time.Unix(1000, 200000).UTC() || !strings.HasPrefix(waiting[0].Permalink, "https://example.slack.com/archives/D2/") {
		t.Fatalf("first waiting message = %+v", waiting[0])
	}
	// Reads: D1, D2, G1, D5, C1 and C2, one each.
	if reads.Load() != 6 || searches.Load() != 3 {
		t.Fatalf("searched %d and read %d times", searches.Load(), reads.Load())
	}

	// Dismissing D2's latest message drops the conversation, its earlier
	// message included, before any read.
	before := reads.Load()
	waiting, _, err = p.SlackWaitingFor(t.Context(), personalTestOwner, generation, from, to, func(channel, ts string) bool { return channel == "D2" && ts == "1000.000200" }, guard)
	got = []string{}
	for _, w := range waiting {
		got = append(got, w.Channel)
	}
	if err != nil || strings.Join(got, " ") != "D5 C2" || reads.Load()-before != 5 {
		t.Fatalf("after dismissing D2: %v, %d reads, %v", got, reads.Load()-before, err)
	}

	busy.Store(true)
	if _, _, err := p.SlackWaitingFor(t.Context(), personalTestOwner, generation, from, to, nil, guard); !errors.Is(err, ErrProviderBusy) {
		t.Fatal("a rate-limited search was not reported as busy", err)
	}
}

func TestSlackTimestampsCompareToTheMicrosecond(t *testing.T) {
	if !slackAfter("1000.000010", "1000.000009") || slackAfter("1000.1", "1000.100000") || !slackAfter("1001", "1000.999999") {
		t.Fatal("Slack timestamps compare wrongly")
	}
	if _, ok := slackTime("not-a-time"); ok {
		t.Fatal("an invalid timestamp parsed")
	}
}
