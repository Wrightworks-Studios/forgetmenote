package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type fixedBearer string

func (b fixedBearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

// seedOAuthFamily stores one live access/refresh pair for an API-audience consent.
func seedOAuthFamily(app *Server, subject, scope string, lifetime time.Duration) (access, refresh string, expires int64) {
	return seedOAuthResource(app, apiPrefix, subject, scope, lifetime)
}
func seedOAuthResource(app *Server, resource, subject, scope string, lifetime time.Duration) (access, refresh string, expires int64) {
	now := time.Now().Unix()
	expires = now + int64(lifetime/time.Second)
	g := accessGrant{Subject: subject, Client: "bounded-client", Scope: scope, Resource: app.origin + resource, Family: RandomToken(), FamilyExpires: expires, Epoch: app.catalog.Epoch(subject)}
	access, refresh = RandomToken(), RandomToken()
	app.oauth.Mu.Lock()
	defer app.oauth.Mu.Unlock()
	g.Expires = now + 3600
	app.oauth.Access[digest(access)] = g
	g.Expires = expires
	app.oauth.Refresh[digest(refresh)] = g
	return access, refresh, expires
}

func TestOAuthMintedCredentialStaysWithinItsGrant(t *testing.T) {
	app, _ := fixture(t)
	alice, err := app.catalog.CreateLocalUser(catalog.Bootstrap, "alice", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", alice, "read"); err != nil {
		t.Fatal(err)
	}
	access, refresh, familyExpires := seedOAuthFamily(app, alice, "notes:read notes:manage", 48*time.Hour)

	for _, scope := range []string{"notes:write", "data:read", "data:write"} {
		body := fmt.Sprintf(`{"name":"wider","allProjects":true,"projects":[],"scopes":["notes:read",%q],"audiences":["api"],"days":30}`, scope)
		if w := apiCall(app, "POST", "/api/v1/actions/create_api_credential", body, access); w.Code != 403 {
			t.Fatalf("%s exceeded the issuing grant: %d %s", scope, w.Code, w.Body)
		}
	}
	w := apiCall(app, "POST", "/api/v1/actions/create_api_credential", `{"name":"bounded","allProjects":true,"projects":[],"scopes":["notes:read","notes:manage"],"audiences":["api","mcp"],"days":365}`, access)
	if w.Code != 200 {
		t.Fatalf("issue within grant: %d %s", w.Code, w.Body)
	}
	var issued issuedCredential
	if err := json.Unmarshal(w.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if issued.Expires > familyExpires {
		t.Fatalf("credential outlives its authorization: %d > %d", issued.Expires, familyExpires)
	}
	if w := apiCall(app, "GET", "/api/v1/capabilities", "", issued.Token); w.Code != 200 {
		t.Fatalf("bounded credential unusable: %d %s", w.Code, w.Body)
	}

	if w := tokenForm(app, "/oauth/revoke", url.Values{"token": {refresh}}); w.Code != 200 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if w := apiCall(app, "GET", "/api/v1/capabilities", "", issued.Token); w.Code != 401 {
		t.Fatalf("credential survived revocation of its authorization: %d", w.Code)
	}
	app.oauth, err = reopenOAuth(app)
	if err != nil {
		t.Fatal(err)
	}
	if w := apiCall(app, "GET", "/api/v1/capabilities", "", issued.Token); w.Code != 401 {
		t.Fatalf("credential revived after restart: %d", w.Code)
	}
}

func TestRefreshReplayEndsCredentialsMintedFromTheFamily(t *testing.T) {
	app, _ := fixture(t)
	access, refresh, _ := seedOAuthFamily(app, catalog.Bootstrap, "notes:read notes:manage", 48*time.Hour)
	w := apiCall(app, "POST", "/api/v1/actions/create_api_credential", `{"name":"replayed","projects":["default"],"scopes":["notes:read"],"audiences":["api"],"days":7}`, access)
	if w.Code != 200 {
		t.Fatalf("issue: %d %s", w.Code, w.Body)
	}
	var issued issuedCredential
	json.Unmarshal(w.Body.Bytes(), &issued)
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {"bounded-client"}, "refresh_token": {refresh}}
	decodePair(t, tokenForm(app, "/oauth/token", form))
	app.oauth.Mu.Lock()
	used := app.oauth.Used[digest(refresh)]
	used.UsedAt = time.Now().Add(-time.Minute).Unix()
	app.oauth.Used[digest(refresh)] = used
	app.oauth.Mu.Unlock()
	if w := tokenForm(app, "/oauth/token", form); w.Code != 400 {
		t.Fatalf("replay accepted: %d %s", w.Code, w.Body)
	}
	if w := apiCall(app, "GET", "/api/v1/capabilities", "", issued.Token); w.Code != 401 {
		t.Fatalf("credential survived replay revocation: %d", w.Code)
	}
}

func TestServiceCredentialStaysWithinIssuingGrant(t *testing.T) {
	app, _ := fixture(t)
	steward, err := app.catalog.CreateLocalUser(catalog.Bootstrap, "steward", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	bot, err := app.catalog.CreateService(catalog.Bootstrap, "Build bot", steward)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", bot.ID, "edit"); err != nil {
		t.Fatal(err)
	}
	access, refresh, familyExpires := seedOAuthFamily(app, catalog.Bootstrap, "notes:read notes:manage", 24*time.Hour)
	issue := func(scopes string) (int, issuedCredential) {
		body := fmt.Sprintf(`{"user":%q,"name":"Build token","projects":["default"],"scopes":%s,"audiences":["api"],"days":90}`, bot.ID, scopes)
		w := apiCall(app, "POST", "/api/v1/actions/issue_service_credential", body, access)
		var out issuedCredential
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if code, _ := issue(`["notes:read","notes:write"]`); code != 403 {
		t.Fatalf("service credential exceeded the issuing grant: %d", code)
	}
	code, issued := issue(`["notes:read"]`)
	if code != 200 || issued.Expires > familyExpires {
		t.Fatalf("bounded service credential: %d expires=%d family=%d", code, issued.Expires, familyExpires)
	}
	tokenForm(app, "/oauth/revoke", url.Values{"token": {refresh}})
	if w := apiCall(app, "GET", "/api/v1/capabilities", "", issued.Token); w.Code != 401 {
		t.Fatalf("service credential survived revocation of its issuing authorization: %d", w.Code)
	}
}

func TestBrowserAndRecoveryIssuedCredentialsKeepFullRange(t *testing.T) {
	app, _ := fixture(t)
	w := apiCall(app, "POST", "/api/v1/actions/create_api_credential", `{"name":"operator","projects":["default"],"scopes":["notes:read","data:write"],"audiences":["api"],"days":365}`, testToken)
	if w.Code != 200 {
		t.Fatalf("recovery issue: %d %s", w.Code, w.Body)
	}
	var issued issuedCredential
	json.Unmarshal(w.Body.Bytes(), &issued)
	if issued.Expires-issued.Created != 365*86400 {
		t.Fatalf("unrestricted issue was shortened: %d", issued.Expires-issued.Created)
	}
}

func TestMCPMintedCredentialStaysWithinItsGrant(t *testing.T) {
	app, _ := fixture(t)
	ts := httptest.NewServer(app)
	t.Cleanup(ts.Close)
	app.origin = ts.URL
	access, _, familyExpires := seedOAuthResource(app, "/mcp", catalog.Bootstrap, "notes:read notes:write notes:manage", 36*time.Hour)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "bounded-mcp", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: fixedBearer(access)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	issue := func(scopes []string) *mcp.CallToolResult {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "create_api_credential", Arguments: map[string]any{"name": "mcp minted", "projects": []string{"default"}, "scopes": scopes, "audiences": []string{"mcp"}, "days": 365}})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := issue([]string{"notes:read", "data:read"}); !result.IsError {
		t.Fatal("MCP grant minted a data credential it was never given")
	}
	result := issue([]string{"notes:read", "notes:write"})
	if result.IsError {
		t.Fatalf("issue within grant failed: %#v", result.Content)
	}
	raw, _ := json.Marshal(result.StructuredContent)
	var issued issuedCredential
	if err := json.Unmarshal(raw, &issued); err != nil || issued.Token == "" {
		t.Fatalf("issued credential: %v %s", err, raw)
	}
	if issued.Expires > familyExpires {
		t.Fatalf("MCP-minted credential outlives its authorization: %d > %d", issued.Expires, familyExpires)
	}
}
