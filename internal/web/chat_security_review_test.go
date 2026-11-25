package web

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
)

type securityEpochWriter struct {
	*httptest.ResponseRecorder
	revoke func()
}

func (w *securityEpochWriter) SetWriteDeadline(deadline time.Time) error {
	if deadline.IsZero() && w.revoke != nil {
		f := w.revoke
		w.revoke = nil
		f()
	}
	return nil
}
func TestSecuritySSERevocationBeforeReplay(t *testing.T) {
	app := chatFixture(t)
	c, err := app.chat.Store.Create("alice", "default", chat.Options{Connection: "local", Model: "synthetic", Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.AddEvent("run", "text", map[string]string{"text": "private-replay-marker"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRecorder()
	if err = app.createSession(login, "alice"); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", app.origin+"/p/default/api/chat/"+c.ID+"/events", nil)
	r.AddCookie(login.Result().Cookies()[0])
	w := &securityEpochWriter{ResponseRecorder: httptest.NewRecorder(), revoke: func() {
		if err := app.catalog.Revoke(catalog.Bootstrap, "alice"); err != nil {
			t.Fatal(err)
		}
	}}
	app.ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), "private-replay-marker") {
		t.Fatalf("revoked browser session received private replay: %s", w.Body.String())
	}
}
func securityConfigureLimits(t *testing.T, app *Server) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chat.json")
	cfg := `{"connections":[{"id":"local","label":"Synthetic","format":"chat-completions","baseURL":"http://127.0.0.1:1/v1","models":["synthetic"],"local":true}],"limits":{"maxConversations":1,"maxRecordBytes":4096}}`
	if err := os.WriteFile(path, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureChat(path); err != nil {
		t.Fatal(err)
	}
}
func TestSecurityCreateHonorsConversationLimit(t *testing.T) {
	app := chatFixture(t)
	securityConfigureLimits(t, app)
	for i := 0; i < 2; i++ {
		w := proxyCall(app, "alice", "POST", "/p/default/api/chat", `{"connection":"local","model":"synthetic","mode":"ask"}`)
		if i == 0 && w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if i == 1 && w.Code == 200 {
			t.Fatalf("second chat accepted beyond configured maxConversations=1: %d", w.Code)
		}
	}
}
func TestSecurityCreateHonorsRecordLimit(t *testing.T) {
	app := chatFixture(t)
	securityConfigureLimits(t, app)
	body, _ := json.Marshal(chat.Options{Connection: "local", Model: "synthetic", Mode: "ask", Actions: []string{strings.Repeat("x", 8192)}})
	w := proxyCall(app, "alice", "POST", "/p/default/api/chat", string(body))
	if w.Code == 200 {
		t.Fatalf("oversized unknown action persisted past maxRecordBytes=4096: HTTP %d response bytes=%d", w.Code, w.Body.Len())
	}
}
