package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/personal"
)

// personalCollection drives collectPersonal one collection at a time, as the
// personal worker does after each judged batch, and records its checkpoints.
type personalCollection struct {
	app         *Server
	ctx         context.Context
	run         *automation.Run
	state       personal.State
	generation  uint64
	checkpoints []int
}

func newPersonalCollection(t *testing.T, app *Server, settings catalog.AssistantSettings, keys ...string) *personalCollection {
	t.Helper()
	spec, err := app.personalSpec("alice", settings)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := app.automationContext(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	c := &personalCollection{app: app, ctx: ctx, run: &automation.Run{Spec: spec}, state: personal.State{Cursors: map[string]string{}, Seen: map[string]string{}, Drafts: map[string]personal.Draft{}}}
	if app.personalConnections != nil {
		account, _ := app.catalog.Account("alice")
		connections, err := app.personalConnections.List(account.ID)
		if err != nil || len(connections) != 1 {
			t.Fatal(connections, err)
		}
		c.generation = connections[0].Generation
	}
	// A ready suggestion per source shows how collection settled it:
	// resolved, superseded by a new observation, or left alone.
	for _, key := range keys {
		c.state.Drafts[key] = personal.Draft{ID: key, State: "ready", Source: personal.Observation{Key: key}}
	}
	return c
}

// collect runs one collection after the previous batch was judged.
func (c *personalCollection) collect() error {
	c.state.Pending, c.checkpoints = nil, nil
	return c.app.personal.Collect(c.ctx, c.run, &c.state, func() error {
		c.checkpoints = append(c.checkpoints, len(c.state.Reads))
		return nil
	})
}

func (c *personalCollection) reads() []string {
	out := []string{}
	for _, read := range c.state.Reads {
		raw, _ := json.Marshal(read.Arguments)
		out = append(out, read.Operation+" "+string(raw))
	}
	return out
}

func (c *personalCollection) pending(t *testing.T) []string {
	t.Helper()
	out := []string{}
	for _, o := range c.state.Pending {
		if o.Source.Provider == "" || o.Source.Generation != c.generation || strings.Contains(string(o.Content), "retrievedAt") || o.Revision != personal.ContentRevision(o.Kind, o.Content) {
			t.Errorf("observation %s lost its source contract or kept volatile fields: %+v", o.Key, o)
		}
		raw, _ := json.Marshal(o.Source.Arguments)
		out = append(out, o.Key+" "+o.Kind+" "+o.URL+" "+o.Source.Operation+" "+string(raw))
	}
	return out
}

func (c *personalCollection) drafts() map[string]string {
	out := map[string]string{}
	for key, d := range c.state.Drafts {
		out[key] = d.State
	}
	return out
}

func (c *personalCollection) cursor(prefix, name string) string {
	return c.state.Cursors[fmt.Sprintf("%s:%d:%s", prefix, c.generation, name)]
}

func samePersonal(t *testing.T, what string, got, want any) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Fatalf("%s:\n got %s\nwant %s", what, g, w)
	}
}

// providerLog records the provider requests that carry collection decisions,
// leaving out identity and token calls made for every operation.
type providerLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *providerLog) add(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	query := r.URL.Query()
	for _, name := range []string{"oldest", "timeMin", "timeMax", "q", "limit", "maxResults", "includeSpamTrash", "format", "singleEvents", "orderBy", "showDeleted", "types"} {
		query.Del(name)
	}
	call := r.URL.Path
	if len(query) > 0 {
		call += "?" + query.Encode()
	}
	l.calls = append(l.calls, call)
}

func (l *providerLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.calls
	l.calls = nil
	if out == nil {
		out = []string{}
	}
	return out
}

func personalCollectionFixture(t *testing.T, features ...string) (*Server, catalog.AssistantSettings) {
	t.Helper()
	app, _ := fixture(t)
	if _, err := app.catalog.SetAssistantPolicy(catalog.Bootstrap, catalog.AssistantPolicy{Enabled: true, BatchItems: 10, LookbackHours: 24}); err != nil {
		t.Fatal(err)
	}
	settings, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{Enabled: true, ExternalProcessing: true, TimeZone: "UTC", Features: features})
	if err != nil {
		t.Fatal(err)
	}
	return app, settings
}

