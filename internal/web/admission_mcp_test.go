package web

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func admissionClient(t *testing.T, app *Server, legacy bool) *mcp.ClientSession {
	return admissionClientWithOptions(t, app, legacy, &mcp.ClientOptions{ResourceUpdatedHandler: func(context.Context, *mcp.ResourceUpdatedNotificationRequest) {}})
}
func admissionClientWithOptions(t *testing.T, app *Server, legacy bool, options *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	ts := httptest.NewServer(app)
	t.Cleanup(ts.Close)
	app.origin = ts.URL
	client := mcp.NewClient(&mcp.Implementation{Name: "synthetic-admission", Version: "1"}, options)
	if legacy {
		client.AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, r mcp.Request) (mcp.Result, error) {
				if method == "server/discover" {
					return nil, &jsonrpc.Error{Code: -32601, Message: "legacy client fixture"}
				}
				return next(ctx, method, r)
			}
		})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: tokenTransport{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}
func TestLegacyMCPReadReleasesCurrentDecodeLease(t *testing.T) {
	t.Setenv("FMN_LIMIT_DECODE_REQUESTS", "1")
	t.Setenv("FMN_LIMIT_READ_REQUESTS", "1")
	t.Setenv("FMN_LIMIT_WRITE_REQUESTS", "1")
	app, _ := fixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	addAction(app, app.revisions.server, &mcp.Tool{Name: "list_projects", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ struct{}) (actionResult, error) {
		close(entered)
		select {
		case <-release:
			return actionResult{OK: true}, nil
		case <-ctx.Done():
			return actionResult{}, ctx.Err()
		}
	})
	client := admissionClient(t, app, true)
	if got := client.InitializeResult().ProtocolVersion; got != "2025-11-25" {
		t.Fatal("wrong legacy fixture", got)
	}
	done := make(chan error, 1)
	go func() {
		result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_projects", Arguments: map[string]any{}})
		if err == nil && result.IsError {
			t.Error("read tool returned error")
		}
		done <- err
	}()
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatal("read did not enter", err)
	case <-time.After(5 * time.Second):
		t.Fatal("read did not enter")
	}
	out := apiCall(app, "POST", "/api/v1/actions/save_note", `{"project":"default","path":"during-legacy-read.md","content":"# Save during read","revision":""}`, testToken)
	if out.Code != 200 {
		t.Fatalf("legacy MCP read retained the current HTTP decode slot: status=%d body=%s", out.Code, out.Body.String())
	}
}
func TestModernMCPSubscriptionDoesNotOccupyBodyDecode(t *testing.T) {
	t.Setenv("FMN_LIMIT_DECODE_REQUESTS", "1")
	app, _ := fixture(t)
	client := admissionClient(t, app, false)
	if got := client.InitializeResult().ProtocolVersion; got < "2026-07-28" {
		t.Fatal("wrong modern fixture", got)
	}
	if err := client.Subscribe(t.Context(), &mcp.SubscribeParams{URI: revisionURI("default")}); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, app, 1)
	out := apiCall(app, "POST", "/api/v1/actions/save_note", `{"project":"default","path":"during-subscription.md","content":"# Save during subscription","revision":""}`, testToken)
	if out.Code != 200 {
		t.Fatalf("modern MCP subscription retained body-decode slot: status=%d body=%s", out.Code, out.Body.String())
	}
}

func TestMCPListChangedStreamsHaveIndependentBound(t *testing.T) {
	t.Setenv("FMN_LIMIT_DECODE_REQUESTS", "1")
	app, _ := fixture(t)
	app.rpcStreams = make(chan struct{}, 1)
	// Exercise a valid SDK list-change stream independently of resource leases.
	// The production tool inventory is currently static and disables this hint.
	server := mcp.NewServer(&mcp.Implementation{Name: "synthetic-streams", Version: "1"}, &mcp.ServerOptions{HasTools: true})
	server.AddReceivingMiddleware(app.revisions.receiving)
	app.mux = http.NewServeMux()
	app.mux.Handle("/mcp", app.subscriptionTransport(server))
	options := &mcp.ClientOptions{ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {}}
	first := admissionClientWithOptions(t, app, false, options)
	if len(app.rpcStreams) != 1 {
		t.Fatal("list-change stream did not retain independent admission")
	}
	app.revisions.mu.Lock()
	resources := len(app.revisions.sessions)
	app.revisions.mu.Unlock()
	if resources != 0 {
		t.Fatal("fixture unexpectedly used a resource subscription")
	}
	out := apiCall(app, "POST", "/api/v1/actions/save_note", `{"project":"default","path":"during-listen.md","content":"# Save","revision":""}`, testToken)
	if out.Code != 200 {
		t.Fatalf("list-change stream occupied save decoding: %d %s", out.Code, out.Body.String())
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "POST", app.origin+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":12,"method":"subscriptions/listen","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"synthetic-stream","version":"1"}},"notifications":{"toolsListChanged":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("MCP-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", "subscriptions/listen")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || !strings.Contains(string(raw), `"error":`) || !strings.Contains(string(raw), "request capacity reached") || strings.Contains(string(raw), "subscriptions/acknowledged") {
		t.Fatalf("unbounded list-change subscription: status=%d body=%s", response.StatusCode, raw)
	}

	first.Close()
	until := time.Now().Add(5 * time.Second)
	for len(app.rpcStreams) != 0 && time.Now().Before(until) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(app.rpcStreams) != 0 {
		t.Fatal("closed subscription retained its admission")
	}
	admissionClientWithOptions(t, app, false, options)
}

func TestModernMCPDiscoveryDoesNotConsumeWriteAdmission(t *testing.T) {
	t.Setenv("FMN_LIMIT_WRITE_REQUESTS", "1")
	app, _ := fixture(t)
	app.rpcWork <- struct{}{}
	defer func() { <-app.rpcWork }()
	session := admissionClient(t, app, false)
	if version := session.InitializeResult().ProtocolVersion; version < "2026-07-28" {
		t.Fatalf("write saturation displaced modern metadata discovery: %s", version)
	}
}
