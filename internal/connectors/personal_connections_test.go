package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

const personalTestOwner = "0123456789abcdef0123456789abcdef"

func personalFixture(t *testing.T, h http.HandlerFunc) (*PersonalConnections, PersonalConfig, *pg.DB) {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	dir := t.TempDir()
	key, secret := filepath.Join(dir, "key"), filepath.Join(dir, "secret")
	if err := os.WriteFile(key, []byte(strings.Repeat("ba", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("fixture-oauth-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	config := PersonalConfig{Version: 1, KeyFile: key, Google: &OAuthApplication{ClientID: "fixture", SecretFile: secret}, Slack: &OAuthApplication{ClientID: "fixture", SecretFile: secret, Workspace: "T123"}}
	db := pgtest.Org(t)
	p, err := OpenPersonalConnections(db, config)
	if err != nil {
		t.Fatal(err)
	}
	p.googleToken, p.googleUser, p.googleAPI, p.slackAPI = server.URL+"/token", server.URL+"/userinfo", server.URL, server.URL
	return p, config, db
}

// gateway opens another gateway's view of the same instance database.
func personalGateway(t *testing.T, p *PersonalConnections, config PersonalConfig, db *pg.DB) *PersonalConnections {
	t.Helper()
	other, err := OpenPersonalConnections(db, config)
	if err != nil {
		t.Fatal(err)
	}
	other.googleToken, other.googleUser, other.googleAPI, other.slackAPI = p.googleToken, p.googleUser, p.googleAPI, p.slackAPI
	return other
}

func sealedCredentials(t *testing.T, db *pg.DB) map[string][]byte {
	t.Helper()
	rows, err := db.Pool.Query(t.Context(), `SELECT owner || '/' || provider, sealed FROM personal_credentials WHERE org = $1`, db.Org)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var id string
		var sealed []byte
		if err = rows.Scan(&id, &sealed); err != nil {
			t.Fatal(err)
		}
		out[id] = sealed
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func listConnections(t *testing.T, p *PersonalConnections, owner string) []PersonalConnection {
	t.Helper()
	connections, err := p.List(owner)
	if err != nil {
		t.Fatal(err)
	}
	return connections
}

func finishGoogleFixture(t *testing.T, p *PersonalConnections) PersonalConnection {
	t.Helper()
	u, err := p.Begin(personalTestOwner, "google", "browser-binding", "https://example.com/account/connections/google/callback")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(u)
	if parsed.Query().Get("code_challenge") == "" || !strings.Contains(parsed.Query().Get("scope"), "gmail.modify") {
		t.Fatal("missing PKCE or requested bundle")
	}
	connection, err := p.Finish(t.Context(), personalTestOwner, "google", "browser-binding", parsed.Query().Get("state"), "fixture-code", func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Finish(t.Context(), personalTestOwner, "google", "browser-binding", parsed.Query().Get("state"), "fixture-code", func() error { return nil }); err == nil {
		t.Fatal("OAuth callback replayed")
	}
	return connection
}

func TestPersonalConnectionsEncryptedRotatingCredentials(t *testing.T) {
	var refresh atomic.Int32
	p, config, db := personalFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			r.ParseForm()
			if r.Form.Get("grant_type") == "refresh_token" {
				refresh.Add(1)
				if r.Form.Get("refresh_token") != "fixture-refresh-original" {
					t.Error("wrong refresh token")
				}
				json.NewEncoder(w).Encode(map[string]any{"access_token": "fixture-access-rotated", "refresh_token": "fixture-refresh-rotated", "expires_in": 3600, "scope": "openid email https://www.googleapis.com/auth/gmail.modify"})
			} else {
				json.NewEncoder(w).Encode(map[string]any{"access_token": "fixture-access-original", "refresh_token": "fixture-refresh-original", "expires_in": 1, "scope": "openid email https://www.googleapis.com/auth/gmail.modify"})
			}
		case "/userinfo":
			json.NewEncoder(w).Encode(map[string]any{"sub": "google-user-123", "email": "alice@example.com", "email_verified": true})
		default:
			http.NotFound(w, r)
		}
	})
	connection := finishGoogleFixture(t, p)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := p.token(t.Context(), personalTestOwner, "google", connection.Generation)
			if err != nil || token.AccessToken != "fixture-access-rotated" {
				t.Errorf("rotated token unavailable: %v", err)
			}
		}()
	}
	wg.Wait()
	if refresh.Load() != 1 {
		t.Fatal("refresh was not serialized", refresh.Load())
	}
	sealed := sealedCredentials(t, db)
	if len(sealed) != 1 {
		t.Fatalf("expected one stored grant: %d", len(sealed))
	}
	for _, raw := range sealed {
		for _, secret := range []string{"fixture-refresh-rotated", "fixture-access-rotated", "alice@example.com", "google-user-123"} {
			if strings.Contains(string(raw), secret) {
				t.Fatal("plaintext credential metadata persisted")
			}
		}
	}
	reopened, err := OpenPersonalConnections(db, config)
	if err != nil {
		t.Fatal(err)
	}
	token, err := reopened.token(t.Context(), personalTestOwner, "google", connection.Generation)
	if err != nil || token.RefreshToken != "fixture-refresh-rotated" {
		t.Fatal("rotation did not survive restart", err)
	}
	if err = reopened.Disconnect(personalTestOwner, "google"); err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.token(t.Context(), personalTestOwner, "google", connection.Generation); err == nil {
		t.Fatal("disconnected token reused")
	}
	if len(listConnections(t, reopened, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")[0].Scopes) > 0 {
		t.Fatal("another owner saw grant scopes")
	}
}

func TestPersonalConnectionsDisconnectWinsOverRefresh(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	p, _, _ := personalFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/userinfo" {
			json.NewEncoder(w).Encode(map[string]any{"sub": "google-user-123", "email": "alice@example.com", "email_verified": true})
			return
		}
		r.ParseForm()
		if r.Form.Get("grant_type") == "refresh_token" {
			close(started)
			<-release
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "fixture-access", "refresh_token": "fixture-refresh", "expires_in": 1, "scope": "openid email"})
	})
	c := finishGoogleFixture(t, p)
	done := make(chan error, 1)
	go func() {
		_, err := p.token(context.Background(), personalTestOwner, "google", c.Generation)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not start")
	}
	if err := p.Disconnect(personalTestOwner, "google"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("in-flight refresh resurrected disconnected account")
	}
	if listConnections(t, p, personalTestOwner)[0].State != "disconnected" {
		t.Fatal("disconnect lost")
	}
}

