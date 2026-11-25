package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/pg/pgtest"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func seedRefresh(t *testing.T, app *Server, token, family string) url.Values {
	t.Helper()
	now := time.Now().Unix()
	app.oauth.Refresh[digest(token)] = accessGrant{Subject: "@local-admin", Client: "refresh-client", Scope: "notes:read notes:write", Resource: app.origin + "/mcp", Family: family, FamilyExpires: now + 90*86400, Expires: now + 30*86400}
	if err := app.oauth.Persist(); err != nil {
		t.Fatal(err)
	}
	return url.Values{"grant_type": {"refresh_token"}, "client_id": {"refresh-client"}, "refresh_token": {token}}
}
func decodePair(t *testing.T, w *httptest.ResponseRecorder) (out struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
}) {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("token HTTP %d: %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return
}

func TestConcurrentRefreshWithinGraceKeepsAuthorization(t *testing.T) {
	app, _ := fixture(t)
	form := seedRefresh(t, app, "shared-refresh", "shared-family")
	results := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); results <- tokenForm(app, "/oauth/token", form) }()
	}
	wg.Wait()
	close(results)
	for w := range results {
		pair := decodePair(t, w)
		next := form.Clone()
		next.Set("refresh_token", pair.Refresh)
		decodePair(t, tokenForm(app, "/oauth/token", next))
	}
}

func TestRefreshExtendsRefreshButNotFamilyLifetime(t *testing.T) {
	app, _ := fixture(t)
	verifier := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sum := sha256.Sum256([]byte(verifier))
	code, err := app.issueCode(grantRequest{Subject: "@local-admin", Client: "refresh-client", Redirect: "http://127.0.0.1/callback", Challenge: base64.RawURLEncoding.EncodeToString(sum[:]), Scope: "notes:read", Resource: app.origin + "/mcp", Expires: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	pair := decodePair(t, tokenForm(app, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {"refresh-client"}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {"http://127.0.0.1/callback"}}))
	grant := app.oauth.Refresh[digest(pair.Refresh)]
	if grant.FamilyExpires < now+90*86400-2 || grant.FamilyExpires > now+90*86400+2 {
		t.Fatalf("family lifetime: %d", grant.FamilyExpires-now)
	}
	if grant.Expires < now+30*86400-2 || grant.Expires > now+30*86400+2 {
		t.Fatalf("refresh lifetime: %d", grant.Expires-now)
	}
	grant.Expires = now + 60
	app.oauth.Refresh[digest(pair.Refresh)] = grant
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {"refresh-client"}, "refresh_token": {pair.Refresh}}
	successor := decodePair(t, tokenForm(app, "/oauth/token", form))
	next := app.oauth.Refresh[digest(successor.Refresh)]
	if next.Family != grant.Family || next.FamilyExpires != grant.FamilyExpires || next.Expires < now+30*86400-2 {
		t.Fatal("refresh failed to renew within original authorization")
	}
}

type rotatingOAuthTransport struct {
	token    atomic.Value
	mu       sync.Mutex
	sessions []string
}

