package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// discoveryProvider serves only OIDC metadata, counting each request; a
// request waits for gate and fails while failing is set.
type discoveryProvider struct {
	server      *httptest.Server
	discoveries atomic.Int32
	failing     atomic.Bool
	gate        chan struct{}
}

func newDiscoveryProvider(t *testing.T) *discoveryProvider {
	p := &discoveryProvider{gate: make(chan struct{})}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		p.discoveries.Add(1)
		<-p.gate
		if p.failing.Load() {
			http.Error(w, "identity provider down", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"issuer": p.server.URL, "authorization_endpoint": p.server.URL + "/authorize", "token_endpoint": p.server.URL + "/token", "jwks_uri": p.server.URL + "/keys", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
	}))
	t.Cleanup(p.server.Close)
	t.Cleanup(func() {
		select {
		case <-p.gate:
		default:
			close(p.gate)
		}
	})
	return p
}

func TestOIDCPageLoadsShareOneDiscoveryOutsideTheLock(t *testing.T) {
	app, _ := fixture(t)
	p := newDiscoveryProvider(t)
	if err := app.ConfigureOIDC(p.server.URL, "test-client", "test-secret"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := app.oidc.config(context.Background(), app.origin)
			errs <- err
		}()
	}
	for p.discoveries.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	// A caller that stops waiting returns while discovery is still in flight.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := app.oidc.config(ctx, app.origin); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("abandoned wait: %v", err)
	}
	close(p.gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal("shared discovery:", err)
		}
	}
	if _, config, err := app.oidc.config(context.Background(), app.origin); err != nil || config.Endpoint.AuthURL != p.server.URL+"/authorize" {
		t.Fatalf("discovered provider: %v %q", err, config.Endpoint.AuthURL)
	}
	if n := p.discoveries.Load(); n != 1 {
		t.Fatalf("%d discoveries for concurrent page loads", n)
	}
}

func TestOIDCDiscoveryFailureIsReportedForThirtySeconds(t *testing.T) {
	app, _ := fixture(t)
	p := newDiscoveryProvider(t)
	close(p.gate)
	p.failing.Store(true)
	if err := app.ConfigureOIDC(p.server.URL, "test-client", "test-secret"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		w := call(app, "GET", "/login", "", "")
		if w.Code != 200 {
			t.Fatalf("login page during outage: %d", w.Code)
		}
	}
	if w := oidcRequest(app, "POST", "/auth/oidc/login", ""); w.Code != 502 {
		t.Fatalf("sign-in during outage: %d %s", w.Code, w.Body.String())
	}
	if n := p.discoveries.Load(); n != 1 {
		t.Fatalf("%d discoveries during the outage", n)
	}
	app.oidc.mu.Lock()
	retry := time.Until(app.oidc.retry)
	app.oidc.mu.Unlock()
	if retry <= 25*time.Second || retry > oidcRetry {
		t.Fatalf("failure reported for %v", retry)
	}

	// After the window the provider is asked again, and its recovery is kept.
	p.failing.Store(false)
	app.oidc.mu.Lock()
	app.oidc.retry = time.Now()
	app.oidc.mu.Unlock()
	if _, _, err := app.oidc.config(context.Background(), app.origin); err != nil {
		t.Fatal("recovered provider:", err)
	}
	call(app, "GET", "/login", "", "")
	if n := p.discoveries.Load(); n != 2 {
		t.Fatalf("%d discoveries after recovery", n)
	}
}
