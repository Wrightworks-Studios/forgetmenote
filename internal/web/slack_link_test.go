package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"forgetmenote/internal/agentmemory"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/messaging"
)

// slackLinkFixture serves Slack workspace members through a service account,
// with no administrator mapping, and seeds one item in alice's memory.
type slackLinkFixture struct {
	app      *Server
	requests *slackMemoryRequests
	replies  []string
	// posts are the bodies of every message sent, with any buttons.
	posts []map[string]any
	// responses are the texts that answered a pressed button, and asides
	// whether each one left the pressed message in place.
	responses []string
	asides    []bool
	service   string
	upstream  string
	// members is the workspace's users.list answer.
	members []map[string]any
	// channels and groups are the members conversations.members and
	// usergroups.users.list report; a missing one is not found.
	channels, groups map[string][]string
	// views are the modals opened with views.open.
	views []map[string]any
	// failPosts is how many coming messages fail with an unknown outcome, and
	// refusePosts how many more Slack refuses, so nothing is posted.
	failPosts, refusePosts int
	// userInfos counts users.info calls, and memberLists users.list calls.
	userInfos, memberLists int
	// guests are the users users.info reports as guests.
	guests map[string]bool
	// mu guards what the fake Slack records, since messages may be sent
	// concurrently.
	mu sync.Mutex
}

