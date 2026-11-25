package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/pg/pgtest"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A legacy client keeps its session while its requests move between gateways
// and while the gateway holding its stream shuts down.
func TestLegacyMCPSessionMovesBetweenGateways(t *testing.T) {
	app, store := fixture(t)
	other := gatewayOf(t, app)
	var current atomic.Pointer[Server]
	current.Store(app)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { current.Load().ServeHTTP(w, r) }))
	defer ts.Close()
	app.origin, other.origin = ts.URL, ts.URL
	events := make(chan string, 8)
	client := mcp.NewClient(&mcp.Implementation{Name: "legacy-gateways", Version: "1"}, &mcp.ClientOptions{ResourceUpdatedHandler: func(_ context.Context, r *mcp.ResourceUpdatedNotificationRequest) { events <- r.Params.URI }})
	client.AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, r mcp.Request) (mcp.Result, error) {
			if method == "server/discover" {
				return nil, &jsonrpc.Error{Code: -32601, Message: "legacy client fixture"}
			}
			return next(ctx, method, r)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: tokenTransport{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if v := session.InitializeResult().ProtocolVersion; v != "2025-11-25" {
		t.Fatal("expected legacy protocol", v)
	}
	// Another gateway recreates the session for the requests it receives.
	current.Store(other)
	if _, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "project_status", Arguments: map[string]any{"project": "default"}}); err != nil {
		t.Fatal("another gateway refused the session's request:", err)
	}
	if err = session.Subscribe(ctx, &mcp.SubscribeParams{URI: revisionURI("default")}); err != nil {
		t.Fatal(err)
	}
	// The stream still open on the first gateway delivers that subscription.
	waitWatch(t, app, 1)
	if _, err = store.Save(ctx, "moved.md", []byte("Subscribed elsewhere"), "", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
	case <-ctx.Done():
		t.Fatal("the stream did not deliver a subscription made through another gateway")
	}
	// The first gateway shuts down; the client's stream reconnects to the other.
	app.revisions.close()
	waitWatch(t, other, 1)
	status, _ := store.Status(ctx)
	if _, err = store.Save(ctx, "moved.md", []byte("After the deploy"), status.Revision, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
	case <-ctx.Done():
		t.Fatal("the reconnected stream did not deliver notifications")
	}
	if err = session.Close(); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, other, 0)
	var stored int
	if err = app.db.Pool.QueryRow(ctx, `SELECT count(*) FROM mcp_sessions WHERE org = $1`, app.db.Org).Scan(&stored); err != nil || stored != 0 {
		t.Fatal("a closed session remained stored", stored, err)
	}
}

const legacyInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"bound","version":"1"}}}`

// legacyRequest POSTs body to gateway as a 2025-11-25 client holding token.
func legacyRequest(gateway *Server, token, session, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", gateway.origin+"/mcp", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2025-11-25")
	if session != "" {
		r.Header.Set("Mcp-Session-Id", session)
	}
	w := httptest.NewRecorder()
	gateway.ServeHTTP(w, r)
	return w
}

// A stored session serves only the credential that began it, on any gateway.
func TestLegacyMCPSessionRefusesAnotherCredential(t *testing.T) {
	app, _ := fixture(t)
	other := gatewayOf(t, app)
	app.catalog.Member("@local-admin", "default", "alice", "edit")
	app.oauth.Access[digest("alice-access")] = withOAuthFamily(accessGrant{Subject: "alice", Client: "test-client", Scope: "notes:read", Resource: app.origin + "/mcp", Expires: time.Now().Add(time.Hour).Unix()})
	if err := app.oauth.Persist(); err != nil {
		t.Fatal(err)
	}
	initialized := legacyRequest(app, testToken, "", legacyInitialize)
	id := initialized.Header().Get("Mcp-Session-Id")
	if initialized.Code != 200 || id == "" {
		t.Fatalf("initialize: %d %s", initialized.Code, initialized.Body)
	}
	list := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
	if w := legacyRequest(other, testToken, id, list); w.Code != 200 || !strings.Contains(w.Body.String(), `"tools"`) {
		t.Fatalf("another gateway did not serve the session: %d %s", w.Code, w.Body)
	}
	for _, gateway := range []*Server{app, other} {
		if w := legacyRequest(gateway, "alice-access", id, list); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "session user mismatch") {
			t.Fatalf("another credential used the session: %d %s", w.Code, w.Body)
		}
	}
	if w := legacyRequest(other, testToken, "unknown-session", list); w.Code != http.StatusNotFound {
		t.Fatalf("an unknown session was served: %d", w.Code)
	}
}

// blockingCall makes list_projects on app wait for its context to end. The
// returned channels close when the call enters and when it sees cancellation.
func blockingCall(t *testing.T, app *Server) (entered, cancelled chan struct{}) {
	entered, cancelled = make(chan struct{}), make(chan struct{})
	addAction(app, app.revisions.server, &mcp.Tool{Name: "list_projects", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ struct{}) (actionResult, error) {
		close(entered)
		select {
		case <-ctx.Done():
			close(cancelled)
		case <-t.Context().Done():
		}
		return actionResult{}, ctx.Err()
	})
	return entered, cancelled
}

// rpcID returns the JSON-RPC ID of r's message and leaves its body to be read
// again.
func rpcID(r *http.Request) json.RawMessage {
	raw, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var envelope struct {
		ID json.RawMessage `json:"id"`
	}
	json.Unmarshal(raw, &envelope)
	return envelope.ID
}

func waitClosed(t *testing.T, done chan struct{}, within time.Duration, failure string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatal(failure)
	}
}

// A legacy client's cancellation reaches its call on the gateway running it,
// whether the load balancer sends the cancellation there or to another gateway.
func TestLegacyMCPCancellationReachesTheGatewayRunningTheCall(t *testing.T) {
	for _, through := range []string{"the same gateway", "another gateway"} {
		t.Run(through, func(t *testing.T) {
			app, _ := fixture(t)
			receiver := app
			if through == "another gateway" {
				receiver = gatewayOf(t, app)
			}
			entered, cancelled := blockingCall(t, app)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if admissionRPCMethod(r) == "notifications/cancelled" {
					receiver.ServeHTTP(w, r)
					return
				}
				app.ServeHTTP(w, r)
			}))
			t.Cleanup(ts.Close)
			receiver.origin = ts.URL
			// A gateway registers its tools on its first MCP request, which is
			// not what the bound below measures.
			if w := legacyRequest(receiver, testToken, "", legacyInitialize); w.Code != http.StatusOK {
				t.Fatalf("initialize: %d %s", w.Code, w.Body)
			}
			client := legacyAdmissionConnect(t, app, ts)
			pgtest.Listening(t, app.db, mcpCancellations)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go client.CallTool(ctx, &mcp.CallToolParams{Name: "list_projects", Arguments: map[string]any{}})
			waitClosed(t, entered, 5*time.Second, "the call did not start")
			cancel()
			waitClosed(t, cancelled, time.Second, "the call did not see a cancellation sent through "+through)
		})
	}
}

// Another credential cancels no call of a legacy session: not by naming the
// session, and not by naming the call's request in a session of its own.
func TestLegacyMCPCancellationNeedsTheSessionsCredential(t *testing.T) {
	app, _ := fixture(t)
	other := gatewayOf(t, app)
	entered, cancelled := blockingCall(t, app)
	requests := make(chan json.RawMessage, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch admissionRPCMethod(r) {
		case "tools/call":
			requests <- rpcID(r)
		case "notifications/cancelled":
			other.ServeHTTP(w, r)
			return
		}
		app.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	other.origin = ts.URL
	client := legacyAdmissionConnect(t, app, ts)
	pgtest.Listening(t, app.db, mcpCancellations)
	app.catalog.Member("@local-admin", "default", "alice", "edit")
	app.oauth.Access[digest("alice-access")] = withOAuthFamily(accessGrant{Subject: "alice", Client: "test-client", Scope: "notes:read", Resource: ts.URL + "/mcp", Expires: time.Now().Add(time.Hour).Unix()})
	if err := app.oauth.Persist(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go client.CallTool(ctx, &mcp.CallToolParams{Name: "list_projects", Arguments: map[string]any{}})
	waitClosed(t, entered, 5*time.Second, "the call did not start")
	cancellation := `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":` + string(<-requests) + `}}`
	for _, gateway := range []*Server{app, other} {
		if w := legacyRequest(gateway, "alice-access", client.ID(), cancellation); w.Code != http.StatusForbidden {
			t.Fatalf("another credential cancelled through the session: %d %s", w.Code, w.Body)
		}
	}
	// Alice's own session lives on the gateway running the call too.
	own := legacyRequest(app, "alice-access", "", legacyInitialize).Header().Get("Mcp-Session-Id")
	if w := legacyRequest(other, "alice-access", own, cancellation); w.Code != http.StatusAccepted {
		t.Fatalf("a cancellation in alice's own session: %d %s", w.Code, w.Body)
	}
	select {
	case <-cancelled:
		t.Fatal("another credential's cancellation reached the session's call")
	case <-time.After(time.Second):
	}
	cancel()
	waitClosed(t, cancelled, time.Second, "the session's own cancellation did not reach its call")
}
