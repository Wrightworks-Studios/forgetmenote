package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConnectGitHubSelectsVerifiedRepository(t *testing.T) {
	finished := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/actions/begin_github_connection":
			io.WriteString(w, `{"id":"connection","url":"https://github.com/login/oauth/authorize?state=synthetic"}`)
		case "/api/v1/actions/github_connection_status":
			io.WriteString(w, `{"ready":true,"choices":[{"id":42,"installation":7,"full_name":"example/notebook"}]}`)
		case "/api/v1/actions/finish_github_connection":
			finished = true
			io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := Client{Server: server.URL, Token: "synthetic"}
	if err := c.ConnectGitHub(context.Background(), "example/notebook", "book", "Book", true, io.Discard, io.Discard); err != nil || !finished {
		t.Fatalf("verified repository selection: %v, finished=%v", err, finished)
	}
}
