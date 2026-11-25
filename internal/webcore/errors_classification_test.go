package webcore

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forgetmenote/internal/backups"
	"forgetmenote/internal/blob"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/githubapp"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/sources"
	"forgetmenote/internal/workspace"
)

func TestErrorsClassifyAlikeForBrowserAndActionAPI(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{Invalid("Name either person or people, not both"), 400, "invalid_request"},
		{fmt.Errorf("sorting: %w", decision.ErrInvalid), 400, "invalid_request"},
		{decision.ErrLocalOnly, 403, "local_only"},
		{decision.ErrLimit, 429, "capacity"},
		{fmt.Errorf("query: %w", connectors.ErrInvalid), 400, "invalid_request"},
		{fmt.Errorf("%w; at most four jobs may wait", sources.ErrJobCapacity), 429, "capacity"},
		{&http.MaxBytesError{Limit: 1 << 20}, 413, "too_large"},
		// Sentinels of operational owners that reach handlers.
		{errors.Join(catalog.ErrStorageUnavailable, errors.New("connection reset")), 503, "unavailable"},
		{fmt.Errorf("judge: %w", decision.ErrUnavailable), 503, "unavailable"},
		{fmt.Errorf("query: %w", connectors.ErrUnavailable), 503, "unavailable"},
		{fmt.Errorf("save automation: %w", pg.ErrConflict), 409, "conflict"},
		{connectors.ErrSchemaChanged, 409, "conflict"},
		{chat.ErrRunActive, 409, "conflict"},
		{fmt.Errorf("automation state: %w", pg.ErrTooLarge), 413, "too_large"},
		{fmt.Errorf("source original: %w", blob.ErrTooLarge), 413, "too_large"},
		{pg.ErrNUL, 400, "invalid_request"},
		{fmt.Errorf("gmail: %w", connectors.ErrProviderBusy), 429, "capacity"},
		{workspace.ErrLiveCapacity, 429, "capacity"},
		// A bundle import that ran out of time is no server failure and no
		// refused bundle: the same import may succeed later.
		{fmt.Errorf("%w after 1m30s, which does not show that the bundle is damaged", workspace.ErrBundleTimeout), 503, "timeout"},
	} {
		for _, fallback := range []int{500, 400} {
			if got := ErrorOf(test.err, fallback); got.Status != test.status || got.Code != test.code || got.Message == "" {
				t.Errorf("%v (fallback %d): %+v", test.err, fallback, got)
			}
		}
	}
	if got := ErrorOf(Invalid("choose a folder"), 500); got.Message != "choose a folder" {
		t.Fatalf("invalid message: %+v", got)
	}
	if got := ErrorOf(fmt.Errorf("%w; at most four jobs may wait", sources.ErrJobCapacity), 500); got.Message != "source processing capacity exceeded; at most four jobs may wait" {
		t.Fatalf("a refusal lost its context: %+v", got)
	}
}

// A notebook refuses input the caller must correct as invalid, a limit it
// has reached until something is released as capacity, which MCP marks
// retryable, and a limit of the notebook or a note that no retry lifts as
// too large, which it does not; either way the client reads the notebook's
// own message.
func TestNotebookRefusalsClassifyAsInputCapacityOrLimit(t *testing.T) {
	_, invalid := workspace.ParseContainer([]byte("---\nfmn-container: box\n---\n# Note\n"))
	for _, test := range []struct {
		err     error
		status  int
		code    string
		message string
	}{
		{invalid, 400, "invalid_request", "fmn-container must be draft or archive; remove the field for Notes"},
		{fmt.Errorf("move: %w", workspace.ErrInvalid), 400, "invalid_request", "move: invalid notebook request"},
		{fmt.Errorf("Trash index full: %w", workspace.ErrCapacity), 429, "capacity", "Trash index full: notebook capacity reached"},
		{fmt.Errorf("note history: %w", workspace.ErrLimit), 413, "too_large", "note history: notebook limit exceeded"},
	} {
		for _, fallback := range []int{500, 400} {
			if got := ErrorOf(test.err, fallback); got.Status != test.status || got.Code != test.code || got.Message != test.message {
				t.Errorf("%v (fallback %d): %+v", test.err, fallback, got)
			}
		}
	}
}

