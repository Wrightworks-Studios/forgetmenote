package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResearchBraveContractAndBoundaries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/res/v1/web/search" || r.URL.Query().Get("q") != "synthetic research" || r.URL.Query().Get("count") != "3" || r.URL.Query().Get("result_filter") != "web" || r.URL.Query().Get("text_decorations") != "false" || r.Header.Get("X-Subscription-Token") != "synthetic-secret" {
			t.Errorf("wrong Brave request contract: method=%s path=%s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"web": map[string]any{"results": []any{map[string]string{"title": "Synthetic source", "url": "https://example.org/evidence", "description": "Pinned evidence candidate"}, map[string]string{"title": "Bad scheme", "url": "javascript:alert(1)", "description": "skip"}}}})
	}))
	defer srv.Close()
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("synthetic-secret\n"), 0600)
	research, err := NewResearch(ResearchConfig{Enabled: true, Format: "brave", BaseURL: srv.URL + "/res/v1/web/search", SecretFile: secret, MaxSearches: 3, MaxFetches: 5})
	if err != nil {
		t.Fatal(err)
	}
	research.client.Transport = srv.Client().Transport
	got, err := research.Search(t.Context(), "synthetic research", 3)
	if err != nil || len(got) != 1 || got[0].Title != "Synthetic source" {
		t.Fatalf("configured search unavailable: %+v %v", got, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = research.Search(ctx, "synthetic research", 3)
	if err == nil {
		t.Fatal("cancelled search succeeded")
	}
	before := calls.Load()
	_, err = research.Search(t.Context(), strings.Repeat("q", 2001), 3)
	if err == nil || calls.Load() != before {
		t.Fatal("oversized search escaped bound")
	}
}

func TestResearchNeverFollowsRedirectsOrLeaksDiagnostics(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Query().Get("q") == "redirect" {
			w.Header().Set("Location", "/credential-target")
			w.WriteHeader(307)
			return
		}
		w.WriteHeader(500)
		w.Write([]byte("synthetic-private-diagnostic credential-value"))
	}))
	defer srv.Close()
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("credential-value"), 0600)
	research, err := NewResearch(ResearchConfig{Enabled: true, Format: "brave", BaseURL: srv.URL, SecretFile: secret})
	if err != nil {
		t.Fatal(err)
	}
	research.client.Transport = srv.Client().Transport
	for _, query := range []string{"redirect", "error"} {
		before := requests.Load()
		_, err = research.Search(t.Context(), query, 2)
		if err == nil || strings.Contains(err.Error(), "credential-value") || strings.Contains(err.Error(), "private-diagnostic") {
			t.Fatalf("unsafe diagnostic: %v", err)
		}
		if requests.Load() != before+1 {
			t.Fatal("research followed an operator endpoint redirect")
		}
	}
}
