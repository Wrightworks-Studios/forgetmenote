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

func TestChatCancelAndReplaySafeResume(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var in any
		json.NewDecoder(r.Body).Decode(&in)
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
			return
		case <-release:
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Resumed answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":4}}`)
	})
	defer close(release)
	path := "/p/default/api/chat/" + c.ID
	w := proxyCall(app, "alice", "POST", path+"/turns", `{"submissionId":"cancel-me","text":"Wait for an answer","options":{"mode":"ask"}}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	var start struct{ Run string }
	json.Unmarshal(w.Body.Bytes(), &start)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	w = proxyCall(app, "alice", "POST", path+"/runs/"+start.Run+"/cancel", `{}`)
	if w.Code != 200 {
		t.Fatalf("cancel unavailable: %d %s", w.Code, w.Body)
	}
	done := waitChatRun(t, app, c.ID)
	if done.Runs[0].State != "cancelled" {
		t.Fatalf("not cancelled: %+v", done.Runs)
	}
}
func TestChatExplicitSaveToNote(t *testing.T) {
	app := chatFixture(t)
	if e := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); e != nil {
		t.Fatal(e)
	}
	c, e := app.chat.Store.Create("alice", "default", chat.Options{Mode: "ask"})
	if e != nil {
		t.Fatal(e)
	}
	e = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Turns = append(c.Turns, chat.Turn{ID: "answer", Role: "assistant", Content: "Evidence worth saving."})
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	body := `{"submissionId":"save-once","path":"notes/chat.md","revision":"","turnIds":["answer"]}`
	path := "/p/default/api/chat/" + c.ID + "/save-note"
	w := proxyCall(app, "alice", "POST", path, body)
	if w.Code != 200 {
		t.Fatalf("save chat unavailable: %d %s", w.Code, w.Body)
	}
	store, _ := app.catalog.Access("alice", "default", "read")
	before, _ := store.Status(context.Background())
	w = proxyCall(app, "alice", "POST", path, body)
	if w.Code != 200 {
		t.Fatalf("save replay %d %s", w.Code, w.Body)
	}
	after, _ := store.Status(context.Background())
	if before.Revision != after.Revision {
		t.Fatal("save replay created duplicate commit")
	}
	note, e := store.Read(context.Background(), "notes/chat.md", "")
	if e != nil || !strings.Contains(string(note.Content), "Evidence worth saving.") {
		t.Fatal("transcript not saved")
	}
}
func TestChatSSEReconnectAndRevocation(t *testing.T) {
	app := chatFixture(t)
	c, e := app.chat.Store.Create("alice", "default", chat.Options{Mode: "ask"})
	if e != nil {
		t.Fatal(e)
	}
	app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.AddEvent("run", "text_delta", map[string]string{"text": "first"})
		c.AddEvent("run", "text_delta", map[string]string{"text": "second"})
		return nil
	})
	srv := httptest.NewServer(app)
	defer srv.Close()
	app.origin = srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/p/default/api/chat/"+c.ID+"/events?after=1", nil)
	req.Header.Set("Remote-User", "alice")
	req.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
	resp, e := srv.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	scanner := bufio.NewScanner(resp.Body)
	found := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			if strings.Contains(line, "first") || !strings.Contains(line, "second") {
				t.Fatalf("wrong replay %s", line)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("replay missing")
	}
	if e = app.catalog.Member(catalog.Bootstrap, "default", "alice", ""); e != nil {
		t.Fatal(e)
	}
	end := make(chan struct{})
	go func() {
		for scanner.Scan() {
		}
		close(end)
	}()
	select {
	case <-end:
	case <-time.After(time.Second):
		t.Fatal("revoked stream remained open")
	}
}

func TestChatSSEBoundsConcurrentSubscribers(t *testing.T) {
	app := chatFixture(t)
	c, err := app.chat.Store.Create("alice", "default", chat.Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.AddEvent("run", "progress", map[string]string{"message": "synthetic"})
		return nil
	})
	server := httptest.NewServer(app)
	defer server.Close()
	app.origin = server.URL
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	responses := []*http.Response{}
	defer func() {
		for _, response := range responses {
			response.Body.Close()
		}
	}()
	for i := 0; i < 9; i++ {
		request, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/p/default/api/chat/"+c.ID+"/events", nil)
		request.Header.Set("Remote-User", "alice")
		request.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, response)
		if i < 8 && response.StatusCode != 200 {
			t.Fatalf("legitimate subscriber rejected: %d", response.StatusCode)
		}
		if i == 8 && response.StatusCode != 429 {
			t.Fatalf("unbounded chat subscribers admitted: %d", response.StatusCode)
		}
	}
}
