package web

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/webcore"
)

// A legacy session's stream that cannot watch a notebook it subscribed to,
// because the notebook has as many watchers as it allows, is told so with a
// typed, retryable capacity answer instead of opening and staying silent.
func TestLegacyStreamOverTheWatchLimitIsTold(t *testing.T) {
	app, store := fixture(t)
	initialized := legacyRequest(app, testToken, "", legacyInitialize)
	id := initialized.Header().Get("Mcp-Session-Id")
	if initialized.Code != 200 || id == "" {
		t.Fatalf("initialize: %d %s", initialized.Code, initialized.Body)
	}
	legacyRequest(app, testToken, id, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if w := legacyRequest(app, testToken, id, `{"jsonrpc":"2.0","id":2,"method":"resources/subscribe","params":{"uri":"`+revisionURI("default")+`"}}`); w.Code != 200 || strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body)
	}
	for {
		_, cancel, err := store.Watch()
		if err != nil {
			break
		}
		t.Cleanup(cancel)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r := httptest.NewRequestWithContext(ctx, "GET", app.origin+"/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Accept", "text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2025-11-25")
	r.Header.Set("Mcp-Session-Id", id)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), `"capacity"`) {
		t.Fatalf("a stream that cannot watch its subscription: HTTP %d %s", w.Code, w.Body)
	}
}

// A legacy stream that is open when its session subscribes, through any
// gateway, to a notebook it cannot watch hears why and ends.
func TestOpenLegacyStreamOverTheWatchLimitIsTold(t *testing.T) {
	app, _ := fixture(t)
	if err := app.catalog.Create(catalog.Bootstrap, "other", "Other"); err != nil {
		t.Fatal(err)
	}
	other, err := app.catalog.Access(catalog.Bootstrap, "other", "read")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app)
	defer server.Close()
	app.origin = server.URL
	initialized := legacyRequest(app, testToken, "", legacyInitialize)
	id := initialized.Header().Get("Mcp-Session-Id")
	legacyRequest(app, testToken, id, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	legacyRequest(app, testToken, id, `{"jsonrpc":"2.0","id":2,"method":"resources/subscribe","params":{"uri":"`+revisionURI("default")+`"}}`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Accept", "text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2025-11-25")
	r.Header.Set("Mcp-Session-Id", id)
	response, err := http.DefaultClient.Do(r)
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("stream: %v %v", response, err)
	}
	defer response.Body.Close()
	waitWatch(t, app, 1)
	for {
		_, stop, err := other.Watch()
		if err != nil {
			break
		}
		t.Cleanup(stop)
	}
	legacyRequest(app, testToken, id, `{"jsonrpc":"2.0","id":3,"method":"resources/subscribe","params":{"uri":"`+revisionURI("other")+`"}}`)
	lines := bufio.NewScanner(response.Body)
	told := false
	for lines.Scan() {
		if strings.Contains(lines.Text(), `"notifications/message"`) && strings.Contains(lines.Text(), `"capacity"`) {
			told = true
		}
	}
	if !told {
		t.Fatalf("the stream ended without saying why: %v", lines.Err())
	}
}

// The subscription hub's limits are capacity refusals, retryable like every
// other capacity limit.
func TestSubscriptionLimitsAreCapacity(t *testing.T) {
	for _, err := range []error{errSubscriptionSessions, errMCPSessionFull} {
		if detail := webcore.ErrorOf(err, 500); detail.Status != http.StatusTooManyRequests || detail.Code != "capacity" || !detail.Retryable() {
			t.Errorf("%v: %+v", err, detail)
		}
	}
}
