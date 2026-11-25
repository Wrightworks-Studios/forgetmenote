package web

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"

	"forgetmenote/internal/webcore"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	s.readinessMu.Lock()
	defer s.readinessMu.Unlock()
	if time.Since(s.readinessAt) > 5*time.Second {
		s.readinessAt = time.Now()
		s.readinessOK = false
		info, err := os.Stat(s.data)
		var disk syscall.Statfs_t
		if err == nil && info.Mode().Perm()&0200 != 0 && syscall.Statfs(s.data, &disk) == nil && uint64(disk.Bavail)*uint64(disk.Bsize) >= 32<<20 {
			file, err := os.CreateTemp(s.data, ".readiness-*")
			if err == nil {
				_, err = file.Write([]byte("ready"))
				closeErr := file.Close()
				removeErr := os.Remove(file.Name())
				s.readinessOK = err == nil && closeErr == nil && removeErr == nil
			}
		}
	}
	if !s.readinessOK {
		http.Error(w, "Storage unavailable or below 32 MiB free; check server logs and volume", 503)
		return
	}
	w.Write([]byte("ready"))
}

type outcomeWriter struct {
	http.ResponseWriter
	status int
	// route names a request no ServeMux matched, such as an action API call.
	route string
	// failure is a server error's cause: the error a handler failed with, or
	// else the start of the error text it wrote.
	failure error
	text    []byte
}

// maxFailureText bounds the cause a request log line carries.
const maxFailureText = 300

var (
	// A URL keeps its scheme and host: userinfo, query strings, fragments and
	// paths can carry signatures, tokens and webhook secrets.
	causeURL = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)(?:[^\s"'<>/?#@]*@)?([^\s"'<>/?#]*)([^\s"'<>]*)`)
	// Email addresses identify users.
	causeEmail = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)+`)
	// Credential-like name=value pairs, as in connection strings, and HTTP
	// authorization schemes keep their name but not their value.
	causeCredential = regexp.MustCompile(`(?i)(\b(?:bearer|basic)\s+|[A-Za-z0-9_\-]*(?:pass|secret|token|key|sig|credential|auth)[A-Za-z0-9_\-]*\s*=\s*)[^\s"'&,;)]+`)
)

// failureCause is a server error's cause for the request log: at most
// maxFailureText bytes, without credentials or user identifiers.
func (w *outcomeWriter) failureCause() string {
	cause := string(w.text)
	if w.failure != nil {
		cause = w.failure.Error()
	} else if len(w.text) > maxFailureText {
		// The captured text stops mid-response; drop its last, possibly partial,
		// word so a cut URL or secret is never half redacted.
		cause = cause[:max(strings.LastIndexAny(cause[:maxFailureText], " \t\r\n"), 0)]
	}
	return redactedCause(cause)
}

// redactedCause is a complete error message as a log line may carry it.
func redactedCause(cause string) string {
	cause = causeURL.ReplaceAllStringFunc(cause, func(url string) string {
		// Punctuation ending a sentence is not part of the URL.
		trimmed := strings.TrimRight(url, ".,:;!)]}")
		match := causeURL.FindStringSubmatch(trimmed)
		redacted := match[1] + match[2]
		if match[3] != "" {
			redacted += "/…"
		}
		return redacted + url[len(trimmed):]
	})
	cause = causeEmail.ReplaceAllString(cause, "<email>")
	cause = causeCredential.ReplaceAllString(cause, "$1…")
	if len(cause) > maxFailureText {
		cause = cause[:maxFailureText]
	}
	return strings.TrimSpace(strings.ToValidUTF8(cause, ""))
}

func (w *outcomeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *outcomeWriter) RecordFailure(err error) {
	if w.failure == nil {
		w.failure = err
	}
}
func (w *outcomeWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *outcomeWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	// One byte beyond the bound shows the text was cut.
	if w.status >= 500 && w.failure == nil && len(w.text) <= maxFailureText {
		w.text = append(w.text, p[:min(len(p), maxFailureText+1-len(w.text))]...)
	}
	return w.ResponseWriter.Write(p)
}
func (w *outcomeWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	http.NewResponseController(w.ResponseWriter).Flush()
}
func logOutcome(start time.Time, r *http.Request, w *outcomeWriter) {
	status := w.status
	if status == 0 {
		status = 200
	}
	// Registered patterns and fixed route names contain no note content, search
	// queries, or credentials.
	route := r.Pattern
	if route == "" {
		route = w.route
	}
	if route == "" {
		route = "unmatched/auth"
	}
	attributes := []any{"method", r.Method, "route", route, "status", status, "duration_ms", time.Since(start).Milliseconds()}
	if status >= 500 {
		attributes = append(attributes, "error", w.failureCause())
	}
	slog.Info("request", attributes...)
}

// Public OAuth traffic has an independent budget from password authentication.
// Behind a reverse proxy every caller shares one peer, so a token request that
// presents a refresh token or code this server issued is budgeted per account
// instead; unauthenticated traffic cannot starve valid refreshes.
func (s *Server) admitOAuth(w http.ResponseWriter, r *http.Request) bool {
	p, keys, limits := &s.oauthRequests, []string{"all", "peer:" + socketPeer(r)}, []int{1200, 240}
	var invalid error
	switch r.URL.Path {
	case "/oauth/revoke":
		p = &s.oauthRevocations
	case "/oauth/token":
		r.Body = http.MaxBytesReader(w, r.Body, 16384)
		if invalid = r.ParseForm(); invalid == nil {
			if account, ok := s.oauth.TokenSubject(r.Form); ok {
				p, keys, limits = &s.oauthGrants, []string{"subject:" + account}, []int{30}
			}
		}
	}
	if ok, err := p.take(r.Context(), keys, limits); !ok {
		limited(w, err, "OAuth request limit; retry in a minute")
		return false
	}
	if invalid != nil {
		http.Error(w, "Invalid token request", 400)
		return false
	}
	return true
}

func logToolOutcome(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		start := time.Now()
		result, err := next(ctx, method, req)
		if method == "tools/call" {
			name, outcome := "unknown", "success"
			toolResult, ok := result.(*mcp.CallToolResult)
			if err != nil || (ok && toolResult != nil && toolResult.IsError) {
				outcome = "error"
			}
			// A successful dispatch establishes that the name belongs to a registered tool.
			// Never log arbitrary names from rejected requests or arguments.
			if call, ok := req.(*mcp.CallToolRequest); ok && err == nil && call.Params != nil {
				name = call.Params.Name
			}
			attributes := []any{"tool", name, "outcome", outcome, "duration_ms", time.Since(start).Milliseconds()}
			// A tool error that a sentinel or webcore.Error classifies as 5xx is
			// a server failure: its cause is logged as the request log does.
			// Unclassified errors log no text: they include the SDK's rejections
			// of schema-invalid arguments, which quote the argument values.
			if ok && toolResult != nil && toolResult.IsError && toolResult.GetError() != nil {
				if cause := toolResult.GetError(); webcore.ErrorOf(cause, 0).Status >= 500 {
					attributes = append(attributes, "error", redactedCause(cause.Error()))
				}
			}
			slog.Info("mcp_tool", attributes...)
		}
		return result, err
	}
}