func (r *rotatingOAuthTransport) RoundTrip(in *http.Request) (*http.Response, error) {
	request := in.Clone(in.Context())
	request.Header = in.Header.Clone()
	request.Header.Set("Authorization", "Bearer "+r.token.Load().(string))
	if id := request.Header.Get("Mcp-Session-Id"); id != "" {
		r.mu.Lock()
		r.sessions = append(r.sessions, id)
		r.mu.Unlock()
	}
	return http.DefaultTransport.RoundTrip(request)
}
func oauthLegacyClient(t *testing.T, app *Server, transport *rotatingOAuthTransport, events chan string) (*mcp.ClientSession, context.Context) {
	t.Helper()
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)
	app.origin = server.URL
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	options := &mcp.ClientOptions{}
	if events != nil {
		options.ResourceUpdatedHandler = func(_ context.Context, r *mcp.ResourceUpdatedNotificationRequest) { events <- r.Params.URI }
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "refresh-fixture", Version: "1"}, options)
	client.AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, r mcp.Request) (mcp.Result, error) {
			if method == "server/discover" {
				return nil, &jsonrpc.Error{Code: -32601, Message: "legacy fixture"}
			}
			return next(ctx, method, r)
		}
	})
	// The fixture resource must use the actual HTTP server origin.
	app.oauth.Mu.Lock()
	for k, g := range app.oauth.Access {
		g.Resource = server.URL + "/mcp"
		app.oauth.Access[k] = g
	}
	for k, g := range app.oauth.Refresh {
		g.Resource = server.URL + "/mcp"
		app.oauth.Refresh[k] = g
	}
	app.oauth.Mu.Unlock()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: transport}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	if session.InitializeResult().ProtocolVersion != "2025-11-25" {
		t.Fatal("fixture did not negotiate legacy protocol")
	}
	return session, ctx
}
func TestMCPLegacySessionSurvivesTokenRefresh(t *testing.T) {
	app, _ := fixture(t)
	form := seedRefresh(t, app, "session-refresh", "session-family")
	pair := decodePair(t, tokenForm(app, "/oauth/token", form))
	transport := &rotatingOAuthTransport{}
	transport.token.Store(pair.Access)
	session, ctx := oauthLegacyClient(t, app, transport, nil)
	if _, err := session.ListTools(ctx, nil); err != nil {
		t.Fatal(err)
	}
	form.Set("refresh_token", pair.Refresh)
	next := decodePair(t, tokenForm(app, "/oauth/token", form))
	transport.token.Store(next.Access)
	if _, err := session.ListTools(ctx, nil); err != nil {
		t.Fatalf("same MCP session failed after refresh: %v", err)
	}
	transport.mu.Lock()
	ids := append([]string(nil), transport.sessions...)
	transport.mu.Unlock()
	if len(ids) < 2 {
		t.Fatal("session requests missing")
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatal("refresh replaced MCP session")
		}
	}
	other := decodePair(t, tokenForm(app, "/oauth/token", seedRefresh(t, app, "other-refresh", "other-family")))
	statusFor := func(token string) int {
		request := httptest.NewRequest("POST", app.origin+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":99,"method":"tools/list","params":{}}`))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Mcp-Session-Id", ids[0])
		request.Header.Set("MCP-Protocol-Version", "2025-11-25")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		response := httptest.NewRecorder()
		app.ServeHTTP(response, request)
		return response.Code
	}
	if status := statusFor(other.Access); status != 403 {
		t.Fatal("another authorization must not take over the session", status)
	}
	if err := app.oauth.RevokeToken(next.Refresh); err != nil {
		t.Fatal(err)
	}
	if status := statusFor(next.Access); status != 401 {
		t.Fatal("revoked authorization must fail admission", status)
	}
}
func TestSubscriptionSurvivesTokenRefresh(t *testing.T) {
	app, store := fixture(t)
	form := seedRefresh(t, app, "subscription-refresh", "subscription-family")
	pair := decodePair(t, tokenForm(app, "/oauth/token", form))
	transport := &rotatingOAuthTransport{}
	transport.token.Store(pair.Access)
	events := make(chan string, 4)
	session, ctx := oauthLegacyClient(t, app, transport, events)
	if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: revisionURI("default")}); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, app, 1)
	form.Set("refresh_token", pair.Refresh)
	next := decodePair(t, tokenForm(app, "/oauth/token", form))
	transport.token.Store(next.Access)
	app.oauth.Mu.Lock()
	old := app.oauth.Access[digest(pair.Access)]
	old.Expires = time.Now().Add(-time.Second).Unix()
	app.oauth.Access[digest(pair.Access)] = old
	app.oauth.Mu.Unlock()
	if _, err := store.Save(ctx, "refreshed.md", []byte("after refresh"), "", "test"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
	case <-time.After(3 * time.Second):
		t.Fatal("subscription stopped with the original access token")
	}
	if err := app.oauth.RevokeToken(next.Refresh); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, app, 0)
}

