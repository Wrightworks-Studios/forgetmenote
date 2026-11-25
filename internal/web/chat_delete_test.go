package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatDeleteHTTPPrivateAndIdempotent(t *testing.T) {
	app := chatFixture(t)
	w := proxyCall(app, "alice", "POST", "/p/default/api/chat", `{"connection":"local","model":"synthetic","mode":"ask"}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var c chat.Conversation
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	path := "/p/default/api/chat/" + c.ID
	if err := app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Title = "private deletion title"
		c.Turns = []chat.Turn{{ID: "private-turn", Role: "user", Content: "private deletion payload", Attachments: []chat.Attachment{{Kind: "private", Content: "private attachment"}}}}
		c.Memory.Text = "private compact memory"
		c.Runs = []chat.Run{{ID: "used-run", State: "completed", Options: chat.Options{Connection: "local"}, CreatedAt: chat.Now(), Usage: chat.Accounting{InputTokens: 91, OutputTokens: 13, UncertainTokens: 7, Requests: 2}}}
		c.AddEvent("used-run", "text_delta", map[string]string{"text": "private event"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := app.chat.Store.PersonalUsage("alice")
	if err != nil {
		t.Fatal(err)
	}
	if w = proxyCall(app, "bob", "DELETE", path, ""); w.Code != 404 {
		t.Fatalf("other owner delete: %d %s", w.Code, w.Body)
	}
	for _, id := range []string{"invalid", "..%2foutside", strings.Repeat("a", 33)} {
		if w = proxyCall(app, "alice", "DELETE", "/p/default/api/chat/"+id, ""); w.Code != 404 {
			t.Fatalf("malformed id %q: %d", id, w.Code)
		}
	}
	for i := 0; i < 2; i++ {
		if w = proxyCall(app, "alice", "DELETE", path, ""); w.Code != http.StatusOK {
			t.Fatalf("delete/retry: %d %s", w.Code, w.Body)
		}
	}
	if w = call(app, "DELETE", path, "", testToken); w.Code != 403 {
		t.Fatalf("bootstrap delete: %d %s", w.Code, w.Body)
	}
	for _, suffix := range []string{"", "/events?after=0"} {
		if w = proxyCall(app, "alice", "GET", path+suffix, ""); w.Code != 404 {
			t.Fatalf("deleted read: %d %s", w.Code, w.Body)
		}
	}
	if w = proxyCall(app, "alice", "GET", "/p/default/api/chat", ""); w.Code != 200 || strings.Contains(w.Body.String(), c.ID) {
		t.Fatalf("deleted listed: %d %s", w.Code, w.Body)
	}
	after, err := app.chat.Store.PersonalUsage("alice")
	if err != nil || after != before {
		t.Fatalf("usage reset: before=%+v after=%+v err=%v", before, after, err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", "alice", ""); err != nil {
		t.Fatal(err)
	}
	if w = proxyCall(app, "alice", "DELETE", path, ""); w.Code != 403 {
		t.Fatalf("revoked retry: %d %s", w.Code, w.Body)
	}
}

func TestChatDeleteRejectsActiveProviderAndLateReply(t *testing.T) {
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"late","type":"function","function":{"name":"save_note","arguments":"{\"path\":\"resurrected.md\",\"content\":\"Late deleted chat\",\"revision\":\"\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":80,"completion_tokens":20}}`)
		close(finished)
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	notebook, err := app.catalog.Access("alice", "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	beforeGit, err := notebook.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path := "/p/default/api/chat/" + c.ID
	body := `{"submissionId":"delete-race","text":"Wait for a reply","options":{"mode":"edit","actions":["notes"]}}`
	if w := proxyCall(app, "alice", "POST", path+"/turns", body); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("provider not called")
	}
	if w := proxyCall(app, "alice", "DELETE", path, ""); w.Code != 409 || !strings.Contains(w.Body.String(), "stop") {
		t.Fatalf("active delete: %d %s", w.Code, w.Body)
	}
	current, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	runID := current.Runs[0].ID
	if w := proxyCall(app, "alice", "POST", path+"/runs/"+runID+"/cancel", `{}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	waitChatRun(t, app, c.ID)
	app.chat.Wait()
	usage, err := app.chat.Store.PersonalUsage("alice")
	if err != nil || usage.Total.UncertainTokens <= 0 {
		t.Fatalf("cancelled request cost lost: %+v %v", usage, err)
	}
	if w := proxyCall(app, "alice", "DELETE", path, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	close(release)
	released = true
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("late provider did not finish")
	}
	for _, suffix := range []string{"/turns", "/runs/" + runID + "/resume", "/runs/" + runID + "/cancel", "/save-note"} {
		if w := proxyCall(app, "alice", "POST", path+suffix, body); w.Code != 404 {
			t.Fatalf("deleted mutation %s: %d %s", suffix, w.Code, w.Body)
		}
	}
	afterGit, err := notebook.Status(context.Background())
	if err != nil || beforeGit.Revision != afterGit.Revision {
		t.Fatalf("late reply mutated Git: %v", err)
	}
	app.chat.Store.Close()
	app.chat.Store, err = chat.OpenStore(app.db)
	if err != nil {
		t.Fatal(err)
	}
	after, err := app.chat.Store.PersonalUsage("alice")
	if err != nil || after != usage {
		t.Fatalf("restart lost usage: %+v %+v %v", usage, after, err)
	}
	if w := proxyCall(app, "alice", "GET", path, ""); w.Code != 404 {
		t.Fatalf("restart resurrected chat: %d", w.Code)
	}
	if w := proxyCall(app, "alice", "DELETE", path, ""); w.Code != 200 {
		t.Fatalf("restart delete retry: %d %s", w.Code, w.Body)
	}
}

func TestChatDeleteStopsOpenSSEAndRejectsOtherNotebook(t *testing.T) {
	app := chatFixture(t)
	c, err := app.chat.Store.Create("alice", "default", chat.Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Create(catalog.Bootstrap, "other", "Other"); err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "other", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Create(catalog.Bootstrap, "deleted:default", "Reserved"); err == nil {
		t.Fatal("receipt namespace accepted as a notebook")
	}
	if w := proxyCall(app, "alice", "DELETE", "/p/other/api/chat/"+c.ID, ""); w.Code != 404 {
		t.Fatalf("cross notebook delete: %d %s", w.Code, w.Body)
	}
	if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.AddEvent("run", "text_delta", map[string]string{"text": "existing replay"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	defer srv.Close()
	app.origin = srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/p/default/api/chat/"+c.ID+"/events", nil)
	request.Header.Set("Remote-User", "alice")
	request.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
	response, err := srv.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.Status)
	}
	scanner := bufio.NewScanner(response.Body)
	found := false
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data:") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("missing initial replay")
	}
	// Receipt of the frame can precede the server finishing its flush. A busy
	// response grants no work; this client explicitly retries that bounded case.
	deleted := false
	for attempt := 0; attempt < 100; attempt++ {
		w := proxyCall(app, "alice", "DELETE", "/p/default/api/chat/"+c.ID, "")
		if w.Code == 200 {
			deleted = true
			break
		}
		if w.Code != 409 || !strings.Contains(w.Body.String(), "busy") {
			t.Fatal(w.Code, w.Body)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !deleted {
		t.Fatal("SSE frame never released the conversation")
	}
	ended := make(chan struct{})
	go func() {
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "data:") {
				t.Error("post-delete replay")
			}
		}
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("deleted SSE stayed open")
	}
	if w := proxyCall(app, "alice", "GET", "/p/default/api/chat/"+c.ID+"/events", ""); w.Code != 404 {
		t.Fatalf("deleted reconnect: %d", w.Code)
	}
}

type heldChatRead struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
}

func (w *heldChatRead) Write(body []byte) (int, error) {
	close(w.entered)
	<-w.release
	return w.ResponseRecorder.Write(body)
}
func TestChatDeleteBusyReaderDoesNotBlockUnrelatedConversations(t *testing.T) {
	app := chatFixture(t)
	alice, err := app.chat.Store.Create("alice", "default", chat.Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := app.chat.Store.Create("alice", "default", chat.Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := app.chat.Store.Create("bob", "default", chat.Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", app.origin+"/p/default/api/chat/"+alice.ID, nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Remote-User", "alice")
	request.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
	writer := &heldChatRead{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	ended := make(chan struct{})
	go func() { app.ServeHTTP(writer, request); close(ended) }()
	defer func() { close(writer.release); <-ended }()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("reader did not start")
	}
	for _, item := range []struct {
		user, id string
		code     int
	}{{"alice", alice.ID, 409}, {"alice", other.ID, 200}, {"bob", bob.ID, 200}} {
		result := make(chan *httptest.ResponseRecorder, 1)
		go func() { result <- proxyCall(app, item.user, "DELETE", "/p/default/api/chat/"+item.id, "") }()
		select {
		case w := <-result:
			if w.Code != item.code {
				t.Fatalf("delete while unrelated reader waits: %d %s", w.Code, w.Body)
			}
		case <-time.After(time.Second):
			t.Fatal("slow reader blocked deletion")
		}
	}
}