func gmailFixtureMessage(id, thread, from string, labels []string, headers ...string) map[string]any {
	list := []any{
		map[string]string{"name": "From", "value": from},
		map[string]string{"name": "To", "value": "alice@example.test"},
		map[string]string{"name": "Subject", "value": "Subject " + id},
		map[string]string{"name": "Message-ID", "value": "<" + id + "@example.test>"},
	}
	for i := 0; i+1 < len(headers); i += 2 {
		list = append(list, map[string]string{"name": headers[i], "value": headers[i+1]})
	}
	return map[string]any{"id": id, "threadId": thread, "historyId": "101", "internalDate": "1763800000000", "labelIds": labels, "payload": map[string]any{"mimeType": "text/plain", "headers": list, "body": map[string]string{"data": base64.RawURLEncoding.EncodeToString([]byte("Body of " + id))}}}
}

func TestPersonalCollectionScansEmailThenFollowsChanges(t *testing.T) {
	app, settings := personalCollectionFixture(t, "email")
	colleague := "colleague@example.test"
	threads := map[string][]map[string]any{
		"t1": {gmailFixtureMessage("m1", "t1", colleague, []string{"INBOX"})},
		"t2": {gmailFixtureMessage("m2", "t2", colleague, []string{"INBOX"}), gmailFixtureMessage("m2r", "t2", "alice@example.test", []string{"SENT"})},
		"t3": {gmailFixtureMessage("m3", "t3", "news@example.test", []string{"INBOX"}, "List-Id", "<news.example.test>")},
		"t4": {gmailFixtureMessage("m4", "t4", colleague, []string{"INBOX"}, "Auto-Submitted", "no")},
		"t5": {gmailFixtureMessage("m5", "t5", colleague, []string{"INBOX"}), gmailFixtureMessage("m5d", "t5", "alice@example.test", []string{"DRAFT"})},
		"t7": {gmailFixtureMessage("m7", "t7", "robot@example.test", []string{"INBOX"}, "Auto-Submitted", "auto-replied")},
		"t8": {gmailFixtureMessage("m8", "t8", colleague, []string{"SPAM"})},
	}
	messages := map[string]map[string]any{}
	for _, thread := range threads {
		for _, m := range thread {
			messages[m["id"].(string)] = m
		}
	}
	var history = "100"
	log := &providerLog{}
	connectPersonalProviderHandler(t, app, "alice", "google", func(w http.ResponseWriter, r *http.Request) {
		reply := func(v any) { json.NewEncoder(w).Encode(v) }
		switch path := r.URL.Path; {
		case path == "/token":
			fmt.Fprint(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600,"scope":"openid email https://www.googleapis.com/auth/gmail.modify"}`)
		case path == "/v1/userinfo":
			fmt.Fprint(w, `{"sub":"google-alice","email":"alice@example.test","email_verified":true}`)
		case path == "/gmail/v1/users/me/profile":
			reply(map[string]string{"emailAddress": "alice@example.test", "historyId": history})
		case path == "/gmail/v1/users/me/messages":
			log.add(r)
			if r.URL.Query().Get("pageToken") == "" {
				reply(map[string]any{"messages": []any{map[string]string{"id": "m1", "threadId": "t1"}, map[string]string{"id": "m2", "threadId": "t2"}}, "nextPageToken": "page2"})
			} else {
				reply(map[string]any{"messages": []any{map[string]string{"id": "m3", "threadId": "t3"}}})
			}
		case path == "/gmail/v1/users/me/history":
			log.add(r)
			if r.URL.Query().Get("startHistoryId") != "100" {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"error":{"code":404,"message":"Requested entity was not found."}}`)
				return
			}
			added := func(ids ...string) []any {
				out := []any{}
				for _, id := range ids {
					out = append(out, map[string]any{"message": map[string]string{"id": id, "threadId": "t" + strings.TrimPrefix(id, "m")}})
				}
				return out
			}
			reply(map[string]any{"historyId": "105", "history": []any{
				map[string]any{"id": "104", "messagesAdded": added("m4", "m5", "m7", "m8")},
				map[string]any{"id": "105", "messagesDeleted": added("m6")},
			}})
		case strings.HasPrefix(path, "/gmail/v1/users/me/messages/"):
			log.add(r)
			m, ok := messages[strings.TrimPrefix(path, "/gmail/v1/users/me/messages/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"error":{"code":404,"message":"Requested entity was not found."}}`)
				return
			}
			reply(m)
		case strings.HasPrefix(path, "/gmail/v1/users/me/threads/"):
			log.add(r)
			id := strings.TrimPrefix(path, "/gmail/v1/users/me/threads/")
			reply(map[string]any{"id": id, "historyId": "101", "messages": threads[id]})
		default:
			t.Errorf("unexpected Google call %s", r.URL)
			fmt.Fprint(w, `{}`)
		}
	})
	keys := []string{"email:t1", "email:t2", "email:t3", "email:t4", "email:t5", "email:t6", "email:t7", "email:t8"}
	c := newPersonalCollection(t, app, settings, keys...)
	began := time.Now().UTC().Truncate(time.Second)
	mail := func(thread string) string {
		return "email:" + thread + " email https://mail.google.com/mail/u/?authuser=alice%40example.test#all/" + thread + ` email.thread {"id":"` + thread + `"}`
	}

	// The first collection anchors the change cursor, then scans the lookback
	// window one page at a time.
	if err := c.collect(); err != nil {
		t.Fatal(err)
	}
	samePersonal(t, "first scan calls", log.take(), []string{"/gmail/v1/users/me/messages", "/gmail/v1/users/me/messages/m1", "/gmail/v1/users/me/threads/t1", "/gmail/v1/users/me/messages/m2", "/gmail/v1/users/me/threads/t2"})
	samePersonal(t, "first scan checkpoints", c.checkpoints, []int{2, 1, 0})
	samePersonal(t, "first scan observations", c.pending(t), []string{mail("t1")})
	if c.cursor("email", "mode") != "scan" || c.cursor("email", "anchor") != "100" || c.cursor("email", "page") != "page2" {
		t.Fatalf("scan cursors: %v", c.state.Cursors)
	}
	after, err := time.Parse(time.RFC3339, c.cursor("email", "after"))
	if err != nil || after.Before(began.Add(-24*time.Hour)) || after.After(time.Now().Add(-24*time.Hour)) {
		t.Fatalf("scan does not start at the lookback horizon: %v %v", c.cursor("email", "after"), err)
	}

	// The last scan page switches to changes from the anchor, so mail that
	// arrived during the scan is not missed.
	if err = c.collect(); err != nil {
		t.Fatal(err)
	}
	samePersonal(t, "second scan calls", log.take(), []string{"/gmail/v1/users/me/messages?pageToken=page2", "/gmail/v1/users/me/messages/m3", "/gmail/v1/users/me/threads/t3"})
	samePersonal(t, "second scan observations", c.pending(t), []string{})
	if c.cursor("email", "mode") != "changes" || c.cursor("email", "cursor") != "100" || c.cursor("email", "page") != "" {
		t.Fatalf("change cursors: %v", c.state.Cursors)
	}

	// Changes queue each message; only a thread waiting on someone else is
	// observed.
	if err = c.collect(); err != nil {
		t.Fatal(err)
	}
	samePersonal(t, "change calls", log.take(), []string{
		"/gmail/v1/users/me/history?startHistoryId=100",
		"/gmail/v1/users/me/messages/m4", "/gmail/v1/users/me/threads/t4",
		"/gmail/v1/users/me/messages/m5", "/gmail/v1/users/me/threads/t5",
		"/gmail/v1/users/me/messages/m7", "/gmail/v1/users/me/threads/t7",
		"/gmail/v1/users/me/messages/m8", "/gmail/v1/users/me/threads/t8",
		"/gmail/v1/users/me/messages/m6",
	})
	samePersonal(t, "change checkpoints", c.checkpoints, []int{5, 4, 3, 2, 1, 0})
	samePersonal(t, "change observations", c.pending(t), []string{mail("t4")})
	samePersonal(t, "settled suggestions", c.drafts(), map[string]string{"email:t1": "superseded", "email:t2": "resolved", "email:t3": "resolved", "email:t4": "superseded", "email:t5": "ready", "email:t6": "ready", "email:t7": "resolved", "email:t8": "resolved"})
	if c.cursor("email", "cursor") != "105" || c.cursor("email", "mode") != "changes" {
		t.Fatalf("change cursor did not advance: %v", c.state.Cursors)
	}

	// An expired change cursor drops back to a fresh scan.
	if err = c.collect(); err != nil {
		t.Fatal(err)
	}
	samePersonal(t, "expired calls", log.take(), []string{"/gmail/v1/users/me/history?startHistoryId=105"})
	samePersonal(t, "expired checkpoints", c.checkpoints, []int{0, 0})
	if _, ok := c.state.Cursors[fmt.Sprintf("email:%d:mode", c.generation)]; ok || len(c.state.Reads) != 0 {
		t.Fatalf("expired cursor kept its mode: %v %v", c.state.Cursors, c.reads())
	}
	history = "110"
	if err = c.collect(); err != nil {
		t.Fatal(err)
	}
	log.take()
	if c.cursor("email", "anchor") != "110" || c.cursor("email", "mode") != "scan" || c.cursor("email", "page") != "page2" {
		t.Fatalf("rescan did not re-anchor: %v", c.state.Cursors)
	}
}

