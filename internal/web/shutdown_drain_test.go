package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
)

// A server that begins to drain, as it does when it shuts down, ends at
// once the requests that wait for changes, whose clients reconnect to the
// next process: an open page's live poll answers as one that timed out,
// its chat events end, a legacy MCP session's stream ends asking its
// client to reconnect promptly, and a current client's subscriptions/listen
// stream ends with its result. A request that does not wait is unaffected.
func TestWaitingRequestsEndWhenTheServerDrains(t *testing.T) {
	app := chatFixture(t)
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(t.Context(), "live.md", []byte("alpha\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	c, err := app.chat.Store.Create("alice", "default", chat.Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.AddEvent("run", "text_delta", map[string]string{"text": "first"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	drain, beginDrain := context.WithCancel(context.Background())
	defer beginDrain()
	server := httptest.NewUnstartedServer(app)
	server.Config.BaseContext = func(net.Listener) context.Context { return webcore.WithDrain(context.Background(), drain) }
	server.Start()
	defer server.Close()
	app.origin = server.URL
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	type ended struct {
		name, body string
		status     int
		at         time.Time
	}
	endings := make(chan ended, 4)
	// open sends request, reads its stream until it holds opened, and then
	// reads the rest until it ends.
	open := func(name string, request *http.Request, opened string) {
		t.Helper()
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if response.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(response.Body)
			t.Fatalf("%s: HTTP %d %s", name, response.StatusCode, raw)
		}
		var body strings.Builder
		buffer := make([]byte, 4096)
		for opened != "" && !strings.Contains(body.String(), opened) {
			n, err := response.Body.Read(buffer)
			body.Write(buffer[:n])
			if err != nil {
				t.Fatalf("%s ended before it opened: %v %s", name, err, body.String())
			}
		}
		go func() {
			rest, _ := io.ReadAll(response.Body)
			response.Body.Close()
			endings <- ended{name, body.String() + string(rest), response.StatusCode, time.Now()}
		}()
	}
	browser := func(path string) *http.Request {
		request, _ := http.NewRequestWithContext(ctx, "GET", server.URL+path, nil)
		request.Header.Set("Remote-User", "alice")
		request.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
		return request
	}

	read := func(path string) (int, []byte) {
		t.Helper()
		request, _ := http.NewRequestWithContext(ctx, "GET", server.URL+path, nil)
		request.Header.Set("Authorization", "Bearer "+testToken)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		return response.StatusCode, raw
	}

	var base workspace.LiveDocument
	if status, raw := read("/p/default/api/live?path=live.md"); status != 200 || json.Unmarshal(raw, &base) != nil {
		t.Fatalf("live read: HTTP %d %s", status, raw)
	}
	since := fmt.Sprintf("%s/%s/%t", base.LiveRevision, base.Version, base.Pending)
	poll, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/p/default/api/live?path=live.md&wait=25000&since="+url.QueryEscape(since), nil)
	poll.Header.Set("Authorization", "Bearer "+testToken)
	go func() {
		// A long poll's answer starts only when it ends.
		response, err := http.DefaultClient.Do(poll)
		if err != nil {
			endings <- ended{"the live poll", err.Error(), 0, time.Now()}
			return
		}
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		endings <- ended{"the live poll", string(raw), response.StatusCode, time.Now()}
	}()

	open("the chat events", browser("/p/default/api/chat/"+c.ID+"/events?after=0"), "first")

	initialized := legacyRequest(app, testToken, "", legacyInitialize)
	id := initialized.Header().Get("Mcp-Session-Id")
	if initialized.Code != 200 || id == "" {
		t.Fatalf("initialize: %d %s", initialized.Code, initialized.Body)
	}
	legacyRequest(app, testToken, id, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if w := legacyRequest(app, testToken, id, `{"jsonrpc":"2.0","id":2,"method":"resources/subscribe","params":{"uri":"`+revisionURI("default")+`"}}`); w.Code != 200 || strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body)
	}
	legacy, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/mcp", nil)
	legacy.Header.Set("Authorization", "Bearer "+testToken)
	legacy.Header.Set("Accept", "text/event-stream")
	legacy.Header.Set("MCP-Protocol-Version", "2025-11-25")
	legacy.Header.Set("Mcp-Session-Id", id)
	open("the legacy MCP stream", legacy, "")

	listen, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":12,"method":"subscriptions/listen","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"synthetic-stream","version":"1"}},"notifications":{"resourceSubscriptions":["`+revisionURI("default")+`"]}}}`))
	listen.Header.Set("Authorization", "Bearer "+testToken)
	listen.Header.Set("MCP-Protocol-Version", "2026-07-28")
	listen.Header.Set("Mcp-Method", "subscriptions/listen")
	listen.Header.Set("Content-Type", "application/json")
	listen.Header.Set("Accept", "application/json, text/event-stream")
	open("the MCP subscriptions/listen stream", listen, "subscriptions/acknowledged")

	select {
	case early := <-endings:
		t.Fatalf("%s ended before the drain: HTTP %d %s", early.name, early.status, early.body)
	case <-time.After(300 * time.Millisecond):
	}
	drained := time.Now()
	beginDrain()
	for range 4 {
		select {
		case e := <-endings:
			if took := e.at.Sub(drained); took > 2*time.Second {
				t.Errorf("%s ended %v after the drain began", e.name, took)
			}
			switch e.name {
			case "the live poll":
				var answer workspace.LiveDocument
				if err := json.Unmarshal([]byte(e.body), &answer); e.status != 200 || err != nil || !answer.Unchanged {
					t.Errorf("the drained live poll answered HTTP %d %s", e.status, e.body)
				}
			case "the legacy MCP stream":
				if !strings.Contains(e.body, "retry: 1000") {
					t.Errorf("the drained legacy stream did not ask its client to reconnect promptly: %s", e.body)
				}
			case "the MCP subscriptions/listen stream":
				if !strings.Contains(e.body, `"id":12,"result"`) {
					t.Errorf("the drained listen stream ended without its result: %s", e.body)
				}
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a waiting request outlived the drain")
		}
	}
	// A request that does not wait for changes is not ended by the drain.
	var now workspace.LiveDocument
	if status, raw := read("/p/default/api/live?path=live.md"); status != 200 || json.Unmarshal(raw, &now) != nil || now.Content != "alpha\n" {
		t.Errorf("a live read during the drain: HTTP %d %s", status, raw)
	}
}