func TestRefreshReplayAfterGraceRevokesFamily(t *testing.T) {
	app, _ := fixture(t)
	form := seedRefresh(t, app, "replayed-refresh", "replay-family")
	pair := decodePair(t, tokenForm(app, "/oauth/token", form))
	app.oauth.Mu.Lock()
	used := app.oauth.Used[digest("replayed-refresh")]
	used.UsedAt = time.Now().Add(-31 * time.Second).Unix()
	app.oauth.Used[digest("replayed-refresh")] = used
	app.oauth.Mu.Unlock()
	w := tokenForm(app, "/oauth/token", form)
	if w.Code != 400 || strings.TrimSpace(w.Body.String()) != `{"error":"invalid_grant"}` {
		t.Fatalf("replay response %d %s", w.Code, w.Body)
	}
	if _, ok := app.oauth.Access[digest(pair.Access)]; ok {
		t.Fatal("replay retained successor")
	}
	organization, err := app.catalog.Organization("@local-admin")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range organization.Activity {
		if event.Action == "oauth.refresh_replay" {
			found = true
			if event.Outcome != "revoked" || event.Target != "refresh-client" || event.Details["family"] != digest("replay-family")[:12] {
				t.Fatal("incorrect replay audit event")
			}
			raw, _ := json.Marshal(event)
			for _, secret := range []string{"replayed-refresh", pair.Access, pair.Refresh, "replay-family"} {
				if strings.Contains(string(raw), secret) {
					t.Fatal("token leaked into audit")
				}
			}
		}
	}
	if !found {
		t.Fatal("replay not recorded")
	}
	reopened, err := reopenOAuth(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.Access) != 0 || len(reopened.Refresh) != 0 {
		t.Fatal("revocation did not survive restart")
	}
}

func TestRefreshReplayAuditRecordsTokenAgeAndPriorRevocation(t *testing.T) {
	app, _ := fixture(t)
	form := seedRefresh(t, app, "stale-refresh", "stale-family")
	pair := decodePair(t, tokenForm(app, "/oauth/token", form))
	app.oauth.Mu.Lock()
	used := app.oauth.Used[digest("stale-refresh")]
	used.UsedAt = time.Now().Add(-17 * time.Minute).Unix()
	app.oauth.Used[digest("stale-refresh")] = used
	app.oauth.Mu.Unlock()
	if w := tokenForm(app, "/oauth/token", form); w.Code != 400 {
		t.Fatal("stale replay accepted", w.Code)
	}
	successor := form.Clone()
	successor.Set("refresh_token", pair.Refresh)
	if w := tokenForm(app, "/oauth/token", successor); w.Code != 400 {
		t.Fatal("revoked successor accepted", w.Code)
	}
	organization, err := app.catalog.Organization("@local-admin")
	if err != nil {
		t.Fatal(err)
	}
	var replays []map[string]string
	for _, event := range organization.Activity {
		if event.Action == "oauth.refresh_replay" {
			replays = append(replays, event.Details)
		}
	}
	// Activity is newest first: the revoked successor, then the stale original.
	if len(replays) != 2 {
		t.Fatalf("recorded %d replays", len(replays))
	}
	successorDetails, staleDetails := replays[0], replays[1]
	if age, err := strconv.ParseInt(staleDetails["usedAgeSeconds"], 10, 64); err != nil || age < 17*60 || age > 17*60+5 {
		t.Fatalf("stale replay age %q", staleDetails["usedAgeSeconds"])
	}
	if staleDetails["chainAlreadyRevoked"] != "false" {
		t.Fatalf("stale replay found chain revoked: %v", staleDetails)
	}
	if _, ok := successorDetails["usedAgeSeconds"]; ok || successorDetails["chainAlreadyRevoked"] != "true" {
		t.Fatalf("unused successor of a revoked chain: %v", successorDetails)
	}
}

