package web

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

// A full pending table drops the oldest sign-in rather than refusing new ones.
func TestOIDCStartEvictsOldestPendingLogin(t *testing.T) {
	app, _ := fixture(t)
	p := provider(t)
	if err := app.ConfigureOIDC(p.server.URL, "test-client", "test-secret"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := 0; i < 256; i++ {
		expires := now.Add(time.Duration(i+1) * time.Minute)
		if err := app.putFlow(t.Context(), "oidc/login", fmt.Sprintf("filler-%03d", i), "", oidcPending{Next: "/", Expires: expires}, expires); err != nil {
			t.Fatal(err)
		}
	}
	state, binding := beginOIDC(t, app, p, "/auth/oidc/login")
	_, oldest, _ := readFlow[oidcPending](app, t.Context(), "oidc/login", "filler-000")
	_, newest, _ := readFlow[oidcPending](app, t.Context(), "oidc/login", "filler-255")
	var size int
	if err := app.db.Pool.QueryRow(t.Context(), `SELECT count(*) FROM pending_flows WHERE org = $1 AND kind = 'oidc/login'`, app.db.Org).Scan(&size); err != nil {
		t.Fatal(err)
	}
	if oldest || !newest || size != 256 {
		t.Fatalf("eviction: oldest kept=%v newest kept=%v size=%d", oldest, newest, size)
	}
	if w := finishOIDC(app, state, binding); w.Code != 303 {
		t.Fatalf("login after eviction: %d %s", w.Code, w.Body)
	}
}

func TestOIDCStartIsRateLimited(t *testing.T) {
	app, _ := fixture(t)
	p := provider(t)
	if err := app.ConfigureOIDC(p.server.URL, "test-client", "test-secret"); err != nil {
		t.Fatal(err)
	}
	limited := false
	for i := 0; i < 100 && !limited; i++ {
		w := oidcRequest(app, "POST", "/auth/oidc/login", "")
		limited = w.Code == 429
		if !limited && w.Code != 303 {
			t.Fatalf("start %d: %d %s", i, w.Code, w.Body)
		}
	}
	if !limited {
		t.Fatal("OIDC sign-in starts were never limited")
	}
	if rateWindows(t, app, "passwords") != 0 {
		t.Fatal("OIDC starts consumed the password budget")
	}
	// Another peer keeps its own budget.
	r := httptest.NewRequest("POST", app.origin+"/auth/oidc/login", nil)
	r.Header.Set("Origin", app.origin)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = "198.51.100.7:4321"
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 303 {
		t.Fatalf("other peer limited by first peer: %d", w.Code)
	}
}
