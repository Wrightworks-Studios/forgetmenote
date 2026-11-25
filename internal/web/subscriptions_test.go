package web

import (
	"context"
	"fmt"
	"forgetmenote/internal/catalog"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestMCPRevisionResource(t *testing.T) {
	app, store := fixture(t)
	ts := httptest.NewServer(app)
	defer ts.Close()
	app.origin = ts.URL
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: tokenTransport{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if caps := session.InitializeResult().Capabilities.Resources; caps == nil || !caps.Subscribe {
		t.Fatal("revision subscription capability missing")
	}
	for _, uri := range []string{"forgetmenote://notebooks/default/revision", "forgetmenote://projects/default/revision"} {
		result, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if err != nil || len(result.Contents) != 1 {
			t.Fatalf("revision resource %s: %v %v", uri, result, err)
		}
	}
	_ = store
}

func TestMCPNotificationsCommitFailureUnsubscribeAndRevocation(t *testing.T) {
	app, store := fixture(t)
	app.catalog.Member("@local-admin", "default", "alice", "edit")
	app.oauth.Access[digest("alice-access")] = withOAuthFamily(accessGrant{Subject: "alice", Client: "test-client", Scope: "notes:read notes:write", Resource: app.origin + "/mcp", Expires: time.Now().Add(time.Hour).Unix()})
	ts := httptest.NewServer(app)
	defer ts.Close()
	app.origin = ts.URL
	g := app.oauth.Access[digest("alice-access")]
	g.Resource = app.origin + "/mcp"
	app.oauth.Access[digest("alice-access")] = g
	app.oauth.Refresh[digest("alice-refresh")] = g
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	notifications := make(chan string, 16)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ClientOptions{ResourceUpdatedHandler: func(ctx context.Context, r *mcp.ResourceUpdatedNotificationRequest) { notifications <- r.Params.URI }})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: "alice-access"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	t.Log("negotiated", session.InitializeResult().ProtocolVersion)
	if _, err = session.ReadResource(ctx, &mcp.ReadResourceParams{URI: revisionURI("secret")}); err == nil {
		t.Fatal("unauthorized read")
	}
	if err = session.Subscribe(ctx, &mcp.SubscribeParams{URI: revisionURI("default")}); err != nil {
		t.Fatal(err)
	}
	canonicalURI := "forgetmenote://notebooks/default/revision"
	if err = session.Subscribe(ctx, &mcp.SubscribeParams{URI: canonicalURI}); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, app, 1)
	doc, err := store.Save(ctx, "signal.md", []byte("committed"), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for range 2 {
		select {
		case uri := <-notifications:
			if uri != revisionURI("default") && uri != canonicalURI || seen[uri] {
				t.Fatal(uri)
			}
			seen[uri] = true
		case <-ctx.Done():
			t.Fatal("no committed notification for each subscribed alias")
		}
	}
	resource, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: revisionURI("default")})
	if err != nil || !strings.Contains(resource.Contents[0].Text, doc.Revision) {
		t.Fatal("notification preceded readable commit", err)
	}
	if _, err = store.Save(ctx, "signal.md", []byte("stale"), "", "test"); err == nil {
		t.Fatal("expected conflict")
	}
	select {
	case <-notifications:
		t.Fatal("failed write notified")
	case <-time.After(200 * time.Millisecond):
	}
	if err = session.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: revisionURI("default")}); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, app, 1)
	if err = session.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: canonicalURI}); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, app, 0)
	doc, err = store.Save(ctx, "signal.md", []byte("after unsubscribe"), doc.Revision, "test")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-notifications:
		t.Fatal("unsubscribed delivery")
	case <-time.After(200 * time.Millisecond):
	}
	session.Close()
	session, err = client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: "alice-access"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err = session.Subscribe(ctx, &mcp.SubscribeParams{URI: revisionURI("default")}); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, app, 1)
	app.oauth.Mu.Lock()
	app.oauth.RevokeFamily(app.oauth.Access[digest("alice-access")].Family)
	app.oauth.Mu.Unlock()
	if _, err = store.Save(ctx, "signal.md", []byte("after revoke"), doc.Revision, "test"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-notifications:
		t.Fatal("revoked delivery")
	case <-time.After(1200 * time.Millisecond):
	}
	app.revisions.mu.Lock()
	n := len(app.revisions.projects)
	app.revisions.mu.Unlock()
	if n != 0 {
		t.Fatal("revoked subscriptions retained", n)
	}
	if _, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "project_status", Arguments: map[string]any{"project": "default"}}); err == nil {
		t.Fatal("revoked read allowed")
	}
}

