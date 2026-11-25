package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Management consent lets a session mint credentials and change access, so an
// ordinary CLI sign-in must not request it.
func TestLoginRequestsManagementConsentOnlyWhenAsked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/oauth/register" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(201)
		fmt.Fprint(w, `{"client_id":"cli"}`)
	}))
	defer server.Close()
	for manage, want := range map[bool]string{false: "notes:read notes:write", true: "notes:read notes:write notes:manage"} {
		ctx, cancel := context.WithCancel(context.Background())
		diagnostics := &authorizationURL{cancel: cancel}
		if _, err := login(ctx, server.URL, true, diagnostics, "", manage); err != context.Canceled {
			t.Fatalf("login(manage=%v) = %v, want cancellation after printing the URL", manage, err)
		}
		cancel()
		if diagnostics.scope != want {
			t.Fatalf("login(manage=%v) scope = %q, want %q", manage, diagnostics.scope, want)
		}
	}
}

type authorizationURL struct {
	cancel context.CancelFunc
	scope  string
}

func (a *authorizationURL) Write(p []byte) (int, error) {
	for _, line := range strings.Split(string(p), "\n") {
		if u, err := url.Parse(strings.TrimSpace(line)); err == nil && strings.HasSuffix(u.Path, "/oauth/authorize") {
			a.scope = u.Query().Get("scope")
			a.cancel()
		}
	}
	return len(p), nil
}
