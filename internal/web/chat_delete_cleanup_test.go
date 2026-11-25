package web

import (
	"context"
	"errors"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type deletionReplayGate struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
}

func (w *deletionReplayGate) SetWriteDeadline(d time.Time) error {
	if d.IsZero() {
		close(w.entered)
		<-w.release
	}
	return nil
}
func TestChatDeleteCleanupFailureFencesCachedReplay(t *testing.T) {
	app := chatFixture(t)
	c, err := app.chat.Store.Create("alice", "default", chat.Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.AddEvent("run", "text_delta", map[string]string{"text": "PRIVATE CACHED REPLAY"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Refuse the cleanup that follows the receipt: a write to this instance's
	// conversations that leaves the version unchanged.
	fault := "chat_cleanup_fault_" + strings.ToLower(app.db.Org)
	if _, err = app.db.Pool.Exec(t.Context(), `CREATE FUNCTION `+fault+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected cleanup failure'; END $$;
		CREATE TRIGGER `+fault+` BEFORE UPDATE ON chat_conversations FOR EACH ROW WHEN (NEW.org = '`+app.db.Org+`' AND NEW.version = OLD.version) EXECUTE FUNCTION `+fault+`()`); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", app.origin+"/p/default/api/chat/"+c.ID+"/events", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Remote-User", "alice")
	request.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	request = request.WithContext(ctx)
	writer := &deletionReplayGate{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	ended := make(chan struct{})
	go func() { app.ServeHTTP(writer, request); close(ended) }()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("cached stream did not enter gate")
	}
	result := proxyCall(app, "alice", "DELETE", "/p/default/api/chat/"+c.ID, "")
	t.Logf("delete status after receipt and cleanup failure: %d %s", result.Code, result.Body)
	if result.Code != 500 {
		close(writer.release)
		<-ended
		t.Fatalf("expected injected cleanup error, got %d", result.Code)
	}
	if _, err = app.chat.Store.Read("alice", "default", c.ID); !errors.Is(err, chat.ErrNotFound) {
		close(writer.release)
		<-ended
		t.Fatalf("receipt did not retire original path: %v", err)
	}
	close(writer.release)
	<-ended
	if strings.Contains(writer.Body.String(), "PRIVATE CACHED REPLAY") {
		t.Errorf("cached private transcript was emitted after durable deletion receipt: %s", writer.Body)
	}
	if _, err = app.db.Pool.Exec(t.Context(), `DROP TRIGGER `+fault+` ON chat_conversations; DROP FUNCTION `+fault+`()`); err != nil {
		t.Fatal(err)
	}
	retry := proxyCall(app, "alice", "DELETE", "/p/default/api/chat/"+c.ID, "")
	if retry.Code != 200 {
		t.Fatalf("retry after cleanup fault resolved: %d %s", retry.Code, retry.Body)
	}
}

func TestChatDeleteWrongNotebookKeepsCachedReplay(t *testing.T) {
	app := chatFixture(t)
	c, err := app.chat.Store.Create("alice", "default", chat.Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.AddEvent("run", "text_delta", map[string]string{"text": "VALID NOTEBOOK REPLAY"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Create(catalog.Bootstrap, "other", "Other"); err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "other", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", app.origin+"/p/default/api/chat/"+c.ID+"/events", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Remote-User", "alice")
	request.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
	ctx, cancel := context.WithTimeout(request.Context(), time.Second)
	defer cancel()
	request = request.WithContext(ctx)
	writer := &deletionReplayGate{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	ended := make(chan struct{})
	go func() { app.ServeHTTP(writer, request); close(ended) }()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("cached replay did not reach gate")
	}
	result := proxyCall(app, "alice", "DELETE", "/p/other/api/chat/"+c.ID, "")
	close(writer.release)
	<-ended
	if result.Code != 404 {
		t.Fatalf("wrong notebook DELETE: %d %s", result.Code, result.Body)
	}
	if !strings.Contains(writer.Body.String(), "VALID NOTEBOOK REPLAY") {
		t.Fatalf("wrong notebook request retired valid stream: %s", writer.Body)
	}
	if _, err = app.chat.Store.Read("alice", "default", c.ID); err != nil {
		t.Fatal(err)
	}
}