func TestGraceDoesNotReviveRevokedFamily(t *testing.T) {
	app, _ := fixture(t)
	form := seedRefresh(t, app, "revoked-refresh", "revoked-family")
	pair := decodePair(t, tokenForm(app, "/oauth/token", form))
	if w := tokenForm(app, "/oauth/revoke", url.Values{"token": {pair.Refresh}}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := tokenForm(app, "/oauth/token", form); w.Code != 400 {
		t.Fatal("grace revived revoked authorization", w.Code)
	}
	if len(app.oauth.Refresh) != 0 || len(app.oauth.Access) != 0 {
		t.Fatal("revoked family regained access")
	}
}

func TestFailedRefreshValidationAndPersistenceKeepToken(t *testing.T) {
	app, _ := fixture(t)
	form := seedRefresh(t, app, "unchanged-refresh", "unchanged-family")
	wrong := form.Clone()
	wrong.Set("resource", "https://other.example/mcp")
	if w := tokenForm(app, "/oauth/token", wrong); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if _, ok := app.oauth.Refresh[digest("unchanged-refresh")]; !ok {
		t.Fatal("invalid resource consumed token")
	}
	restore := pgtest.FailWrites(t, app.db, "oauth")
	if w := tokenForm(app, "/oauth/token", form); w.Code < 500 {
		t.Fatal("persistence failure succeeded", w.Code)
	}
	if _, ok := app.oauth.Refresh[digest("unchanged-refresh")]; !ok {
		t.Fatal("persistence failure consumed token")
	}
	if len(app.oauth.Used) != 0 || len(app.oauth.Access) != 0 {
		t.Fatal("persistence failure retained partial rotation")
	}
	restore()
	decodePair(t, tokenForm(app, "/oauth/token", form))
}

// Direct token fixtures still represent a distinct, bounded OAuth authorization.
func withOAuthFamily(g accessGrant) accessGrant {
	if g.Family == "" {
		g.Family = RandomToken()
	}
	if g.FamilyExpires == 0 {
		g.FamilyExpires = max(time.Now().Add(90*24*time.Hour).Unix(), g.Expires)
	}
	return g
}

func TestOAuthSourceScopeSurvivesTokenRefresh(t *testing.T) {
	app, _ := fixture(t)
	form := seedRefresh(t, app, "source-refresh", "source-family")
	pair := decodePair(t, tokenForm(app, "/oauth/token", form))
	request := httptest.NewRequest("POST", app.origin+"/mcp", nil)
	request.Header.Set("Authorization", "Bearer "+pair.Access)
	if !app.validAccessToken(request) {
		t.Fatal("OAuth fixture rejected")
	}
	scope := app.sourceScope(request.Context(), "default")
	if _, err := app.sourceContext(t.Context(), scope); err != nil {
		t.Fatalf("valid OAuth source job rejected: %v", err)
	}
	form.Set("refresh_token", pair.Refresh)
	next := decodePair(t, tokenForm(app, "/oauth/token", form))
	app.oauth.Mu.Lock()
	expired := app.oauth.Access[digest(pair.Access)]
	expired.Expires = time.Now().Unix() - 1
	app.oauth.Access[digest(pair.Access)] = expired
	app.oauth.Mu.Unlock()
	if _, err := app.sourceContext(t.Context(), scope); err != nil {
		t.Fatalf("source job lost consent on rotation: %v", err)
	}
	if err := app.oauth.RevokeToken(next.Refresh); err != nil {
		t.Fatal(err)
	}
	if _, err := app.sourceContext(t.Context(), scope); err == nil {
		t.Fatal("source job survived consent revocation")
	}
}

func TestRefreshGraceCrossesConsumedTokenExpiry(t *testing.T) {
	app, _ := fixture(t)
	form := seedRefresh(t, app, "near-expiry", "expiry-family")
	first := decodePair(t, tokenForm(app, "/oauth/token", form))
	app.oauth.Mu.Lock()
	prior := app.oauth.Used[digest("near-expiry")]
	prior.Expires = time.Now().Unix() - 1
	app.oauth.Used[digest("near-expiry")] = prior
	app.oauth.Mu.Unlock()
	second := decodePair(t, tokenForm(app, "/oauth/token", form))
	if got, ok := app.oauth.Used[digest("near-expiry")]; !ok || got.UsedAt != prior.UsedAt {
		t.Fatal("grace receipt expired early or its window moved")
	}
	app.oauth.Mu.Lock()
	prior.UsedAt = time.Now().Add(-31 * time.Second).Unix()
	app.oauth.Used[digest("near-expiry")] = prior
	app.oauth.Mu.Unlock()
	if w := tokenForm(app, "/oauth/token", form); w.Code != 400 {
		t.Fatal("expired consumed token outlived grace", w.Code)
	}
	for _, token := range []string{first.Refresh, second.Refresh} {
		next := form.Clone()
		next.Set("refresh_token", token)
		decodePair(t, tokenForm(app, "/oauth/token", next))
	}
}
