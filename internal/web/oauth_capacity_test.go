package web

import (
	"net/url"
	"testing"
	"time"
)

// Behind one reverse proxy every caller shares a peer; unauthenticated token
// traffic must not starve refreshes that present a token this server issued.
func TestUnauthenticatedTokenFloodCannotBlockRefresh(t *testing.T) {
	app, _ := fixture(t)
	app.oauth.Refresh[digest("valid")] = withOAuthFamily(accessGrant{Client: "client", Subject: "@local-admin", Resource: app.origin + "/mcp", Expires: time.Now().Add(time.Hour).Unix()})
	junk := url.Values{"grant_type": {"refresh_token"}, "client_id": {"x"}, "refresh_token": {"garbage"}}
	for i := 0; i < 240; i++ {
		if w := tokenForm(app, "/oauth/token", junk); w.Code != 400 {
			t.Fatal(i, w.Code)
		}
	}
	if w := tokenForm(app, "/oauth/token", junk); w.Code != 429 {
		t.Fatalf("unauthenticated flood is no longer limited: %d", w.Code)
	}
	if w := tokenForm(app, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {"client"}, "refresh_token": {"valid"}}); w.Code != 200 {
		t.Fatalf("unauthenticated flood blocked a legitimate refresh: %d %s", w.Code, w.Body)
	}
}

// Issued tokens have their own per-account allowance, so one account's
// clients cannot exchange tokens without bound either.
func TestRecognizedTokenExchangesHaveAPerAccountAllowance(t *testing.T) {
	app, _ := fixture(t)
	other := withOAuthFamily(accessGrant{Client: "client", Subject: "@local-admin", Resource: app.origin + "/mcp", Expires: time.Now().Add(time.Hour).Unix()})
	app.oauth.Refresh[digest("unused")] = other
	busy := url.Values{"grant_type": {"refresh_token"}, "client_id": {"wrong-client"}, "refresh_token": {"unused"}}
	limited := false
	for i := 0; i < 100 && !limited; i++ {
		limited = tokenForm(app, "/oauth/token", busy).Code == 429
	}
	if !limited {
		t.Fatal("recognized token exchanges were never limited")
	}
	if rateWindows(t, app, "oauth-requests") != 0 {
		t.Fatal("recognized token exchanges consumed the unauthenticated budget")
	}
}
