package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func legacyAdmissionConnect(t *testing.T, app *Server, ts *httptest.Server) *mcp.ClientSession {
	t.Helper()
	app.origin = ts.URL
	client := mcp.NewClient(&mcp.Implementation{Name: "synthetic-disconnect", Version: "1"}, nil)
	client.AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "server/discover" {
				return nil, &jsonrpc.Error{Code: -32601, Message: "legacy fixture"}
			}
			return next(ctx, method, req)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: tokenTransport{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	if got := session.InitializeResult().ProtocolVersion; got != "2025-11-25" {
		t.Fatal("wrong legacy fixture", got)
	}
	return session
}

func admissionRPCMethod(r *http.Request) string {
	if r.Method != "POST" || r.URL.Path != "/mcp" {
		return ""
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var envelope struct {
		Method string `json:"method"`
	}
	json.Unmarshal(raw, &envelope)
	return envelope.Method
}

func TestLegacyMCPDisconnectKeepsExecutingReadAdmission(t *testing.T) {
	t.Setenv("FMN_LIMIT_DECODE_REQUESTS", "1")
	t.Setenv("FMN_LIMIT_READ_REQUESTS", "1")
	t.Setenv("FMN_LIMIT_WRITE_REQUESTS", "1")
	app, _ := fixture(t)
	entered, release, workDone, httpDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	addAction(app, app.revisions.server, &mcp.Tool{Name: "list_projects", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ struct{}) (actionResult, error) {
		close(entered)
		// Real handlers can finish cleanup after cancellation. Keep that actual work
		// alive until the test proves that HTTP disconnect alone cannot free its slot.
		<-release
		defer close(workDone)
		return actionResult{OK: true}, nil
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if admissionRPCMethod(r) == "tools/call" {
			defer close(httpDone)
		}
		app.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	client := legacyAdmissionConnect(t, app, ts)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "list_projects", Arguments: map[string]any{}})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("read did not enter")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("call cancellation=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client did not disconnect")
	}
	select {
	case <-httpDone:
	case <-time.After(5 * time.Second):
		t.Fatal("tool HTTP request remained open")
	}
	select {
	case <-workDone:
		t.Fatal("fixture work ended before release")
	default:
	}
	out := apiCall(app, "GET", "/api/v1/actions", ``, testToken)
	if out.Code != 429 {
		t.Fatalf("disconnected but executing legacy tool lost read admission: %d %s", out.Code, out.Body.String())
	}
	out = apiCall(app, "POST", "/api/v1/actions/save_note", `{"project":"default","path":"while-disconnected.md","content":"# Save","revision":""}`, testToken)
	if out.Code != 200 {
		t.Fatalf("detached read retained decoder or write admission: %d %s", out.Code, out.Body.String())
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-workDone:
	case <-time.After(5 * time.Second):
		t.Fatal("tool did not finish")
	}
	until := time.Now().Add(5 * time.Second)
	for len(app.rpcReadWork) != 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if len(app.rpcReadWork) != 0 {
		t.Fatal("completed detached work retained admission")
	}
	// Discovery does not re-enter the deliberately blocking fixture action.
	out = apiCall(app, "GET", "/api/v1/actions", ``, testToken)
	if out.Code != 200 {
		t.Fatalf("read admission did not recover: %d %s", out.Code, out.Body.String())
	}
}

func TestAcknowledgedLegacyNotificationStillDispatches(t *testing.T) {
	app, _ := fixture(t)
	notificationHTTPDone := make(chan struct{})
	handled := make(chan error, 1)
	app.revisions.server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "notifications/initialized" {
				// The SDK acknowledges an enqueued notification before the receiving
				// goroutine dispatches it. Make that legal ordering deterministic.
				select {
				case <-notificationHTTPDone:
				case <-time.After(5 * time.Second):
					return nil, errors.New("notification HTTP response did not finish")
				}
				result, err := next(ctx, method, req)
				handled <- err
				return result, err
			}
			return next(ctx, method, req)
		}
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if admissionRPCMethod(r) == "notifications/initialized" {
			defer close(notificationHTTPDone)
		}
		app.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	legacyAdmissionConnect(t, app, ts)
	select {
	case err := <-handled:
		if err != nil {
			t.Fatalf("valid acknowledged initialized notification was discarded: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initialized notification never dispatched")
	}
}

type delayedMCPOutput struct {
	OK      bool `json:"ok"`
	entered chan struct{}
	release chan struct{}
}

func (o delayedMCPOutput) MarshalJSON() ([]byte, error) {
	close(o.entered)
	<-o.release
	return []byte(`{"ok":true}`), nil
}
func TestDisconnectedMCPOutputEncodingRetainsReadAdmission(t *testing.T) {
	t.Setenv("FMN_LIMIT_DECODE_REQUESTS", "1")
	t.Setenv("FMN_LIMIT_READ_REQUESTS", "1")
	t.Setenv("FMN_LIMIT_WRITE_REQUESTS", "1")
	app, _ := fixture(t)
	encoding, release, httpDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	addAction(app, app.revisions.server, &mcp.Tool{Name: "list_projects", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(context.Context, struct{}) (delayedMCPOutput, error) {
		return delayedMCPOutput{OK: true, entered: encoding, release: release}, nil
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if admissionRPCMethod(r) == "tools/call" {
			defer close(httpDone)
		}
		app.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	client := legacyAdmissionConnect(t, app, ts)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "list_projects", Arguments: map[string]any{}})
		done <- err
	}()
	select {
	case <-encoding:
	case <-time.After(5 * time.Second):
		t.Fatal("SDK output encoding did not begin")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tool call did not cancel")
	}
	select {
	case <-httpDone:
	case <-time.After(5 * time.Second):
		t.Fatal("tool HTTP request remained open")
	}
	out := apiCall(app, "GET", "/api/v1/actions", ``, testToken)
	if out.Code != 429 {
		t.Fatalf("SDK still encoding disconnected output owns no read admission: %d", out.Code)
	}
	out = apiCall(app, "POST", "/api/v1/actions/save_note", `{"project":"default","path":"while-encoding.md","content":"# Save","revision":""}`, testToken)
	if out.Code != 200 {
		t.Fatalf("detached encoding consumed write/decode admission: %d %s", out.Code, out.Body.String())
	}
}

func TestMCPSelfRevocationStillReturnsSuccess(t *testing.T) {
	app, _ := fixture(t)
	issue := apiCall(app, "POST", "/api/v1/actions/create_api_credential", `{"name":"self revoke fixture","projects":["default"],"scopes":["notes:read","notes:manage"],"audiences":["api","mcp"],"days":1}`, testToken)
	if issue.Code != 200 {
		t.Fatalf("issue failed: %d", issue.Code)
	}
	var issued issuedCredential
	if err := json.Unmarshal(issue.Body.Bytes(), &issued); err != nil || issued.Token == "" {
		t.Fatal("invalid synthetic credential fixture", err)
	}
	ts := httptest.NewServer(app)
	defer ts.Close()
	app.origin = ts.URL
	client := mcp.NewClient(&mcp.Implementation{Name: "synthetic-self-revoke", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: issued.Token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "revoke_api_credential", Arguments: map[string]any{"id": issued.ID}})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("successful self-revocation was reported as a tool failure: result=%v err=%v", result, err)
	}
	revoked := apiCall(app, "GET", "/api/v1/capabilities", ``, issued.Token)
	if revoked.Code != 401 {
		t.Fatalf("self-revoked credential remains valid: %d", revoked.Code)
	}
}