// MCP marks a refusal retryable when the same call may succeed later: a
// capacity limit or an import that ran out of time, never an unavailable
// owner or a refused bundle.
func TestRetryableRefusals(t *testing.T) {
	for _, test := range []struct {
		err       error
		retryable bool
	}{
		{workspace.ErrLiveCapacity, true},
		{fmt.Errorf("%w after 2s", workspace.ErrBundleTimeout), true},
		{fmt.Errorf("%w: damaged or incomplete Git history", workspace.ErrBundle), false},
		{errors.Join(catalog.ErrStorageUnavailable, errors.New("connection reset")), false},
		{errors.New("unclassified"), false},
	} {
		if got := ErrorOf(test.err, 500); got.Retryable() != test.retryable {
			t.Errorf("%v: %+v retryable %v", test.err, got, got.Retryable())
		}
	}
}

// A classified server failure tells the client what failed; the cause under
// it, such as a database error, goes only to the request log.
func TestServerFailuresSendOnlyWhatFailed(t *testing.T) {
	cause := errors.Join(catalog.ErrStorageUnavailable, errors.New("ERROR: injected write failure (SQLSTATE P0001)"))
	for _, fail := range []func(http.ResponseWriter, error){Fail, FailAction} {
		response := httptest.NewRecorder()
		log := &recordingWriter{ResponseWriter: response}
		fail(log, fmt.Errorf("register account: %w", cause))
		if response.Code != 503 || !strings.Contains(response.Body.String(), "catalog authority unavailable; retry shortly") || strings.Contains(response.Body.String(), "SQLSTATE") || strings.Contains(response.Body.String(), "register account") {
			t.Errorf("response: %d %s", response.Code, response.Body)
		}
		if log.failure == nil || !strings.Contains(log.failure.Error(), "SQLSTATE P0001") {
			t.Errorf("logged cause: %v", log.failure)
		}
	}
}

// A server failure whose sentinel is wrapped alone keeps the message written
// for users, such as a rate limit's or an upgrade's guidance; only a sentinel
// combined with a cause, by errors.Join or a second %w, is reduced to itself.
func TestServerFailuresKeepTheirOwnMessages(t *testing.T) {
	for _, test := range []struct {
		err     error
		message string
	}{
		{&githubapp.RateLimitError{}, "GitHub rate limit reached; refresh will retry after the cooldown"},
		{fmt.Errorf("refresh: %w", &githubapp.RateLimitError{}), "refresh: GitHub rate limit reached; refresh will retry after the cooldown"},
		{fmt.Errorf("%w: the owner does not accept notebook imports (HTTP 404); upgrade it to this release", workspace.ErrOwnerUnavailable), "notebook owner unavailable; retry against the same owner: the owner does not accept notebook imports (HTTP 404); upgrade it to this release"},
		{fmt.Errorf("%w: %w", knowledge.ErrUnavailable, errors.New("SQLSTATE 53300")), "knowledge storage is unavailable; retry"},
		{fmt.Errorf("refresh: %w", errors.Join(reposource.ErrUnavailable, errors.New("SQLSTATE 53300"))), "GitHub integration unavailable"},
	} {
		for _, fallback := range []int{500, 400} {
			if got := ErrorOf(test.err, fallback); got.Message != test.message {
				t.Errorf("%q (fallback %d): %+v", test.err, fallback, got)
			}
		}
	}
}

// recordingWriter stands in for the request log's response writer.
type recordingWriter struct {
	http.ResponseWriter
	failure error
}

func (w *recordingWriter) RecordFailure(err error) { w.failure = err }

func TestServerFailuresReachTheRequestLog(t *testing.T) {
	cause := errors.New("synthetic git failure")
	for _, test := range []struct {
		fail     func(http.ResponseWriter, error)
		err      error
		recorded bool
	}{
		{Fail, cause, true},
		{FailAction, cause, false}, // an unclassified action error is the caller's input
		{FailAction, fmt.Errorf("owner: %w", backups.ErrUnavailable), true},
		{Fail, Invalid("choose a folder"), false},
	} {
		log := &recordingWriter{ResponseWriter: httptest.NewRecorder()}
		// Handlers may write through wrappers of the log's writer.
		test.fail(JSONErrors(log, false), test.err)
		if (log.failure != nil) != test.recorded || test.recorded && !errors.Is(log.failure, test.err) {
			t.Errorf("%v: recorded %v", test.err, log.failure)
		}
	}
}