func TestPersonalConnectionsRejectWrongBrowserAndTamperedStorage(t *testing.T) {
	p, config, db := personalFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid callback reached provider")
		http.Error(w, "unexpected", 500)
	})
	u, err := p.Begin(personalTestOwner, "google", "correct", "https://example.com/callback")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(u)
	if _, err = p.Finish(t.Context(), personalTestOwner, "google", "wrong", parsed.Query().Get("state"), "code", func() error { return nil }); err == nil {
		t.Fatal("wrong browser accepted")
	}
	if err = p.Disconnect(personalTestOwner, "google"); err != nil {
		t.Fatal(err)
	}
	raw := sealedCredentials(t, db)[personalKey(personalTestOwner, "google")]
	// A grant sealed for one account does not open as another's.
	const other = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err = db.Pool.Exec(t.Context(), `INSERT INTO personal_credentials (org, owner, provider, version, sealed) VALUES ($1, $2, 'google', 1, $3)`, db.Org, other, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenPersonalConnections(db, config); err == nil || !strings.Contains(err.Error(), "cannot decrypt") {
		t.Fatal("grant moved to another account accepted", err)
	}
	if _, err = p.List(other); err == nil {
		t.Fatal("running gateway served a moved grant")
	}
	if _, err = db.Pool.Exec(t.Context(), `DELETE FROM personal_credentials WHERE org = $1 AND owner = $2`, db.Org, other); err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if _, err = db.Pool.Exec(t.Context(), `UPDATE personal_credentials SET sealed = $2 WHERE org = $1`, db.Org, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenPersonalConnections(db, config); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}

func TestPersonalConnectionsShareGrantsAcrossGateways(t *testing.T) {
	var refresh atomic.Int32
	p, config, db := personalFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			r.ParseForm()
			if r.Form.Get("grant_type") == "refresh_token" {
				refresh.Add(1)
				json.NewEncoder(w).Encode(map[string]any{"access_token": "fixture-access-rotated", "refresh_token": "fixture-refresh-rotated", "expires_in": 3600, "scope": "openid email"})
			} else {
				json.NewEncoder(w).Encode(map[string]any{"access_token": "fixture-access-original", "refresh_token": "fixture-refresh-original", "expires_in": 1, "scope": "openid email"})
			}
		case "/userinfo":
			json.NewEncoder(w).Encode(map[string]any{"sub": "google-user-123", "email": "alice@example.com", "email_verified": true})
		default:
			http.NotFound(w, r)
		}
	})
	other := personalGateway(t, p, config, db)
	connection := finishGoogleFixture(t, p)
	if got := listConnections(t, other, personalTestOwner)[0]; got.State != "connected" || got.Generation != connection.Generation || got.Email != "alice@example.com" {
		t.Fatalf("another gateway did not see the grant: %+v", got)
	}
	if other.LinkedOwner("google", "", "google-user-123") != personalTestOwner {
		t.Fatal("another gateway did not resolve the linked account")
	}
	if err := other.Check(personalTestOwner, "google", connection.Generation); err != nil {
		t.Fatal(err)
	}
	// One gateway's rotation is the other's token, without a second refresh.
	if token, err := other.token(t.Context(), personalTestOwner, "google", connection.Generation); err != nil || token.AccessToken != "fixture-access-rotated" {
		t.Fatal("refresh on another gateway failed", err)
	}
	if token, err := p.token(t.Context(), personalTestOwner, "google", connection.Generation); err != nil || token.RefreshToken != "fixture-refresh-rotated" || refresh.Load() != 1 {
		t.Fatal("rotated token was not shared", err, refresh.Load())
	}
	// The provider identity stays linked to one account across gateways, and a
	// flow begun on one gateway completes on another.
	const second = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	u, err := other.Begin(second, "google", "second-browser", "https://example.com/callback")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(u)
	if _, err = p.Finish(t.Context(), second, "google", "second-browser", parsed.Query().Get("state"), "fixture-code", func() error { return nil }); err == nil || !strings.Contains(err.Error(), "already linked") {
		t.Fatal("one provider account linked to two employees", err)
	}
	// Disconnecting on another gateway ends a flow begun before it.
	stale, err := p.Begin(personalTestOwner, "google", "stale-browser", "https://example.com/callback")
	if err != nil {
		t.Fatal(err)
	}
	if err = other.Disconnect(personalTestOwner, "google"); err != nil {
		t.Fatal(err)
	}
	if err = p.Check(personalTestOwner, "google", connection.Generation); err == nil {
		t.Fatal("disconnect on another gateway was not seen")
	}
	if _, err = p.token(t.Context(), personalTestOwner, "google", connection.Generation); err == nil {
		t.Fatal("token served after another gateway disconnected")
	}
	parsed, _ = url.Parse(stale)
	if _, err = p.Finish(t.Context(), personalTestOwner, "google", "stale-browser", parsed.Query().Get("state"), "fixture-code", func() error { return nil }); err == nil || !strings.Contains(err.Error(), "invalid or expired connection") {
		t.Fatal("a flow outlived a disconnect on another gateway", err)
	}
	// Nor can a flow overwrite a grant another gateway changed after it began.
	raced, err := p.Begin(personalTestOwner, "google", "raced-browser", "https://example.com/callback")
	if err != nil {
		t.Fatal(err)
	}
	current, version, err := other.load(t.Context(), db.Pool, personalTestOwner, "google")
	if err != nil {
		t.Fatal(err)
	}
	current.Generation++
	if err = other.save(t.Context(), db.Pool, current, version); err != nil {
		t.Fatal(err)
	}
	parsed, _ = url.Parse(raced)
	if _, err = p.Finish(t.Context(), personalTestOwner, "google", "raced-browser", parsed.Query().Get("state"), "fixture-code", func() error { return nil }); err == nil || !strings.Contains(err.Error(), "changed while authorization") {
		t.Fatal("stale authorization replaced a newer grant", err)
	}
	if got := listConnections(t, p, personalTestOwner)[0]; got.State != "disconnected" {
		t.Fatalf("disconnect lost: %+v", got)
	}
}

