package web

import (
	"net/url"
	"testing"
	"time"

	"forgetmenote/internal/pg/pgtest"
)

func TestIndependentRevocationRetryAfterStorageFailure(t *testing.T) {
	app, _ := fixture(t)
	now := time.Now().Add(time.Hour).Unix()
	g := accessGrant{Client: "client", Subject: "@local-admin", Resource: app.origin + "/mcp", Family: "family", FamilyExpires: now, Expires: now}
	app.oauth.Access[digest("access")] = g
	app.oauth.Refresh[digest("refresh")] = g
	if err := app.oauth.Persist(); err != nil {
		t.Fatal(err)
	}
	restore := pgtest.FailWrites(t, app.db, "oauth")
	if w := tokenForm(app, "/oauth/revoke", url.Values{"token": {"access"}}); w.Code < 400 {
		t.Fatalf("expected failed storage, got %d", w.Code)
	}
	restore()
	if w := tokenForm(app, "/oauth/revoke", url.Values{"token": {"access"}}); w.Code != 200 {
		t.Fatalf("retry: %d", w.Code)
	}
	restarted, err := reopenOAuth(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.Access) != 0 || len(restarted.Refresh) != 0 {
		t.Fatalf("successful retry did not revoke durably: access=%d refresh=%d", len(restarted.Access), len(restarted.Refresh))
	}
}

func TestUnknownTokenRevocationDoesNotRequireStorageWrite(t *testing.T) {
	app, _ := fixture(t)
	pgtest.FailWrites(t, app.db, "oauth")
	if w := tokenForm(app, "/oauth/revoke", url.Values{"token": {"unknown"}}); w.Code != 200 {
		t.Fatalf("unknown token attempted a state write: %d %s", w.Code, w.Body)
	}
}
