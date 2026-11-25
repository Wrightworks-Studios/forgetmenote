package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientActionPreservesConflictAndNeverRetries(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/v1/actions/save_note" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("incorrect request")
		}
		w.WriteHeader(409)
		w.Write([]byte(`{"error":{"code":"conflict","message":"newer content exists"}}`))
	}))
	defer server.Close()
	c := Client{Server: server.URL, Token: "secret"}
	err := c.Call(context.Background(), "save_note", map[string]string{"path": "note.md"}, new(any))
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "conflict" || calls != 1 {
		t.Fatalf("conflict transport: %v, calls=%d", err, calls)
	}
}

func TestClientDoesNotForwardCredentialsOnRedirect(t *testing.T) {
	leaked := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer server.Close()
	c := Client{Server: server.URL, Token: "secret"}
	if err := c.Call(context.Background(), "list_projects", struct{}{}, new(any)); err == nil || leaked {
		t.Fatalf("followed credential redirect: %v", err)
	}
}
