package web

import (
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
		if at.Sub(revoked) > chatAccessInterval+chatAccessInterval/2 {
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
