package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/chatrun"
)

// A revoked browser session stops a streaming answer within the access
// interval, while the tokens streamed before it no longer each cost an
// authorization query.
func TestChatRevokedSessionStopsStreamWithinAccessInterval(t *testing.T) {
	app := chatFixture(t)
	var tokens atomic.Int64
	started := make(chan struct{})
	var once sync.Once
	stopped := make(chan time.Time, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < 4000; i++ {
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"token \"},\"finish_reason\":null}]}\n\n")
			flusher.Flush()
			tokens.Add(1)
			if i == 40 {
				once.Do(func() { close(started) })
			}
			select {
			case <-r.Context().Done():
				stopped <- time.Now()
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "chat.json")
	raw, _ := json.Marshal(map[string]any{"connections": []any{map[string]any{"id": "local", "label": "Synthetic", "format": "chat-completions", "baseURL": srv.URL, "models": []string{"synthetic"}, "local": true, "capabilities": map[string]bool{"tools": true, "stream": true}, "limits": map[string]int{"runSeconds": 60}}}})
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureChat(file); err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRecorder()
	if err := app.createSession(login, "alice"); err != nil {
		t.Fatal(err)
	}
	session := login.Result().Cookies()[0]
	browser := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, app.origin+path, strings.NewReader(body))
		r.Header.Set("Origin", app.origin)
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(session)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		return w
	}
	created := browser("POST", "/p/default/api/chat", `{"connection":"local","model":"synthetic","mode":"ask"}`)
	var c chat.Conversation
	if json.Unmarshal(created.Body.Bytes(), &c) != nil || c.ID == "" {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	// Background workers query the database too; compare with an idle second.
	idleStart := app.db.Pool.Stat().AcquireCount()
	time.Sleep(time.Second)
	idle := app.db.Pool.Stat().AcquireCount() - idleStart
	if w := browser("POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"stream-1","text":"Write at length.","options":{"mode":"ask"}}`); w.Code != 200 {
		t.Fatalf("turn: %d %s", w.Code, w.Body)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not start")
	}
	streamedStart, queriesStart := tokens.Load(), app.db.Pool.Stat().AcquireCount()
	time.Sleep(time.Second)
	streamed, queries := tokens.Load()-streamedStart, app.db.Pool.Stat().AcquireCount()-queriesStart
	t.Logf("one second: %d tokens streamed, %d pool acquisitions while streaming, %d idle", streamed, queries, idle)
	if streamed < 40 || queries-idle > streamed/4 {
		t.Fatalf("authorization cost still scales with tokens: %d tokens, %d acquisitions, %d idle", streamed, queries, idle)
	}
	revoked := time.Now()
	if err := app.catalog.Revoke(catalog.Bootstrap, "alice"); err != nil {
		t.Fatal(err)
	}
	select {
	case at := <-stopped:
		t.Logf("stream stopped %s after revocation", at.Sub(revoked))
		if at.Sub(revoked) > chatrun.AccessInterval+chatrun.AccessInterval/2 {
			t.Fatalf("revoked stream ran for %s", at.Sub(revoked))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revoked session kept streaming")
	}
	got := waitChatRun(t, app, c.ID)
	if run := got.Runs[0]; run.State != "failed" || !strings.Contains(run.Error, "revoked") {
		t.Fatalf("revoked run %s %q", run.State, run.Error)
	}
	for _, turn := range got.Turns {
		if turn.Role == "assistant" {
			t.Fatal("revoked stream saved an assistant answer")
		}
	}
}

// Signing one browser session out ends that session's open chat event stream
// within a poll, though the account and its notebook access are unchanged:
// another session of the account still opens the stream.
func TestChatSignedOutSessionEndsItsEventStream(t *testing.T) {
	app := chatFixture(t)
	c, err := app.chat.Store.Create("alice", "default", chat.Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.AddEvent("run", "progress", map[string]string{"message": "first"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	defer srv.Close()
	open := func(ctx context.Context, session *http.Cookie) *http.Response {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/p/default/api/chat/"+c.ID+"/events?after=0", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Host = app.originHost()
		r.AddCookie(session)
		resp, err := srv.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("event stream: %s", resp.Status)
		}
		return resp
	}
	session := signIn(t, app)
	resp := open(t.Context(), session)
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() && !strings.HasPrefix(scanner.Text(), "data:") {
	}
	signOut(t, app, session)
	ended := make(chan struct{})
	go func() {
		for scanner.Scan() {
		}
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("the signed-out session's event stream stayed open")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	other := open(ctx, signIn(t, app))
	other.Body.Close()
}

// signIn signs alice in and returns the session cookie.
func signIn(t *testing.T, app *Server) *http.Cookie {
	t.Helper()
	login := httptest.NewRecorder()
	if err := app.createSession(login, "alice"); err != nil {
		t.Fatal(err)
	}
	return login.Result().Cookies()[0]
}

// signOut signs one browser session out, as its page's Sign out does.
func signOut(t *testing.T, app *Server, session *http.Cookie) {
	t.Helper()
	r := httptest.NewRequest("POST", app.origin+"/logout", nil)
	r.Header.Set("Origin", app.origin)
	r.AddCookie(session)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("sign out: %d %s", w.Code, w.Body)
	}
}