func TestPersonalCollectionQueuesEachSlackThreadOnce(t *testing.T) {
	app, settings := personalCollectionFixture(t, "slack")
	message := func(user, ts string, fields ...string) map[string]any {
		m := map[string]any{"type": "message", "user": user, "ts": ts, "text": "Message " + ts}
		for i := 0; i+1 < len(fields); i += 2 {
			m[fields[i]] = fields[i+1]
		}
		return m
	}
	history := map[string]map[string]any{
		"D1": {"ok": true, "messages": []any{
			message("U2", "1.5"),
			message("U2", "1.2", "thread_ts", "1.1"),
			message("U2", "1.1"),
			message("", "1.3", "bot_id", "B1"),
			message("U2", "1.4", "subtype", "channel_join"),
		}, "has_more": true, "response_metadata": map[string]string{"next_cursor": "h2"}},
		"D1h2": {"ok": true, "messages": []any{message("U2", "1.0", "thread_ts", "1.1")}, "response_metadata": map[string]string{"next_cursor": ""}},
		"D2":   {"ok": true, "messages": []any{message("U123", "2.1")}},
		"D3":   {"ok": true, "messages": []any{}, "has_more": true},
	}
	replies := map[string][]any{
		"D1/1.1": {message("U2", "1.1"), message("U2", "1.2", "thread_ts", "1.1")},
		"D1/1.5": {message("U2", "1.5"), message("U123", "1.6", "thread_ts", "1.5")},
		"D2/2.1": {},
	}
	log := &providerLog{}
	connectPersonalProviderHandler(t, app, "alice", "slack", func(w http.ResponseWriter, r *http.Request) {
		reply := func(v any) { json.NewEncoder(w).Encode(v) }
		query := r.URL.Query()
		switch r.URL.Path {
		case "/api/oauth.v2.access":
			fmt.Fprint(w, `{"ok":true,"team":{"id":"T123"},"authed_user":{"id":"U123","token_type":"user","access_token":"fixture-user","scope":"im:read,mpim:read,im:history,mpim:history"}}`)
		case "/api/auth.test":
			fmt.Fprint(w, `{"ok":true,"user_id":"U123","team_id":"T123"}`)
		case "/api/users.conversations":
			log.add(r)
			if query.Get("cursor") == "" {
				reply(map[string]any{"ok": true, "channels": []any{map[string]string{"id": "D1"}, map[string]string{"id": "D2"}}, "response_metadata": map[string]string{"next_cursor": "c2"}})
			} else {
				reply(map[string]any{"ok": true, "channels": []any{map[string]string{"id": "D3"}}, "response_metadata": map[string]string{"next_cursor": ""}})
			}
		case "/api/conversations.history":
			log.add(r)
			reply(history[query.Get("channel")+query.Get("cursor")])
		case "/api/conversations.replies":
			log.add(r)
			reply(map[string]any{"ok": true, "messages": replies[query.Get("channel")+"/"+query.Get("ts")]})
		default:
			t.Errorf("unexpected Slack call %s", r.URL)
			fmt.Fprint(w, `{"ok":false}`)
		}
	})
	c := newPersonalCollection(t, app, settings, "slack:D1:1.1", "slack:D1:1.5", "slack:D2:2.1")
	began := time.Now()
	if err := c.collect(); err != nil {
		t.Fatal(err)
	}
	samePersonal(t, "Slack calls", log.take(), []string{
		"/api/users.conversations",
		"/api/conversations.history?channel=D1",
		"/api/conversations.history?channel=D2",
		"/api/conversations.history?channel=D1&cursor=h2",
		"/api/conversations.replies?channel=D1&ts=1.5",
		"/api/conversations.replies?channel=D1&ts=1.1",
		"/api/conversations.replies?channel=D2&ts=2.1",
	})
	// Each page checkpoint keeps its continuation and every thread it found;
	// a thread already queued is not queued again.
	samePersonal(t, "Slack checkpoints", c.checkpoints, []int{2, 4, 4, 3, 2, 1, 0})
	samePersonal(t, "Slack observations", c.pending(t), []string{`slack:D1:1.1 slack slack://channel?id=D1&message=1.1 slack.thread {"channel":"D1","ts":"1.1"}`})
	samePersonal(t, "Slack suggestions", c.drafts(), map[string]string{"slack:D1:1.1": "superseded", "slack:D1:1.5": "resolved", "slack:D2:2.1": "resolved"})
	if c.cursor("slack", "page") != "c2" {
		t.Fatalf("conversation page cursor: %v", c.state.Cursors)
	}

	// The next collection continues the conversation list; a truncated history
	// page without a continuation fails without losing the queued read.
	err := c.collect()
	if err == nil || err.Error() != "Slack history truncated without continuation" {
		t.Fatalf("truncated history accepted: %v", err)
	}
	samePersonal(t, "truncated history calls", log.take(), []string{"/api/users.conversations?cursor=c2", "/api/conversations.history?channel=D3"})
	reads := c.reads()
	if len(reads) != 1 || !strings.HasPrefix(reads[0], `slack.history {"channel":"D3","oldest":"`) {
		t.Fatalf("queued history read lost: %v", reads)
	}
	oldest, err := strconv.ParseInt(fmt.Sprint(c.state.Reads[0].Arguments["oldest"]), 10, 64)
	if err != nil || oldest < began.Add(-24*time.Hour).Unix()-1 || oldest > time.Now().Add(-24*time.Hour).Unix() {
		t.Fatalf("history does not start at the lookback horizon: %v %v", c.state.Reads[0].Arguments["oldest"], err)
	}
}