func TestPersonalConnectionsRefreshClaimIsShared(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var refreshes atomic.Int32
	p, config, db := personalFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/userinfo" {
			json.NewEncoder(w).Encode(map[string]any{"sub": "google-user-123", "email": "alice@example.com", "email_verified": true})
			return
		}
		r.ParseForm()
		if r.Form.Get("grant_type") == "refresh_token" {
			if refreshes.Add(1) == 1 {
				close(started)
				<-release
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "fixture-access-rotated", "refresh_token": "fixture-refresh-rotated", "expires_in": 3600, "scope": "openid email"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "fixture-access", "refresh_token": "fixture-refresh", "expires_in": 1, "scope": "openid email"})
	})
	other := personalGateway(t, p, config, db)
	c := finishGoogleFixture(t, p)
	done := make(chan error, 1)
	go func() {
		_, err := p.token(context.Background(), personalTestOwner, "google", c.Generation)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not start")
	}
	// A second gateway neither reuses the token being rotated nor refreshes it
	// again: it waits for the refresh in progress and then uses its token.
	type result struct {
		token personalCredential
		err   error
	}
	waited := make(chan result, 1)
	go func() {
		token, err := other.token(t.Context(), personalTestOwner, "google", c.Generation)
		waited <- result{token, err}
	}()
	select {
	case r := <-waited:
		t.Fatal("second gateway did not wait for the refresh in progress", r.token.AccessToken, r.err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r := <-waited; r.err != nil || r.token.AccessToken != "fixture-access-rotated" {
		t.Fatal("completed refresh unavailable to the waiting gateway", r.err)
	}
	if n := refreshes.Load(); n != 1 {
		t.Fatal("the grant was refreshed more than once", n)
	}
}

func TestPersonalRefreshOfAGoneGatewayNeedsReauthorization(t *testing.T) {
	p, config, db := personalFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/userinfo" {
			json.NewEncoder(w).Encode(map[string]any{"sub": "google-user-123", "email": "alice@example.com", "email_verified": true})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "fixture-access", "refresh_token": "fixture-refresh", "expires_in": 1, "scope": "openid email"})
	})
	c := finishGoogleFixture(t, p)
	// Another gateway claimed the refresh and is still calling the provider.
	other, leave := pgtest.Gateway(t, db)
	cred, version, err := p.load(t.Context(), db.Pool, personalTestOwner, "google")
	if err != nil {
		t.Fatal(err)
	}
	cred.Refreshing, cred.RefreshHolder = true, other.Holder
	if err = p.save(t.Context(), db.Pool, cred, version); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenPersonalConnections(db, config)
	if err != nil {
		t.Fatal(err)
	}
	if got := listConnections(t, reopened, personalTestOwner)[0]; got.State != "connected" {
		t.Fatalf("opening interrupted a live gateway's refresh: %+v", got)
	}
	if _, err = reopened.token(t.Context(), personalTestOwner, "google", c.Generation); !errors.Is(err, ErrPersonalChanged) {
		t.Fatal("a refresh in progress elsewhere was not waited for", err)
	}
	leave()
	if _, err = reopened.token(t.Context(), personalTestOwner, "google", c.Generation); err == nil || errors.Is(err, ErrPersonalChanged) {
		t.Fatal("the refresh of a gateway that is gone was reused", err)
	}
	if got := listConnections(t, reopened, personalTestOwner)[0]; got.State != "reauthorize" {
		t.Fatalf("the refresh of a gateway that is gone did not ask for reauthorization: %+v", got)
	}
}

