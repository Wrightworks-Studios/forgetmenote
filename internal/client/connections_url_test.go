package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The CLI hands the server-supplied authorization URL to the system opener, so
// only a GitHub HTTPS page is accepted; anything else is refused before use.
func TestConnectGitHubRefusesNonGitHubAuthorizationURL(t *testing.T) {
	for _, target := range []string{"https://example.test/authorize", "http://github.com/login/oauth/authorize", "https://github.com.example.test/x", "file:///etc/hosts", "x-custom-scheme://open", "https://github.com"} {
		t.Run(target, func(t *testing.T) {
			var polled atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/actions/begin_github_connection":
					io.WriteString(w, `{"id":"connection","url":"`+target+`"}`)
				default:
					polled.Add(1)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			var diagnostics strings.Builder
			c := Client{Server: server.URL, Token: "synthetic"}
			err := c.ConnectGitHub(context.Background(), "example/notebook", "book", "Book", true, &diagnostics, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "https://github.com/") {
				t.Fatalf("unexpected authorization URL accepted: %v", err)
			}
			if polled.Load() != 0 || strings.Contains(diagnostics.String(), target) {
				t.Fatal("refused URL was still used")
			}
		})
	}
}
