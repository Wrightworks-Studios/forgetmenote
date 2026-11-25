package web

import (
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
)

// Save to note writes with Edit authority checked again inside its content
// fence, so a demotion while the request is in flight cannot be outrun.
func TestChatSaveNoteRechecksEditBeforeWriting(t *testing.T) {
	app := chatFixture(t)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	c, err := app.chat.Store.Create("alice", "default", chat.Options{Connection: "local", Model: "synthetic", Mode: "ask", Corpus: &chat.Corpus{Version: 1, Archives: "exclude", Drafts: "exclude"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Turns = append(c.Turns, chat.Turn{ID: "answer", Role: "assistant", Content: "Evidence worth saving."})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	body, send := io.Pipe()
	r := httptest.NewRequest("POST", app.origin+"/p/default/api/chat/"+c.ID+"/save-note", body)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Remote-User", "alice")
	r.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
	r.Header.Set("Origin", app.origin)
	r.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		app.ServeHTTP(recorder, r)
		close(done)
	}()
	// The first write returns once the handler, past its first Edit check, is
	// reading the body.
	if _, err = send.Write([]byte(`{`)); err != nil {
		t.Fatal(err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	go func() {
		send.Write([]byte(`"submissionId":"save-demoted","path":"notes/saved.md","revision":"","turnIds":["answer"]}`))
		send.Close()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("save did not finish")
	}
	store, _ := app.catalog.Access(catalog.Bootstrap, "default", "read")
	if _, err := store.Read(t.Context(), "notes/saved.md", ""); err == nil || recorder.Code != 403 {
		t.Fatalf("demoted reader saved a note: %d %s", recorder.Code, recorder.Body)
	}
}