func TestPersonalConnectionsFailClosedWithoutDatabase(t *testing.T) {
	p, _, db := personalFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			json.NewEncoder(w).Encode(map[string]any{"access_token": "fixture-access", "refresh_token": "fixture-refresh", "expires_in": 3600, "scope": "openid email"})
		case "/userinfo":
			json.NewEncoder(w).Encode(map[string]any{"sub": "google-user-123", "email": "alice@example.com", "email_verified": true})
		default:
			http.NotFound(w, r)
		}
	})
	c := finishGoogleFixture(t, p)
	restore := pgtest.FailTable(t, db, "personal_credentials")
	if err := p.Disconnect(personalTestOwner, "google"); !errors.Is(err, ErrPersonalUnavailable) {
		t.Fatal("failed disconnect was not reported", err)
	}
	restore()
	if err := p.Check(personalTestOwner, "google", c.Generation); err != nil {
		t.Fatal("failed disconnect changed the grant", err)
	}
}

func TestPersonalSlackUsesUserGrantAndFullThreadPrecondition(t *testing.T) {
	var writes atomic.Int32
	var change atomic.Bool
	p, _, _ := personalFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth.v2.access":
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "access_token": "bot-token-must-not-be-used", "team": map[string]string{"id": "T123"}, "authed_user": map[string]any{"id": "U123", "token_type": "user", "access_token": "fixture-user-token", "scope": "im:history,chat:write"}})
		case "/auth.test":
			if r.Header.Get("Authorization") != "Bearer fixture-user-token" {
				t.Error("used bot identity")
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "user_id": "U123", "team_id": "T123"})
		case "/conversations.replies":
			if r.URL.Query().Get("cursor") == "" {
				json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []any{map[string]string{"ts": "1.1", "user": "U456", "text": "Original"}}, "has_more": true, "response_metadata": map[string]string{"next_cursor": "page-two"}})
			} else {
				text := "Follow-up"
				if change.Load() {
					text = "Already handled"
				}
				json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []any{map[string]string{"ts": "1.2", "user": "U456", "text": text}}, "has_more": false})
			}
		case "/chat.postMessage":
			writes.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1.3"})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	target, err := p.Begin(personalTestOwner, "slack", "browser", "https://example.test/callback")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(target)
	connection, err := p.Finish(t.Context(), personalTestOwner, "slack", "browser", u.Query().Get("state"), "code", func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	guard := func(context.Context) error { return nil }
	read := PersonalOperation{Provider: "slack", Generation: connection.Generation, Operation: "slack.thread", Arguments: map[string]any{"channel": "D123", "ts": "1.1"}}
	result, err := p.Call(t.Context(), personalTestOwner, read, guard)
	if err != nil {
		t.Fatal(err)
	}
	data := object(result.Data)
	if len(array(data["messages"])) != 2 {
		t.Fatal("incomplete thread")
	}
	write := PersonalOperation{Provider: "slack", Generation: connection.Generation, Operation: "slack.send", Arguments: map[string]any{"channel": "D123", "thread_ts": "1.1", "text": "Reviewed reply", "actionID": "action", "expectedRevision": data["revision"]}}
	if _, err = p.Call(t.Context(), personalTestOwner, write, guard); err == nil {
		t.Fatal("missing explicit submission accepted")
	}
	change.Store(true)
	if _, err = p.Call(WithSubmission(t.Context()), personalTestOwner, write, guard); err == nil || !WriteRejected(err) {
		t.Fatalf("stale later page accepted: %v", err)
	}
	if writes.Load() != 0 {
		t.Fatal("sent a stale message")
	}
	change.Store(false)
	if _, err = p.Call(WithSubmission(t.Context()), personalTestOwner, write, guard); err != nil || writes.Load() != 1 {
		t.Fatal("approved fresh reply failed", err)
	}
	if owner := p.LinkedOwner("slack", "T123", "U123"); owner != personalTestOwner {
		t.Fatal("verified Slack identity not linked")
	}
	if owner := p.LinkedOwner("slack", "T999", "U123"); owner != "" {
		t.Fatal("cross-workspace identity linked")
	}
}

