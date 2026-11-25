package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type settledProviderFailure struct {
	error
	cause error
}

func (e *settledProviderFailure) Unwrap() error { return errors.Join(e.error, e.cause) }

// A definitive rejected HTTP response is distinct from an ambiguous transport
// failure. Its sanitized diagnostic can be retained without blocking recovery.
func ProviderOutcomeSettled(err error) bool {
	var settled *settledProviderFailure
	return errors.As(err, &settled)
}

func (p *Provider) providerHTTP(req *http.Request, check func(context.Context) error) (*http.Response, error) {
	var rejected error
	for attempt := 0; ; attempt++ {
		if check != nil {
			if err := check(req.Context()); err != nil {
				if rejected != nil {
					return nil, &settledProviderFailure{error: rejected, cause: err}
				}
				return nil, err
			}
		}
		response, err := p.client.Do(req)
		if err != nil {
			var network net.Error
			if errors.As(err, &network) && network.Timeout() {
				return nil, &providerFailure{ErrProviderRequest, "provider response timed out; outcome is unknown"}
			}
			return nil, &providerFailure{ErrProviderRequest, "configured provider is unavailable; outcome is unknown"}
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return response, nil
		}
		if p.connection.Cloudflare == nil {
			response.Body.Close()
			return nil, &providerFailure{ErrProviderRequest, "provider request failed (HTTP " + http.StatusText(response.StatusCode) + ")"}
		}
		var envelope struct {
			Errors []struct {
				Code int `json:"code"`
			} `json:"errors"`
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
		response.Body.Close()
		code := 0
		if readErr == nil && json.Unmarshal(raw, &envelope) == nil && len(envelope.Errors) == 1 {
			code = envelope.Errors[0].Code
		}
		failure := error(&providerFailure{ErrProviderRequest, fmt.Sprintf("Cloudflare request failed (HTTP %d, code %d)", response.StatusCode, code)})
		capacity := (response.StatusCode == 429 || response.StatusCode == 503) && (code == 8005 || code == 3040)
		if capacity || response.StatusCode == 400 || response.StatusCode == 401 || response.StatusCode == 402 || response.StatusCode == 403 || response.StatusCode == 404 || response.StatusCode == 413 || response.StatusCode == 429 && code == 3036 {
			failure = &settledProviderFailure{error: failure}
		}
		if ProviderOutcomeSettled(failure) {
			rejected = failure
		}
		// Only explicit inference rejections are repeatable. Never repeat search,
		// billing/auth failures, incomplete output, or an ambiguous network call.
		retry := check != nil && strings.Contains(req.URL.Path, "/run/") && capacity
		if !retry || attempt == 2 || req.GetBody == nil {
			return nil, failure
		}
		delay := time.Second << attempt
		if header := response.Header.Get("Retry-After"); header != "" {
			if seconds, err := strconv.Atoi(header); err == nil && seconds >= 0 {
				if seconds > 30 {
					return nil, failure
				}
				delay = time.Duration(seconds) * time.Second
			} else if when, err := http.ParseTime(header); err == nil {
				delay = max(time.Until(when), 0)
			}
		}
		if delay > 30*time.Second {
			return nil, failure
		}
		timer := time.NewTimer(delay)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, errors.Join(failure, req.Context().Err())
		case <-timer.C:
		}
		next := req.Clone(req.Context())
		next.Body, err = req.GetBody()
		if err != nil {
			return nil, failure
		}
		req = next
	}
}
