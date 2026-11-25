package web

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/reposource"
)

// expireFlow ends a pending flow's lifetime, as time passing would.
func expireFlow(t *testing.T, app *Server, kind, key string) {
	t.Helper()
	tag, err := app.db.Pool.Exec(context.Background(), `UPDATE pending_flows SET expires = now() - interval '1 minute' WHERE org = $1 AND kind = $2 AND key = $3`, app.db.Org, kind, key)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatal("no pending flow to expire", kind, err)
	}
}

func pendingFlow[T any](t *testing.T, app *Server, kind, key string) T {
	t.Helper()
	value, ok, err := readFlow[T](app, context.Background(), kind, key)
	if err != nil || !ok {
		t.Fatal("pending flow missing", kind, ok, err)
	}
	return value
}

// rateWindows counts group's live request windows.
func rateWindows(t *testing.T, app *Server, group string) int {
	t.Helper()
	var n int
	if err := app.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM rate_windows WHERE org = $1 AND split_part(key, '/', 1) = $2 AND started > now() - interval '1 minute'`, app.db.Org, group).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// gatewayOf serves app's instance as another gateway: the same catalog and
// database organization, its own process state.
func gatewayOf(t *testing.T, app *Server) *Server {
	t.Helper()
	return gatewayWith(t, app, app.blobs)
}

// gatewayWith is another gateway of app's instance, with a data directory of
// its own and blobs as the instance's blob storage.
func gatewayWith(t *testing.T, app *Server, blobs blob.Store) *Server {
	t.Helper()
	db, leave := pgtest.Gateway(t, app.db)
	t.Cleanup(leave)
	other, err := NewWithBlobs(app.catalog, db, blobs, t.TempDir(), app.origin, app.token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	return other
}

// A password limit holds for the instance however attempts are spread over
// gateways.
func TestPasswordLimitHoldsAcrossGateways(t *testing.T) {
	app, _ := fixture(t)
	other := gatewayOf(t, app)
	body := url.Values{"username": {"missing"}, "password": {"incorrect"}}.Encode()
	for i := range 10 {
		gateway := app
		if i%2 == 1 {
			gateway = other
		}
		if w := oidcRequest(gateway, "POST", "/auth/password/login", body); w.Code != 401 {
			t.Fatal(i, w.Code, w.Body.String())
		}
	}
	for _, gateway := range []*Server{app, other} {
		if w := oidcRequest(gateway, "POST", "/auth/password/login", body); w.Code != 429 {
			t.Fatal("a gateway allowed attempts past the account's limit", w.Code)
		}
	}
	if rateWindows(t, app, "oauth-requests") != 0 {
		t.Fatal("password attempts consumed the OAuth budget")
	}
}

// The provider may return the browser to any gateway; the sign-in still
// completes exactly once.
func TestOIDCCallbackCompletesOnAnotherGateway(t *testing.T) {
	app, _ := fixture(t)
	p := provider(t)
	other := gatewayOf(t, app)
	for _, gateway := range []*Server{app, other} {
		if err := gateway.ConfigureOIDC(p.server.URL, "test-client", "test-secret"); err != nil {
			t.Fatal(err)
		}
	}
	state, binding := beginOIDC(t, app, p, "/auth/oidc/login")
	if w := finishOIDC(other, state); w.Code != 403 {
		t.Fatal("a callback without the browser binding completed", w.Code)
	}
	if w := finishOIDC(other, state, binding); w.Code != 303 {
		t.Fatalf("another gateway could not complete the sign-in: %d %s", w.Code, w.Body)
	}
	if w := finishOIDC(app, state, binding); w.Code != 403 {
		t.Fatal("a sign-in completed twice", w.Code)
	}
}

// A connection ticket starts one browser authorization, whichever gateway
// serves each request.
func TestConnectionTicketStartsOnceAcrossGateways(t *testing.T) {
	app, _ := fixture(t)
	other := gatewayOf(t, app)
	for _, gateway := range []*Server{app, other} {
		gateway.ConfigureHeadless(true)
		gateway.repositoryConnections = &repositoryConnections{github: &reposource.GitHub{Config: reposource.Config{ClientID: "fixture"}}}
	}
	t.Cleanup(func() { app.repositories, other.repositories = nil, nil })
	start := apiCall(app, "POST", "/api/v1/actions/begin_github_connection", `{"project":"new-notebook","name":"New notebook","purpose":"source","creating":true}`, testToken)
	var ticket struct{ ID, URL string }
	if start.Code != 200 || json.Unmarshal(start.Body.Bytes(), &ticket) != nil {
		t.Fatalf("start: %d %s", start.Code, start.Body)
	}
	if w := apiCall(other, "GET", strings.TrimPrefix(ticket.URL, app.origin), "", testToken); w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "https://github.com/login/oauth/authorize?") {
		t.Fatalf("another gateway could not start the ticket: %d %s", w.Code, w.Body)
	}
	if w := apiCall(app, "GET", strings.TrimPrefix(ticket.URL, app.origin), "", testToken); w.Code != 403 {
		t.Fatal("a ticket started twice across gateways", w.Code)
	}
	if !pendingFlow[githubSetup](t, app, "connection/ticket", digest(ticket.ID)).Started {
		t.Fatal("the start was not recorded")
	}
}