func waitWatch(t *testing.T, app *Server, want int) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		app.revisions.mu.Lock()
		n := len(app.revisions.projects)
		app.revisions.mu.Unlock()
		if n == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("subscription count did not reach %d", want)
}

func TestLegacyMCPSubscriptionsThroughReverseProxy(t *testing.T) {
	app, store := fixture(t)
	upstream := httptest.NewServer(app)
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	proxy := httptest.NewServer(httputil.NewSingleHostReverseProxy(target))
	defer proxy.Close()
	app.origin = proxy.URL
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	events := make(chan string, 4)
	client := mcp.NewClient(&mcp.Implementation{Name: "legacy-fixture", Version: "1"}, &mcp.ClientOptions{ResourceUpdatedHandler: func(ctx context.Context, r *mcp.ResourceUpdatedNotificationRequest) { events <- r.Params.URI }})
	client.AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, r mcp.Request) (mcp.Result, error) {
			if method == "server/discover" {
				return nil, &jsonrpc.Error{Code: -32601, Message: "legacy client fixture"}
			}
			return next(ctx, method, r)
		}
	})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: proxy.URL + "/mcp", HTTPClient: &http.Client{Transport: tokenTransport{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if v := session.InitializeResult().ProtocolVersion; v != "2025-11-25" {
		t.Fatal("expected legacy protocol", v)
	}
	if err = session.Subscribe(ctx, &mcp.SubscribeParams{URI: revisionURI("default")}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(ctx, "legacy.md", []byte("Through the proxy"), "", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
	case <-ctx.Done():
		t.Fatal("legacy GET SSE notification did not reach client")
	}
	if err = session.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: revisionURI("default")}); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, app, 0)
	if err = session.Subscribe(ctx, &mcp.SubscribeParams{URI: revisionURI("default")}); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, app, 1)
	session.Close()
	waitWatch(t, app, 0)
}

func TestMCPStreamingWriteDeadlineBoundsSlowReaders(t *testing.T) {
	result := make(chan error, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := (&mcpStreamWriter{ResponseWriter: w}).Write(make([]byte, 32<<20))
		result <- err
	}))
	defer ts.Close()
	address := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", address)
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("slow response unexpectedly completed")
		}
	case <-time.After(2 * time.Second):
		conn.Close()
		t.Fatal("slow reader held stream writer")
	}
}

func TestActiveSubscriptionsLoseAccess(t *testing.T) {
	for _, reason := range []string{"permission", "disabled", "expired"} {
		t.Run(reason, func(t *testing.T) {
			app, store := fixture(t)
			app.catalog.Member("@local-admin", "default", "alice", "read")
			ts := httptest.NewServer(app)
			defer ts.Close()
			app.origin = ts.URL
			app.oauth.Access[digest("access")] = withOAuthFamily(accessGrant{Subject: "alice", Client: "test", Scope: "notes:read", Resource: app.origin + "/mcp", Expires: time.Now().Add(time.Hour).Unix()})
			app.oauth.Refresh[digest("refresh")] = app.oauth.Access[digest("access")]
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			events := make(chan string, 2)
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ClientOptions{ResourceUpdatedHandler: func(ctx context.Context, r *mcp.ResourceUpdatedNotificationRequest) { events <- r.Params.URI }})
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: "access"}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			session.Subscribe(ctx, &mcp.SubscribeParams{URI: revisionURI("default")})
			waitWatch(t, app, 1)
			switch reason {
			case "permission":
				app.catalog.Member("@local-admin", "default", "alice", "")
			case "disabled":
				app.catalog.SetUser("@local-admin", catalog.User{ID: "alice", Disabled: true})
			case "expired":
				app.oauth.Mu.Lock()
				g := app.oauth.Refresh[digest("refresh")]
				g.Expires = time.Now().Unix() - 1
				app.oauth.Refresh[digest("refresh")] = g
				app.oauth.Mu.Unlock()
			}
			if _, err = store.Save(ctx, "private-change.md", []byte("Private"), "", ""); err != nil {
				t.Fatal(err)
			}
			waitWatch(t, app, 0)
			select {
			case <-events:
				t.Fatal("activity delivered after " + reason)
			default:
			}
		})
	}
}