func newSlackLinkFixture(t *testing.T) *slackLinkFixture {
	t.Helper()
	f := &slackLinkFixture{requests: &slackMemoryRequests{}}
	f.app, _ = runnerFixture(t, f.requests.handle)
	app := f.app
	configureSlackFixture(t, app)
	if err := app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "bob"}); err != nil {
		t.Fatal(err)
	}
	service, err := app.catalog.CreateService(catalog.Bootstrap, "Company assistant", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", service.ID, "read"); err != nil {
		t.Fatal(err)
	}
	f.service = service.ID
	policy := app.slack.policy()
	policy.Users, policy.Channels, policy.DirectMessages = nil, nil, nil
	policy.Members = &messaging.MemberAccess{Actor: service.ID, Binding: messaging.Binding{Project: "default", AIConnection: "local", Model: "synthetic", ExternalProcessing: true}}
	if _, err = app.catalog.SetChatClients(catalog.Bootstrap, catalog.ChatClients{Clients: map[string]messaging.Client{"slack": policy}}); err != nil {
		t.Fatal(err)
	}
	settings, err := app.catalog.SaveAssistant("alice", catalog.AssistantSettings{TimeZone: "UTC", MaintainMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	store, err := app.catalog.Access("alice", settings.Project, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(t.Context(), agentmemory.Path, []byte(agentmemory.Header("")+"\n## Facts\n\n- Alice owns the Denver office rollout.\n"), "", "alice"); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/users.info":
			f.userInfos++
			user := r.URL.Query().Get("user")
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": map[string]any{"id": user, "team_id": "T123", "is_restricted": f.guests[user]}})
		case "/users.list":
			f.memberLists++
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "members": f.members})
		case "/conversations.members":
			if members, ok := f.channels[r.URL.Query().Get("channel")]; ok {
				json.NewEncoder(w).Encode(map[string]any{"ok": true, "members": members})
				return
			}
			w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
		case "/chat.getPermalink":
			channel, ts := r.URL.Query().Get("channel"), r.URL.Query().Get("message_ts")
			if channel == "CHIDDEN" {
				w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "permalink": "https://acme.slack.com/archives/" + channel + "/p" + strings.ReplaceAll(ts, ".", "")})
		case "/usergroups.users.list":
			if users, ok := f.groups[r.URL.Query().Get("usergroup")]; ok {
				json.NewEncoder(w).Encode(map[string]any{"ok": true, "users": users})
				return
			}
			w.Write([]byte(`{"ok":false,"error":"no_such_subteam"}`))
		case "/views.open":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			f.views = append(f.views, body)
			w.Write([]byte(`{"ok":true}`))
		case "/chat.postMessage", "/chat.postEphemeral":
			if f.failPosts > 0 {
				f.failPosts--
				http.Error(w, "upstream timeout", http.StatusGatewayTimeout)
				return
			}
			if f.refusePosts > 0 {
				f.refusePosts--
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
				return
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			channel, _ := body["channel"].(string)
			text, _ := body["text"].(string)
			f.posts = append(f.posts, body)
			f.replies = append(f.replies, channel+": "+text)
			w.Write([]byte(`{"ok":true}`))
		default:
			if strings.HasPrefix(r.URL.Path, "/actions/") {
				var body struct {
					Text    string `json:"text"`
					Replace bool   `json:"replace_original"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				f.responses = append(f.responses, body.Text)
				f.asides = append(f.asides, !body.Replace)
			}
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(upstream.Close)
	f.upstream = upstream.URL
	app.slack.endpoint = upstream.URL + "/chat.postEphemeral"
	return f
}

// dm sends one direct message from Slack user UNEW and runs it.
func (f *slackLinkFixture) dm(t *testing.T, id, text string) string {
	t.Helper()
	before := len(f.replies)
	d := messaging.Address{Client: "slack", Workspace: "T123", Channel: "DNEW", Thread: id, Message: id, User: "UNEW", Direct: true}
	if err := f.app.enqueueMessage(t.Context(), messaging.Event{ID: id, Text: text, Address: d}); err != nil {
		t.Fatal(err)
	}
	if ran, err := f.app.automations.Queue.RunNext(t.Context(), f.app.runAutomation); err != nil || !ran || len(f.replies) != before+1 {
		t.Fatalf("the DM %q was not answered once: %v %v %v", text, ran, err, f.replies[before:])
	}
	return f.replies[before]
}

// browser makes a signed-in form request as user.
func (f *slackLinkFixture) browser(user, method, path string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, f.app.origin+path, strings.NewReader(form.Encode()))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Remote-User", user)
	r.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
	r.Header.Set("Origin", f.app.origin)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	f.app.ServeHTTP(w, r)
	return w
}

var slackLinkURL = regexp.MustCompile(`https?://[^\s>|]+/account/link\?token=([A-Za-z0-9_.-]+)`)

func TestSlackMembersLinkTheirOwnAccountForMemory(t *testing.T) {
	f := newSlackLinkFixture(t)
	app := f.app

	f.dm(t, "before", "What do I usually check on Mondays?")
	if len(f.requests.prompts) != 1 || strings.Contains(f.requests.prompts[0], "Denver office rollout") || strings.Contains(strings.Join(f.requests.tools[0], ","), "remember") {
		t.Fatalf("an unlinked member reached someone's memory: %v\n%v", f.requests.tools, f.requests.prompts)
	}
	if !strings.Contains(f.requests.prompts[0], "`link`") {
		t.Fatalf("an unlinked member is not told how to link memory:\n%s", f.requests.prompts[0])
	}

	reply := f.dm(t, "link", "link")
	match := slackLinkURL.FindStringSubmatch(reply)
	if match == nil || !strings.HasPrefix(reply, "DNEW: ") || len(f.requests.prompts) != 1 {
		t.Fatalf("link did not answer with a sign-in link and without the model: %q", reply)
	}
	token := match[1]
	runs, err := app.automations.Queue.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if raw, _ := json.Marshal(r); strings.Contains(string(raw), token) {
			t.Fatal("the link token was stored with the run")
		}
	}

	page := f.browser("alice", "GET", "/account/link?token="+token, nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "UNEW") || !strings.Contains(page.Body.String(), `action="/account/link"`) {
		t.Fatalf("confirmation page: %d %s", page.Code, page.Body.String())
	}
	if app.catalog.MessageLink("slack", "T123", "UNEW") != "" {
		t.Fatal("viewing the page linked the account")
	}
	w := f.browser("alice", "POST", "/account/link", url.Values{"token": {token}})
	if w.Code != 303 || app.catalog.MessageLink("slack", "T123", "UNEW") != "alice" {
		t.Fatalf("confirm did not link alice: %d %s", w.Code, w.Body.String())
	}
	if last := f.replies[len(f.replies)-1]; !strings.HasPrefix(last, "DNEW: ") || !strings.Contains(last, "unlink") {
		t.Fatalf("the Slack user was not told about the link: %q", last)
	}
	if links := app.catalog.MessageLinks("alice"); len(links) != 1 || links[0].User != "UNEW" || links[0].Workspace != "T123" {
		t.Fatalf("alice's links: %+v", links)
	}
	if w = f.browser("bob", "POST", "/account/link", url.Values{"token": {token}}); w.Code < 400 || app.catalog.MessageLink("slack", "T123", "UNEW") != "alice" {
		t.Fatalf("a second account took over the link: %d", w.Code)
	}

	f.requests.remember = `{"items":[{"text":"Alice reviews service health every Monday.","kind":"fact"}]}`
	f.requests.prompts, f.requests.tools = nil, nil
	f.dm(t, "after", "What do I usually check on Mondays?")
	if len(f.requests.prompts) != 2 || !strings.Contains(f.requests.prompts[0], "Alice owns the Denver office rollout.") || !strings.Contains(strings.Join(f.requests.tools[0], ","), "remember") {
		t.Fatalf("a linked member's DM did not use their memory: %v\n%v", f.requests.tools, f.requests.prompts)
	}
	settings, _ := app.catalog.Assistant("alice")
	store, err := app.catalog.Access("alice", settings.Project, "read")
	if err != nil {
		t.Fatal(err)
	}
	file, err := agentmemory.Read(t.Context(), store, "")
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, item := range file.Items {
		kept = kept || item.Text == "Alice reviews service health every Monday." && item.Via == "slack"
	}
	if !kept {
		t.Fatalf("the linked DM did not remember as alice: %+v", file.Items)
	}

	if reply = f.dm(t, "unlink", "unlink"); app.catalog.MessageLink("slack", "T123", "UNEW") != "" || !strings.Contains(reply, "nlinked") {
		t.Fatalf("unlink from Slack: %q", reply)
	}
	f.requests.remember, f.requests.prompts, f.requests.tools = "", nil, nil
	f.dm(t, "unlinked", "What do I usually check on Mondays?")
	if strings.Contains(f.requests.prompts[0], "Denver office rollout") {
		t.Fatal("memory reached the DM after unlink")
	}
}

func TestSlackLinkRefusesForgedExpiredAndRemovedLinks(t *testing.T) {
	f := newSlackLinkFixture(t)
	app := f.app
	d := messaging.Address{Client: "slack", Workspace: "T123", Channel: "DNEW", User: "UNEW", Direct: true}
	valid := app.messageLinkToken(d, time.Now().Add(time.Minute))
	forged := strings.Replace(valid, "UNEW", "UOTHER", 1)
	if forged == valid {
		payload, signature, _ := strings.Cut(valid, ".")
		forged = payload[:len(payload)-2] + "xx." + signature
	}
	for name, token := range map[string]string{
		"forged":  forged,
		"expired": app.messageLinkToken(d, time.Now().Add(-time.Minute)),
		"other":   app.sign("x") + "." + app.sign("y"),
	} {
		if w := f.browser("alice", "POST", "/account/link", url.Values{"token": {token}}); w.Code < 400 {
			t.Fatalf("%s token linked: %d", name, w.Code)
		}
		if w := f.browser("alice", "GET", "/account/link?token="+url.QueryEscape(token), nil); w.Code < 400 {
			t.Fatalf("%s token showed a confirmation page: %d", name, w.Code)
		}
	}
	if app.catalog.MessageLink("slack", "T123", "UNEW") != "" {
		t.Fatal("a refused token linked the account")
	}

	if w := f.browser("alice", "POST", "/account/link", url.Values{"token": {valid}}); w.Code != 303 {
		t.Fatalf("valid token: %d %s", w.Code, w.Body.String())
	}
	status := proxyCall(app, "alice", "POST", "/api/assistant/action/assistant_status", `{}`)
	if !strings.Contains(status.Body.String(), `"links":[{`) || !strings.Contains(status.Body.String(), "UNEW") {
		t.Fatalf("assistant status does not list the link: %s", status.Body.String())
	}
	remove := url.Values{"client": {"slack"}, "workspace": {"T123"}, "user": {"UNEW"}}
	if w := f.browser("bob", "POST", "/account/link/remove", remove); app.catalog.MessageLink("slack", "T123", "UNEW") != "alice" {
		t.Fatalf("bob removed alice's link: %d", w.Code)
	}
	if w := f.browser("alice", "POST", "/account/link/remove", remove); w.Code != 303 || app.catalog.MessageLink("slack", "T123", "UNEW") != "" {
		t.Fatalf("alice could not remove her link: %d %s", w.Code, w.Body.String())
	}
}

func TestSlackLinkedMemoryIsReviewedAsThePerson(t *testing.T) {
	f := newSlackLinkFixture(t)
	app := f.app
	if err := app.catalog.LinkMessageUser("alice", "slack", "T123", "UNEW"); err != nil {
		t.Fatal(err)
	}
	settings, _ := app.catalog.Assistant("alice")
	store, err := app.catalog.Access("alice", settings.Project, "owner")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := agentmemory.Observation{Text: "Alice reviews service health every Monday.", Kind: "fact"}.Item(false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.memory.Write(t.Context(), "alice", settings.Project, store, "", []agentmemory.Item{seed}, time.Now(), func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	file, err := agentmemory.Read(t.Context(), store, "")
	if err != nil || len(file.Items) != 2 {
		t.Fatal(file.Items, err)
	}
	old := file.Items[1].ID
	calls := 0
	configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var in struct {
			Questions map[string]struct {
				Criteria map[string]string `json:"criteria"`
			} `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		answers := map[string]any{}
		for name, question := range in.Questions {
			probabilities := map[string]float64{}
			for option := range question.Criteria {
				probabilities[option] = 0
			}
			probabilities[old] = 1
			answers[name] = map[string]any{"type": "choice", "choice": old, "confidence": 1, "probabilities": probabilities}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 40, "output_tokens": 4}})
	})
	if w := proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":true}`); w.Code != 200 {
		t.Fatalf("Jev consent: %d %s", w.Code, w.Body.String())
	}
	f.requests.remember = `{"items":[{"text":"Alice reviews service health every Tuesday now.","kind":"fact"}]}`
	f.dm(t, "correction", "I moved service health reviews to Tuesdays.")
	if file, err = agentmemory.Read(t.Context(), store, ""); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(file.Items) != 2 || file.Items[1].Text != "Alice reviews service health every Tuesday now." || len(file.Changed) != 1 {
		t.Fatalf("the linked DM's correction was not reviewed as alice: calls=%d %+v", calls, file)
	}
}

func TestASlackAnswerChecksItsMemberWithSlackOnce(t *testing.T) {
	f := newSlackLinkFixture(t)
	f.dm(t, "first", "What do I usually check on Mondays?")
	if f.userInfos != 1 {
		t.Fatalf("one answer asked Slack about its member %d times", f.userInfos)
	}
	// Every message is checked again, so a member Slack has since
	// deactivated is refused.
	f.dm(t, "second", "And on Tuesdays?")
	if f.userInfos != 2 {
		t.Fatalf("two answers asked Slack about their member %d times", f.userInfos)
	}
}
