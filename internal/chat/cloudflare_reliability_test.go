package chat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func moondreamReliabilityProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	secret := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(secret, []byte("synthetic-token"), 0600); err != nil {
		t.Fatal(err)
	}
	c := providerTestConnection(server.URL+"/ai", "cloudflare-moondream")
	c.SecretFile = secret
	c.Models, c.DefaultModel = []string{moondreamModel}, moondreamModel
	c.Capabilities = Capabilities{Images: true}
	c.Limits.RunSeconds = 45
	c.Cloudflare = &CloudflareConfig{AccountIDFile: secret, DecisionModel: "@cf/cloudflare/clef", GatewayID: "default", SearchProvider: "ceramic"}
	p, err := NewProvider(c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func moondreamReliabilityRequest() Request {
	return Request{Check: func(ctx context.Context) error { return ctx.Err() }, System: "Extract synthetic visible text.", Messages: []Message{{Role: "user", Images: []string{"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl2nGAAAAAASUVORK5CYII="}}}}
}

const moondreamReliabilityAnswer = `{"success":true,"result":{"result":{"answer":"{\"visible_text\":[]}","finish_reason":"stop","metrics":{"input_tokens":10,"output_tokens":5}},"usage":{"prompt_tokens":10,"completion_tokens":5}}}`

func TestCloudflareInferenceRecoversExplicitEngineRejection(t *testing.T) {
	calls := new(atomic.Int32)
	p := moondreamReliabilityProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if calls.Load() < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"success":false,"errors":[{"code":8005,"message":"Engine Not Ready; private upstream detail"}]}`))
			return
		}
		w.Write([]byte(moondreamReliabilityAnswer))
	})
	result, err := p.Generate(t.Context(), moondreamReliabilityRequest(), nil)
	if err != nil || calls.Load() != 3 || result.Usage.Estimated || result.Usage.InputTokens != 10 {
		t.Fatal("explicit rejected attempts should recover without invented usage", calls.Load(), result.Usage, err)
	}
}

func TestCloudflareInferenceDoesNotRetryBillingOrInvalidOutput(t *testing.T) {
	for _, billing := range []bool{true, false} {
		t.Run(map[bool]string{true: "billing", false: "invalid output"}[billing], func(t *testing.T) {
			calls := new(atomic.Int32)
			p := moondreamReliabilityProvider(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if billing {
					w.WriteHeader(http.StatusTooManyRequests)
					w.Write([]byte(`{"success":false,"errors":[{"code":3036,"message":"private account allocation"}]}`))
					return
				}
				w.Write([]byte(`{"success":true,"result":{}}`))
			})
			_, err := p.Generate(t.Context(), moondreamReliabilityRequest(), nil)
			if err == nil || calls.Load() != 1 || strings.Contains(err.Error(), "private") {
				t.Fatal("unsafe retry or diagnostic disclosure", calls.Load(), err)
			}
		})
	}
}

func TestCloudflareInferenceWaitsWithinConfiguredDeadline(t *testing.T) {
	p := moondreamReliabilityProvider(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(31 * time.Second):
			w.Write([]byte(moondreamReliabilityAnswer))
		case <-r.Context().Done():
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	if _, err := p.Generate(ctx, moondreamReliabilityRequest(), nil); err != nil {
		t.Fatal("inference was cut off before its configured deadline", err)
	}
}

func TestCloudflareInferenceRejectionsStayBoundedAndPrivate(t *testing.T) {
	for _, test := range []struct {
		name                string
		status, code, calls int
		retryAfter          string
		settled             bool
	}{
		{"engine still unavailable", 429, 8005, 3, "0", true},
		{"ambiguous server failure", 503, 0, 1, "0", false},
		{"permission denied", 403, 5018, 1, "0", true},
		{"long backoff", 429, 8005, 1, "9000000000000000000", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := new(atomic.Int32)
			p := moondreamReliabilityProvider(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", test.retryAfter)
				w.WriteHeader(test.status)
				fmt.Fprintf(w, `{"success":false,"errors":[{"code":%d,"message":"private upstream detail"}]}`, test.code)
			})
			_, err := p.Generate(t.Context(), moondreamReliabilityRequest(), nil)
			if err == nil || int(calls.Load()) != test.calls || ProviderOutcomeSettled(err) != test.settled || strings.Contains(err.Error(), "private") {
				t.Fatal(calls.Load(), err)
			}
		})
	}
}

func TestCloudflareInferenceRechecksAuthorityBeforeRetry(t *testing.T) {
	calls := new(atomic.Int32)
	p := moondreamReliabilityProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(429)
		w.Write([]byte(`{"success":false,"errors":[{"code":8005}]}`))
	})
	revoked := errors.New("private-source-canary")
	req := moondreamReliabilityRequest()
	req.Check = func(context.Context) error {
		if calls.Load() > 0 {
			return revoked
		}
		return nil
	}
	_, err := p.Generate(t.Context(), req, nil)
	if !errors.Is(err, revoked) || calls.Load() != 1 || strings.Contains(err.Error(), "private-source-canary") {
		t.Fatal("revocation did not fence another request", calls.Load(), err)
	}
}

func TestCloudflareInferenceDoesNotRetryAmbiguousTimeout(t *testing.T) {
	calls := new(atomic.Int32)
	p := moondreamReliabilityProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	result, err := p.Generate(ctx, moondreamReliabilityRequest(), nil)
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 || ProviderOutcomeSettled(err) || !result.Usage.Estimated {
		t.Fatal("ambiguous attempt was repeated or reported settled", calls.Load(), result.Usage, err)
	}
}

func TestCloudflareClefRechecksAuthorityAfterCapacityRejection(t *testing.T) {
	calls := new(atomic.Int32)
	p := moondreamReliabilityProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(429)
		w.Write([]byte(`{"success":false,"errors":[{"code":8005}]}`))
	})
	revoked := errors.New("private-source-canary")
	req := DecisionsRequest{Prompt: "Synthetic evidence", Questions: []DecisionQuestion{{Type: "choice", Name: "identity", Instructions: "Matches?", Choices: []DecisionChoice{{Value: "pass", Description: "Matches"}, {Value: "uncertain", Description: "Unknown"}}}}, Check: func(context.Context) error {
		if calls.Load() > 0 {
			return revoked
		}
		return nil
	}}
	_, err := p.Decide(t.Context(), req)
	if !errors.Is(err, revoked) || calls.Load() != 1 || strings.Contains(err.Error(), "private-source-canary") {
		t.Fatal("Clef revocation did not fence another request", calls.Load(), err)
	}
}

func TestCloudflareInferenceDeadlineDuringRejectedBackoffIsSettled(t *testing.T) {
	calls := new(atomic.Int32)
	p := moondreamReliabilityProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
		w.Write([]byte(`{"success":false,"errors":[{"code":8005}]}`))
	})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	result, err := p.Generate(ctx, moondreamReliabilityRequest(), nil)
	if !ProviderOutcomeSettled(err) || !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 || result.Usage.InputTokens != 0 || result.Usage.Estimated {
		t.Fatal("no inference was in flight during rejected backoff", calls.Load(), result.Usage, err)
	}
}

func TestCloudflareInferenceWithoutRetryAuthorityMakesOneRequest(t *testing.T) {
	calls := new(atomic.Int32)
	p := moondreamReliabilityProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(429)
		w.Write([]byte(`{"success":false,"errors":[{"code":8005}]}`))
	})
	req := moondreamReliabilityRequest()
	req.Check = nil
	_, err := p.Generate(t.Context(), req, nil)
	if err == nil || calls.Load() != 1 {
		t.Fatal("existing callers without an authority guard must not start automatic repeats", calls.Load(), err)
	}
}

func TestCloudflareInferenceDoesNotTrustMalformedRejection(t *testing.T) {
	calls := new(atomic.Int32)
	p := moondreamReliabilityProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(429)
		w.Write([]byte(`{"errors":[{"code":8005}],"errors":"bad-wire"}`))
	})
	_, err := p.Generate(t.Context(), moondreamReliabilityRequest(), nil)
	if err == nil || calls.Load() != 1 || ProviderOutcomeSettled(err) {
		t.Fatal("invalid rejection envelope must not authorize another call", calls.Load(), err)
	}
}
