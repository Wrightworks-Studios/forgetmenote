package connectors

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOutboundClientReturnsRedirectsAndBoundsBodies(t *testing.T) {
	followed := false
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	defer other.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer upstream.Close()
	resp, err := noRedirectClient(http.DefaultTransport, 5*time.Second).Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || followed {
		t.Fatal("redirect followed", resp.StatusCode, followed)
	}
	if raw, err := readBounded(strings.NewReader("12345"), 5); err != nil || string(raw) != "12345" {
		t.Fatal("a body of exactly the limit was refused", err)
	}
	if raw, err := readBounded(strings.NewReader("123456"), 5); !errors.Is(err, errTooLarge) || raw != nil {
		t.Fatal("a body over the limit was returned truncated", string(raw), err)
	}
}