func TestPersonalProviderRejectsTrailingJSON(t *testing.T) {
	p, _, _ := personalFixture(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ok":true} {"extra":true}`)) })
	if _, err := p.requestJSON(t.Context(), "GET", p.googleAPI, "synthetic", nil); err == nil {
		t.Fatal("ambiguous response accepted")
	}
}

// A refresh failure asks the person to authorize again only when the provider
// revoked the grant, or, since Slack rotates refresh tokens, when Slack may have
// spent the old one. Any other failure keeps the grant for the next request.
func TestPersonalRefreshReauthorizesOnlyWhenTheGrantIsGone(t *testing.T) {
	answer := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			w.Write([]byte(body))
		}
	}
	drop := func(w http.ResponseWriter, r *http.Request) {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
	}
	rotated := answer(200, `{"ok":true,"token_type":"user","access_token":"fixture-access-rotated","refresh_token":"fixture-refresh-rotated","expires_in":3600}`)
	for _, c := range []struct {
		name, provider string
		refresh        http.HandlerFunc // nil: the provider cannot be reached
		reauthorize    bool
		busy           bool
	}{
		{name: "google server error", provider: "google", refresh: answer(503, `{"error":"backend_error"}`)},
		{name: "google rate limit", provider: "google", refresh: answer(429, ``), busy: true},
		{name: "google dropped connection", provider: "google", refresh: drop},
		{name: "google unreachable", provider: "google"},
		{name: "google unusable answer", provider: "google", refresh: answer(200, `{"access_token":"fixture-access-rotated"}`)},
		{name: "google revoked grant", provider: "google", refresh: answer(400, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`), reauthorize: true},
		{name: "google rejected client", provider: "google", refresh: answer(401, `{"error":"invalid_client"}`), reauthorize: true},
		{name: "slack rate limit", provider: "slack", refresh: answer(200, `{"ok":false,"error":"ratelimited"}`), busy: true},
		{name: "slack HTTP rate limit", provider: "slack", refresh: answer(429, ``), busy: true},
		{name: "slack unreachable", provider: "slack"},
		{name: "slack server error", provider: "slack", refresh: answer(500, ``), reauthorize: true},
		{name: "slack dropped connection", provider: "slack", refresh: drop, reauthorize: true},
		{name: "slack internal error", provider: "slack", refresh: answer(200, `{"ok":false,"error":"internal_error"}`), reauthorize: true},
		{name: "slack revoked grant", provider: "slack", refresh: answer(200, `{"ok":false,"error":"invalid_refresh_token"}`), reauthorize: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var refresh atomic.Pointer[http.HandlerFunc]
			refresh.Store(&c.refresh)
			p, _, db := personalFixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/token", "/oauth.v2.access":
					r.ParseForm()
					if r.Form.Get("grant_type") == "refresh_token" {
						(*refresh.Load())(w, r)
					} else if r.URL.Path == "/token" {
						json.NewEncoder(w).Encode(map[string]any{"access_token": "fixture-access", "refresh_token": "fixture-refresh", "expires_in": 1, "scope": "openid email"})
					} else {
						json.NewEncoder(w).Encode(map[string]any{"ok": true, "team": map[string]string{"id": "T123"}, "authed_user": map[string]any{"id": "U123", "token_type": "user", "access_token": "fixture-access", "refresh_token": "fixture-refresh", "expires_in": 1, "scope": "im:history"}})
					}
				case "/userinfo":
					json.NewEncoder(w).Encode(map[string]any{"sub": "google-user-123", "email": "alice@example.com", "email_verified": true})
				case "/auth.test":
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "user_id": "U123", "team_id": "T123"})
				default:
					http.NotFound(w, r)
				}
			})
			target, err := p.Begin(personalTestOwner, c.provider, "browser", "https://example.test/callback")
			if err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(target)
			connection, err := p.Finish(t.Context(), personalTestOwner, c.provider, "browser", u.Query().Get("state"), "code", func() error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			tokenURL, slackAPI := p.googleToken, p.slackAPI
			if c.refresh == nil {
				gone := httptest.NewServer(http.NotFoundHandler())
				gone.Close()
				p.googleToken, p.slackAPI = gone.URL+"/token", gone.URL
			}
			_, err = p.token(t.Context(), personalTestOwner, c.provider, connection.Generation)
			if err == nil || errors.Is(err, ErrProviderBusy) != c.busy {
				t.Fatalf("refresh outcome: %v", err)
			}
			stored, _, err := p.load(t.Context(), db.Pool, personalTestOwner, c.provider)
			if err != nil {
				t.Fatal(err)
			}
			if want := map[bool]string{true: "reauthorize", false: "connected"}[c.reauthorize]; stored.State != want || stored.Refreshing {
				t.Fatalf("state %q (refreshing %v), want %q", stored.State, stored.Refreshing, want)
			}
			if c.reauthorize {
				return
			}
			// The kept grant refreshes once the provider answers again.
			p.googleToken, p.slackAPI = tokenURL, slackAPI
			refresh.Store(&rotated)
			if token, err := p.token(t.Context(), personalTestOwner, c.provider, connection.Generation); err != nil || token.AccessToken != "fixture-access-rotated" {
				t.Fatal("the kept grant did not refresh later", err)
			}
		})
	}
}
