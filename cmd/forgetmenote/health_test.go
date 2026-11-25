package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHealthCommandProbesWithoutOpeningState(t *testing.T) {
	data := filepath.Join(t.TempDir(), "untouched")
	t.Setenv("DATA_DIR", data)
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/healthz" || r.Header.Get("Authorization") != "" {
			t.Error("unexpected health request")
		}
		w.Write([]byte("ok\n"))
	}))
	defer server.Close()
	if err := runArguments([]string{"healthz", "-url", server.URL + "/healthz"}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("health command did not contact the running application")
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("health command opened application state", err)
	}
}

func TestHealthCommandRejectsRedirectsAndInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{{"failure", 503, "private diagnostic"}, {"redirect", 302, "ok"}, {"invalid", 200, "private error"}, {"oversized", 200, strings.Repeat(" ", 1025) + "ok"}} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "https://example.test/private")
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer server.Close()
			err := runArguments([]string{"healthz", "-url", server.URL + "/healthz"})
			if err == nil || strings.Contains(err.Error(), "private") {
				t.Fatal("invalid response or diagnostic disclosure", err)
			}
		})
	}
}
