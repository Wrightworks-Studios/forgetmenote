package web

import (
	"encoding/json"
	"fmt"
	"net/url"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
)

func TestLiveLongPollRechecksAccessOncePerSecond(t *testing.T) {
	app, store := fixture(t)
	reader := addLocal(t, app, recoverySession(t, app), "live-poller")
	if err := app.catalog.Member(catalog.Bootstrap, "default", reader, "read"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(t.Context(), "quiet.md", []byte("unchanged\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	login := oidcRequest(app, "POST", "/auth/password/login", url.Values{"username": {"live-poller"}, "password": {"test-password-long"}}.Encode())
	if login.Code != 303 {
		t.Fatal(login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	initial := oidcRequest(app, "GET", "/p/default/api/live?path=quiet.md", "", cookie)
	var base workspace.LiveDocument
	if initial.Code != 200 || json.Unmarshal(initial.Body.Bytes(), &base) != nil {
		t.Fatal(initial.Code, initial.Body.String())
	}
	queries := countAuthQueries(t, app)
	// Each browser-session check reaches the database.
	app.oauth.SetFreshness(0)
	since := fmt.Sprintf("%s/%s/%t", base.LiveRevision, base.Version, base.Pending)
	started := time.Now()
	w := oidcRequest(app, "GET", "/p/default/api/live?path=quiet.md&wait=3000&since="+url.QueryEscape(since), "", cookie)
	if w.Code != 200 || time.Since(started) < 3*time.Second {
		t.Fatalf("an unchanged long poll ended early: %d after %v", w.Code, time.Since(started))
	}
	// Admission, the monitor's start, its ticks at 1, 2 and 3 s and the check
	// before returning make six, with slack for timer skew; a 250 ms monitor
	// makes about sixteen.
	if checks := queries.n.Load(); checks > 8 {
		t.Fatalf("a 3 s long poll checked its session %d times", checks)
	} else {
		t.Logf("a 3 s long poll checked its session %d times", checks)
	}
}
