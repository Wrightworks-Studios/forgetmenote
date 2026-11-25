package web

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/pg/pgtest"
)

func TestMCPIdentityUsesInstanceName(t *testing.T) {
	for _, tc := range []struct {
		name, server string
		legacy       bool
	}{{"TeamBot", "teambot_mcp", false}, {"Research & Design", "research_design_mcp", false}, {"Legacy Brand", "legacy_brand_mcp", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			projects, err := catalog.Open(dir, nil, pgtest.Dir(t, dir))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { projects.Close() })
			app, err := New(projects, pgtest.Org(t), dir, "http://localhost", testToken)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(app.Close)
			if err := projects.SetInstanceName(catalog.Bootstrap, tc.name); err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewServer(app)
			defer ts.Close()
			app.origin = ts.URL
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client := mcp.NewClient(&mcp.Implementation{Name: "naming-test", Version: "1"}, nil)
			if tc.legacy {
				client.AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
					return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
						if method == "server/discover" {
							return nil, &jsonrpc.Error{Code: -32601, Message: "legacy client"}
						}
						return next(ctx, method, req)
					}
				})
			}
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: tokenTransport{}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			info := session.InitializeResult().ServerInfo
			if info == nil || info.Name != tc.server || info.Title != tc.name {
				t.Errorf("instance %q: got server %+v", tc.name, info)
			}
			if len(info.Icons) != 1 || info.Icons[0].Source != ts.URL+"/static/brand/flower.svg" || info.WebsiteURL != ts.URL {
				t.Errorf("missing instance icon or origin: %+v", info)
			}
			if tc.legacy {
				if err := session.Ping(ctx, nil); err != nil {
					t.Fatalf("legacy keepalive: %v", err)
				}
				if err := session.SetLoggingLevel(ctx, &mcp.SetLoggingLevelParams{Level: "warning"}); err != nil {
					t.Fatalf("legacy logging: %v", err)
				}
			}
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_projects", Arguments: map[string]any{}})
			session.Close()
			if err != nil || result.IsError {
				t.Fatalf("list projects: %v %+v", err, result)
			}
		})
	}
}

func TestConnectAIAuthenticatedPageAndPackage(t *testing.T) {
	app, _ := fixture(t)
	for _, path := range []string{"/account/connect", "/account/connect/download/codex"} {
		w := oidcRequest(app, "GET", path, "")
		if w.Code != 401 && w.Code != 303 {
			t.Fatalf("anonymous %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	cookie := recoverySession(t, app)
	w := oidcRequest(app, "GET", "/account/connect", "", cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Connect your AI") {
		t.Fatalf("connect page: %d %s", w.Code, w.Body.String())
	}
	w = oidcRequest(app, "GET", "/account/connect/download/codex", "", cookie)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("download: %d %s", w.Code, w.Body.String())
	}
	z, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range z.File {
		if strings.Contains(f.Name, "..") || strings.HasPrefix(f.Name, "/") {
			t.Fatal(f.Name)
		}
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(testToken)) {
			t.Fatal("credential in package")
		}
		if f.Name == "mcp.json" && bytes.Contains(b, []byte("http://localhost/mcp")) {
			found = true
		}
	}
	if !found {
		t.Fatal("missing configured MCP endpoint")
	}
}

func TestConnectAICanonicalOriginHeadlessAndScopeDiscovery(t *testing.T) {
	app, _ := fixture(t)
	cookie := recoverySession(t, app)
	app.ConfigureHeadless(true)
	if err := app.catalog.SetInstanceName("@local-admin", `Research <script>alert(1)</script>`); err != nil {
		t.Fatal(err)
	}
	w := oidcRequest(app, "GET", "/account/connect", "", cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "revoke-sessions") || strings.Contains(w.Body.String(), "<script>alert(1)</script>") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = oidcRequest(app, "GET", "/static/connect-ai.mjs", "", cookie); w.Code != 200 {
		t.Fatal("headless setup script unavailable", w.Code)
	}
	for _, client := range []string{"codex", "claude"} {
		r := httptest.NewRequest("GET", app.origin+"/account/connect/download/"+client, nil)
		r.Header.Set("X-Forwarded-Host", "attacker.example")
		r.Header.Set("X-Forwarded-Proto", "https")
		r.AddCookie(cookie)
		out := httptest.NewRecorder()
		app.ServeHTTP(out, r)
		if out.Code != 200 {
			t.Fatal(client, out.Code, out.Body.String())
		}
		z, err := zip.NewReader(bytes.NewReader(out.Body.Bytes()), int64(out.Body.Len()))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range z.File {
			reader, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(reader)
			reader.Close()
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(b, []byte("attacker.example")) {
				t.Fatal("untrusted endpoint in download")
			}
		}
	}
	if w = oidcRequest(app, "GET", "/account/connect/download/unknown", "", cookie); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, path := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server"} {
		w = oidcRequest(app, "GET", path, "")
		if w.Code != 200 || strings.Contains(w.Body.String(), "notes:manage") || strings.Contains(w.Body.String(), "data:write") || !strings.Contains(w.Body.String(), "data:read") {
			t.Fatal("employee scope recommendation", path, w.Code, w.Body.String())
		}
	}
	if w = localRequest(app, "/account/sessions/revoke", `{}`, cookie); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = oidcRequest(app, "GET", "/account/connect/download/codex", "", cookie); w.Code != 401 {
		t.Fatal("revoked session downloaded package", w.Code)
	}
}

func TestProvisionWorkNotebookRequiresAdmin(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	user := addLocal(t, app, admin, "employee")
	w := localRequest(app, "/api/organization/action/provision_work_notebook", `{"id":"work","name":"Work","user":"`+user+`"}`, admin)
	if w.Code != 200 {
		t.Fatalf("provision: %d %s", w.Code, w.Body.String())
	}
	projects := app.catalog.List(user)
	if len(projects) != 1 || projects[0].ID != "work" || projects[0].Role != "owner" {
		t.Fatalf("not privately owned: %+v", projects)
	}
}