func TestPersonalCollectionPagesCalendarEvents(t *testing.T) {
	app, settings := personalCollectionFixture(t, "calendar")
	log := &providerLog{}
	var windows []string
	connectPersonalProviderHandler(t, app, "alice", "google", func(w http.ResponseWriter, r *http.Request) {
		reply := func(v any) { json.NewEncoder(w).Encode(v) }
		switch r.URL.Path {
		case "/token":
			fmt.Fprint(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":3600,"scope":"openid email https://www.googleapis.com/auth/calendar.events"}`)
		case "/v1/userinfo":
			fmt.Fprint(w, `{"sub":"google-alice","email":"alice@example.test","email_verified":true}`)
		case "/calendar/v3/calendars/primary/events":
			log.add(r)
			windows = append(windows, r.URL.Query().Get("timeMin")+" "+r.URL.Query().Get("timeMax"))
			if r.URL.Query().Get("pageToken") == "" {
				reply(map[string]any{"nextPageToken": "e2", "items": []any{
					map[string]any{"id": "ev1", "status": "confirmed", "etag": "1", "summary": "Planning", "htmlLink": "https://calendar.example.test/ev1"},
					map[string]any{"id": "ev2", "status": "cancelled"},
				}})
			} else {
				reply(map[string]any{"items": []any{map[string]any{"id": "ev3", "status": "confirmed", "etag": "3", "summary": "Review", "htmlLink": "https://calendar.example.test/ev3"}}})
			}
		default:
			t.Errorf("unexpected Google call %s", r.URL)
			fmt.Fprint(w, `{}`)
		}
	})
	c := newPersonalCollection(t, app, settings, "calendar:ev1", "calendar:ev2", "calendar:ev3")
	began := time.Now().UTC().Truncate(time.Second)
	if err := c.collect(); err != nil {
		t.Fatal(err)
	}
	samePersonal(t, "calendar calls", log.take(), []string{"/calendar/v3/calendars/primary/events", "/calendar/v3/calendars/primary/events?pageToken=e2"})
	samePersonal(t, "calendar checkpoints", c.checkpoints, []int{1, 1, 0})
	samePersonal(t, "calendar observations", c.pending(t), []string{
		`calendar:ev1 calendar https://calendar.example.test/ev1 calendar.event {"calendar":"primary","id":"ev1"}`,
		`calendar:ev3 calendar https://calendar.example.test/ev3 calendar.event {"calendar":"primary","id":"ev3"}`,
	})
	samePersonal(t, "calendar suggestions", c.drafts(), map[string]string{"calendar:ev1": "superseded", "calendar:ev2": "resolved", "calendar:ev3": "superseded"})
	if !strings.Contains(string(c.state.Pending[0].Content), `"summary":"Planning"`) {
		t.Fatalf("event payload not retained: %s", c.state.Pending[0].Content)
	}
	// Both pages read the same rolling horizon from the collection time.
	if len(windows) != 2 || windows[0] != windows[1] {
		t.Fatalf("pages read different windows: %v", windows)
	}
	from, to, _ := strings.Cut(windows[0], " ")
	start, err := time.Parse(time.RFC3339, from)
	end, err2 := time.Parse(time.RFC3339, to)
	if err != nil || err2 != nil || start.Before(began) || start.After(time.Now()) || end.Sub(start) != 24*time.Hour {
		t.Fatalf("calendar window is not the lookback horizon from now: %v", windows)
	}
}

func TestPersonalCollectionObservesDataWatchesOnce(t *testing.T) {
	app, settings := personalCollectionFixture(t, "operations")
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprintf(w, `{"count":%d,"retrievedAt":"volatile"}`, 7)
	}))
	t.Cleanup(upstream.Close)
	manager, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "metrics", Kind: "http", Endpoint: upstream.URL, ExternalProcessing: true, Operations: []connectors.Operation{{Name: "read", ReadOnly: true, Method: "GET", Path: "/"}, {Name: "write", Method: "POST", Path: "/"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	app.dataConnections = manager
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "metrics", Users: map[string][]string{"alice": {"read"}}}); err != nil {
		t.Fatal(err)
	}
	settings.Watches = []catalog.AssistantWatch{{Connection: "metrics", Operation: "read", Arguments: json.RawMessage(`{}`)}}
	if settings, err = app.catalog.SaveAssistant("alice", settings); err != nil {
		t.Fatal(err)
	}
	c := newPersonalCollection(t, app, settings)
	if err = c.collect(); err != nil {
		t.Fatal(err)
	}
	call := automation.Call{Connection: "metrics", Operation: "read", Arguments: map[string]any{}}
	if calls != 1 || len(c.state.Pending) != 1 || len(c.run.Receipts) != 1 || len(c.state.Reads) != 0 {
		t.Fatalf("watch not observed once: calls=%d pending=%d receipts=%d", calls, len(c.state.Pending), len(c.run.Receipts))
	}
	samePersonal(t, "watch checkpoints", c.checkpoints, []int{0})
	o := c.state.Pending[0]
	if o.Key != "data:"+automation.Hash(call) || o.Kind != "operations" || o.URL != "receipt:"+c.run.Receipts[0].ID || o.Data == nil || automation.Hash(*o.Data) != automation.Hash(call) || o.Revision != automation.Hash([]byte(o.Content)) || !strings.Contains(string(o.Content), "volatile") {
		t.Fatalf("watch observation: %+v", o)
	}

	settings.Watches[0].Operation = "write"
	if settings, err = app.catalog.SaveAssistant("alice", settings); err != nil {
		t.Fatal(err)
	}
	c = newPersonalCollection(t, app, settings)
	if err = c.collect(); err == nil || err.Error() != "personal data watches require an allowed read-only operation" || calls != 1 {
		t.Fatalf("write operation watched: calls=%d %v", calls, err)
	}
}

// With no data connections, as when an instance turns the data-connections
// module off, a person's saved watches have nothing to read. The rest of
// their assistant goes on and the watches stay saved.
func TestPersonalCollectionSkipsWatchesWithoutDataConnections(t *testing.T) {
	app, settings := personalCollectionFixture(t, "operations")
	app.dataConnections = nil
	settings.Watches = []catalog.AssistantWatch{{Connection: "metrics", Operation: "read", Arguments: json.RawMessage(`{}`)}}
	settings, err := app.catalog.SaveAssistant("alice", settings)
	if err != nil {
		t.Fatal(err)
	}
	c := newPersonalCollection(t, app, settings)
	if err = c.collect(); err != nil {
		t.Fatal("a saved watch stops the assistant:", err)
	}
	if len(c.state.Pending) != 0 || len(c.run.Receipts) != 0 {
		t.Fatalf("a watch ran without data connections: pending=%d receipts=%d", len(c.state.Pending), len(c.run.Receipts))
	}
	if stored, err := app.catalog.Assistant("alice"); err != nil || len(stored.Watches) != 1 {
		t.Fatalf("the saved watch is gone: %+v %v", stored.Watches, err)
	}
}
