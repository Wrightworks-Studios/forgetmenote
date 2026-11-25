package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"forgetmenote/internal/webcore"
)

// requestLog captures the request lines that ServeHTTP logs during a test.
func requestLog(t *testing.T) func() []map[string]any {
	t.Helper()
	var logs lockedLogBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return func() []map[string]any {
		var entries []map[string]any
		for _, line := range strings.Split(logs.String(), "\n") {
			var entry map[string]any
			if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == "request" {
				entries = append(entries, entry)
			}
		}
		return entries
	}
}

func TestRequestLogsNameTheRouteOfMCPAndActionRequests(t *testing.T) {
	app, _ := fixture(t)
	entries := requestLog(t)
	call(app, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, testToken)
	apiCall(app, "GET", "/api/v1/capabilities", "", testToken)
	apiCall(app, "GET", "/api/v1/actions", "", testToken)
	apiCall(app, "POST", "/api/v1/actions/list_projects", `{}`, testToken)
	apiCall(app, "POST", "/api/v1/actions/private-unknown-action", `{}`, testToken)
	apiCall(app, "GET", "/api/v1/private-unknown-path", "", testToken)
	call(app, "GET", "/api/status", "", testToken)
	call(app, "GET", "/oauth/private-unknown-path", "", "")
	var routes []string
	for _, entry := range entries() {
		routes = append(routes, entry["method"].(string)+" "+entry["route"].(string))
	}
	want := []string{
		// The test helper initializes the MCP session before its request.
		"POST /mcp", "POST /mcp",
		"GET GET /api/v1/capabilities",
		"GET GET /api/v1/actions",
		"POST POST /api/v1/actions/{action}",
		"POST POST /api/v1/actions/{action}",
		"GET /api/v1/",
		"GET GET /api/status",
		"GET unmatched/auth",
	}
	if strings.Join(routes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("logged routes:\n%s", strings.Join(routes, "\n"))
	}
	if raw, _ := json.Marshal(entries()); strings.Contains(string(raw), "private-") {
		t.Fatalf("logged a request path: %s", raw)
	}
}

func TestServerErrorsLogTheirCause(t *testing.T) {
	app, _ := fixture(t)
	p := newDiscoveryProvider(t)
	close(p.gate)
	p.failing.Store(true)
	if err := app.ConfigureOIDC(p.server.URL, "test-client", "test-secret"); err != nil {
		t.Fatal(err)
	}
	entries := requestLog(t)
	// A handler's error and a handler's plain error text both name the cause.
	call(app, "GET", "/api/managed-backup/choices", "", testToken)
	oidcRequest(app, "POST", "/auth/oidc/login", "")
	call(app, "POST", "/api/comments", `{"path":`, testToken)
	var causes []string
	for _, entry := range entries() {
		cause, _ := entry["error"].(string)
		causes = append(causes, entry["route"].(string)+" "+strings.TrimSuffix(strings.TrimSuffix(cause, "; try again or use recovery access"), "\n"))
	}
	want := []string{
		"GET /api/managed-backup/choices managed backups unavailable",
		"POST /auth/oidc/login Identity provider unavailable",
		"POST /api/comments ",
	}
	if strings.Join(causes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("logged causes:\n%s", strings.Join(causes, "\n"))
	}
}

func TestServerErrorLogsOmitCredentialsAndIdentifiers(t *testing.T) {
	app, _ := fixture(t)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	// A failed fetch's *url.Error keeps the query string and username.
	signed := strings.Replace(gone.URL, "://", "://reader:hunter2@", 1) + "/bucket/doc.pdf?X-Amz-Signature=SECRET-SIG#frag"
	app.mux.HandleFunc("GET /test-capture", func(w http.ResponseWriter, r *http.Request) {
		_, err := http.Get(signed)
		webcore.Fail(w, fmt.Errorf("source capture failed: %w", err))
	})
	app.mux.HandleFunc("GET /test-text", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "owner alice@example.com refused Authorization: Bearer SECRET-BEARER with password=SECRET-PASSWORD "+strings.Repeat("x", 180)+" retry https://owner.example/hook/SECRET-HOOK?token=SECRET-TOKEN", 502)
	})
	entries := requestLog(t)
	if w := call(app, "GET", "/test-capture", "", testToken); w.Code != 500 {
		t.Fatalf("capture: %d %s", w.Code, w.Body)
	}
	if w := call(app, "GET", "/test-text", "", testToken); w.Code != 502 {
		t.Fatalf("text: %d %s", w.Code, w.Body)
	}
	var causes []string
	for _, entry := range entries() {
		cause, _ := entry["error"].(string)
		causes = append(causes, cause)
	}
	if len(causes) != 2 {
		t.Fatalf("logged causes: %q", causes)
	}
	host := strings.TrimPrefix(gone.URL, "http://")
	if want := `source capture failed: Get "http://` + host + `/…": dial tcp ` + host + `: connect: connection refused`; causes[0] != want {
		t.Fatalf("fetch cause:\n%s\nwant:\n%s", causes[0], want)
	}
	if want := "owner <email> refused Authorization: Bearer … with password=… " + strings.Repeat("x", 180) + " retry"; causes[1] != want {
		t.Fatalf("text cause:\n%s\nwant:\n%s", causes[1], want)
	}
	for _, secret := range []string{"SECRET", "hunter2", "reader", "alice", "bucket", "Amz", "frag"} {
		if raw, _ := json.Marshal(entries()); strings.Contains(string(raw), secret) {
			t.Fatalf("logged %q: %s", secret, raw)
		}
	}
}

func TestFailureCausesKeepOnlyTheirDiagnosis(t *testing.T) {
	for cause, want := range map[string]string{
		`dial postgres://app:pw@db.internal:5432/fmn?sslmode=require: refused`: `dial postgres://db.internal:5432/…: refused`,
		`fetch https://hooks.slack.com/services/T0/B0/SECRET failed`:           `fetch https://hooks.slack.com/… failed`,
		`ssh://git@github.com/org/repo.git unreachable`:                        `ssh://github.com/… unreachable`,
		`connect host=db user=app password=pw dbname=fmn`:                      `connect host=db user=app password=… dbname=fmn`,
		`api_key = abc123; access_token=xyz&next=1`:                            `api_key = …; access_token=…&next=1`,
		`member bob.smith+notes@example.co.uk not found`:                       `member <email> not found`,
		`open notes/plan.md: no space left on device`:                          `open notes/plan.md: no space left on device`,
	} {
		w := &outcomeWriter{failure: errors.New(cause)}
		if got := w.failureCause(); got != want {
			t.Errorf("%s\n got %s\nwant %s", cause, got, want)
		}
	}
	long := &outcomeWriter{failure: errors.New(strings.Repeat("é", maxFailureText))}
	if got := long.failureCause(); len(got) > maxFailureText || !utf8.ValidString(got) {
		t.Fatalf("bounded cause: %d bytes", len(got))
	}
}
