package connectors

import (
	"errors"
	"io"
	"net/http"
	"time"
)

// errTooLarge is a response body longer than its caller's limit.
var errTooLarge = errors.New("response exceeds its limit")

// noRedirectClient returns a redirect as the response instead of following it,
// so a request's credential never reaches a location the operator did not
// configure.
func noRedirectClient(transport http.RoundTripper, timeout time.Duration) *http.Client {
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// readBounded reads a body of at most limit bytes. A longer body is errTooLarge
// rather than a truncated prefix that could parse as a complete answer.
func readBounded(body io.Reader, limit int) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		return nil, errTooLarge
	}
	return raw, nil
}
