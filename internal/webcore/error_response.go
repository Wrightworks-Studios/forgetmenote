package webcore

import (
	"bytes"
	"net/http"
	"strings"
)

// ErrorResponse gives the browser JSON API one error contract even for standard
// middleware/ServeMux rejections. Successes, existing JSON and event streams go
// directly to the underlying writer. Only bounded plain error text is delayed.
type ErrorResponse struct {
	http.ResponseWriter
	status    int
	plain     bool
	body      bytes.Buffer
	actionAPI bool
}

func JSONErrors(w http.ResponseWriter, actionAPI bool) *ErrorResponse {
	return &ErrorResponse{ResponseWriter: w, actionAPI: actionAPI}
}
func (w *ErrorResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *ErrorResponse) WriteHeader(status int) {
	if status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status != 0 {
		return
	}
	w.status = status
	w.plain = status >= 400 && !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json")
	if !w.plain {
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *ErrorResponse) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if !w.plain {
		return w.ResponseWriter.Write(body)
	}
	const maxErrorText = 8192
	if remaining := maxErrorText - w.body.Len(); remaining > 0 {
		w.body.Write(body[:min(remaining, len(body))])
	}
	return len(body), nil
}
func (w *ErrorResponse) FlushError() error {
	if w.plain {
		return nil
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *ErrorResponse) Flush() { _ = w.FlushError() }
func (w *ErrorResponse) Finish() {
	if !w.plain {
		return
	}
	w.plain = false
	message := strings.TrimSpace(w.body.String())
	if message == "" {
		message = http.StatusText(w.status)
	}
	w.Header().Del("Content-Length")
	WriteError(w.ResponseWriter, Error{Status: w.status, Code: statusCode(w.status), Message: message}, w.actionAPI)
}
func statusCode(status int) string {
	switch status {
	case 400, 422:
		return "invalid_request"
	case 401:
		return "unauthenticated"
	case 403:
		return "forbidden"
	case 404:
		return "not_found"
	case 405:
		return "method_not_allowed"
	case 409:
		return "conflict"
	case 413:
		return "too_large"
	case 415:
		return "unsupported_media_type"
	case 429:
		return "capacity"
	case 503:
		return "unavailable"
	case 504:
		return "timeout"
	default:
		return "internal_error"
	}
}
