package web

import (
	"errors"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestChatDeleteRechecksQueuedAuthority(t *testing.T) {
	for _, retry := range []bool{false, true} {
		for _, kind := range []string{"notebook-access", "disabled-account", "revoked-session", "signed-out-session"} {
			label := kind
			if retry {
				label += "/retry"
			}
			t.Run(label, func(t *testing.T) {
				app := chatFixture(t)
				c, err := app.chat.Store.Create("alice", "default", chat.Options{Mode: "ask"})
				if err != nil {
					t.Fatal(err)
				}
				path := "/p/default/api/chat/" + c.ID
				if retry {
					if w := proxyCall(app, "alice", "DELETE", path, ""); w.Code != 200 {
						t.Fatal(w.Code, w.Body)
					}
				}
				request := httptest.NewRequest("DELETE", app.origin+path, nil)
				request.Header.Set("Origin", app.origin)
				var session *http.Cookie
				if strings.HasSuffix(kind, "-session") {
					session = signIn(t, app)
					request.AddCookie(session)
				} else {
					request.RemoteAddr = "127.0.0.1:1234"
					request.Header.Set("Remote-User", "alice")
					request.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
				}
				app.chat.Mu.Lock()
				locked := true
				defer func() {
					if locked {
						app.chat.Mu.Unlock()
					}
				}()
				writer := httptest.NewRecorder()
				ended := make(chan struct{})
				go func() { app.ServeHTTP(writer, request); close(ended) }()
				queued := false
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					b := make([]byte, 2<<20)
					n := runtime.Stack(b, true)
					for _, stack := range strings.Split(string(b[:n]), "\n\n") {
						if strings.Contains(stack, "(*Runtime).RetainContent") && strings.Contains(stack, "chatrun.(*Service).chatDelete") {
							queued = true
							break
						}
					}
					if queued {
						break
					}
					time.Sleep(time.Millisecond)
				}
				if !queued {
					t.Fatal("delete did not reach service mutex after its authority checks")
				}
				switch kind {
				case "notebook-access":
					err = app.catalog.Member(catalog.Bootstrap, "default", "alice", "")
				case "disabled-account":
					err = app.catalog.SetUser(catalog.Bootstrap, catalog.User{ID: "alice", Disabled: true})
				case "revoked-session":
					err = app.catalog.Revoke(catalog.Bootstrap, "alice")
				case "signed-out-session":
					// Only this session ends; the account and its access are unchanged.
					signOut(t, app, session)
				}
				if err != nil {
					t.Fatal(err)
				}
				app.chat.Mu.Unlock()
				locked = false
				select {
				case <-ended:
				case <-time.After(3 * time.Second):
					t.Fatal("delete remained stuck")
				}
				_, readErr := app.chat.Store.Read("alice", "default", c.ID)
				t.Logf("queued %s deletion returned HTTP %d; direct durable read error=%v", kind, writer.Code, readErr)
				if writer.Code != 403 && writer.Code != 401 {
					t.Errorf("revoked request deleted conversation: HTTP %d %s", writer.Code, writer.Body)
				}
				if !retry && readErr != nil {
					t.Errorf("revocation did not preserve transcript: %v", readErr)
				}
				if retry && !errors.Is(readErr, chat.ErrNotFound) {
					t.Errorf("retired record changed on revoked retry: %v", readErr)
				}
			})
		}
	}
}
