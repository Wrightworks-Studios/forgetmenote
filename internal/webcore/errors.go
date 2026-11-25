// Package webcore owns shared request mechanics for concrete HTTP features.
// Catalog and credential authorization remain with their operational owners.
package webcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"

	"forgetmenote/internal/backups"
	"forgetmenote/internal/blob"
	"forgetmenote/internal/capacity"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/jsonfile"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/notebooks"
	"forgetmenote/internal/outgoing"
	"forgetmenote/internal/pg"
	"forgetmenote/internal/quicknotes"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/sources"
	"forgetmenote/internal/workspace"
)

type Error struct {
	Status  int
	Code    string
	Message string
}

func (e Error) Error() string { return e.Message }

// Retryable reports a refusal that the same call may get past later without
// any change: a capacity limit, or work that ran out of its time.
func (e Error) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Code == "timeout"
}

// Invalid reports input the caller must correct, as 400 on every transport.
func Invalid(message string) error { return Error{400, "invalid_request", message} }

// ErrorOf is shared by browser and API transports. Only the fallback for
// unclassified errors differs for the existing action API's input contract.
func ErrorOf(err error, fallback int) Error {
	var classified Error
	if errors.As(err, &classified) {
		return classified
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return Error{413, "too_large", fmt.Sprintf("Request body exceeds its %d-byte limit", tooLarge.Limit)}
	}
	for _, class := range sentinels {
		for _, sentinel := range class.errs {
			if !errors.Is(err, sentinel) {
				continue
			}
			detail := Error{class.status, class.code, err.Error()}
			switch {
			case class.message != "":
				detail.Message = class.message
			case class.status >= 500 && joined(err, sentinel):
				// A server failure combined with its cause, such as a database
				// error, names only what failed; the cause belongs in the log.
				// A sentinel wrapped alone keeps the text written for users.
				detail.Message = sentinel.Error()
			}
			return detail
		}
	}
	if fallback == 400 {
		return Error{fallback, "invalid_request", err.Error()}
	}
	return Error{fallback, "internal_error", err.Error()}
}

// joined reports whether err reaches sentinel through an error that wraps
// several, as errors.Join and fmt.Errorf with more than one %w do.
func joined(err, sentinel error) bool {
	for err != nil && err != sentinel {
		switch wrapper := err.(type) {
		case interface{ Unwrap() []error }:
			errs := wrapper.Unwrap()
			if len(errs) != 1 {
				return len(errs) > 1
			}
			err = errs[0]
		case interface{ Unwrap() error }:
			err = wrapper.Unwrap()
		default:
			return false
		}
	}
	return false
}

// sentinels classify the errors of the operational owners that reach
// handlers; the first class with a matching sentinel applies.
var sentinels = []struct {
	status        int
	code, message string
	errs          []error
}{
	// The client closed the request (nginx's 499): not a server failure.
	{499, "canceled", "", []error{context.Canceled}},
	{503, "outgoing_paused", "", []error{outgoing.ErrPaused}},
	{409, "conflict", "", []error{catalog.ErrManagedAI}},
	{422, "invalid_notebook_heading", "", []error{notebooks.ErrHeading}},
	{403, "local_only", "", []error{decision.ErrLocalOnly}},
	{403, "read_only", "", []error{workspace.ErrReadOnly}},
	{403, "forbidden", "", []error{workspace.ErrForbidden, backups.ErrDenied, reposource.ErrDenied}},
	{503, "owner_unavailable", "", []error{workspace.ErrOwnerUnavailable}},
	// An import that ran out of time; the same bundle may import later.
	{503, "timeout", "", []error{workspace.ErrBundleTimeout}},
	{503, "unavailable", "", []error{backups.ErrUnavailable, reposource.ErrUnavailable, quicknotes.ErrUnavailable, knowledge.ErrUnavailable, connectors.ErrPersonalUnavailable, connectors.ErrUnavailable, catalog.ErrStorageUnavailable, decision.ErrUnavailable}},
	{422, "live_operation_rejected", "", []error{workspace.ErrLiveOperationRejected}},
	{409, "live_pending", "", []error{workspace.ErrLivePending}},
	{409, "dirty_workspace", "", []error{workspace.ErrDirty}},
	{409, "conflict", "", []error{workspace.ErrConflict, backups.ErrConflict, reposource.ErrConflict, reposource.ErrBusy, catalog.ErrLastOwner, catalog.ErrLastAdmin, chat.ErrConflict, chat.ErrDeleteActive, chat.ErrRunActive, connectors.ErrPersonalChanged, connectors.ErrSchemaChanged, pg.ErrConflict}},
	{413, "too_large", "", []error{workspace.ErrBundleTooLarge, workspace.ErrLimit, reposource.ErrLimit, pg.ErrTooLarge, blob.ErrTooLarge}},
	{400, "invalid_request", "", []error{workspace.ErrInvalid, workspace.ErrBundle, catalog.ErrLocalInput, quicknotes.ErrInput, sources.ErrRejected, decision.ErrInvalid, connectors.ErrInvalid, pg.ErrNUL}},
	{429, "capacity", "", []error{workspace.ErrCapacity, workspace.ErrLiveCapacity, chat.ErrCapacity, capacity.ErrBusy, decision.ErrLimit, sources.ErrJobCapacity, connectors.ErrProviderBusy}},
	{404, "not_found", "Conversation not found", []error{chat.ErrNotFound}},
	{404, "not_found", "", []error{os.ErrNotExist}},
}

func JSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}

// Legacy browser consumers require a string error; v1 action consumers require
// the nested error object. Both carry the same status/code/message. Keep these
// explicit wire adapters until those supported consumers have migrated.
func WriteError(w http.ResponseWriter, detail Error, actionAPI bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(detail.Status)
	if actionAPI {
		JSON(w, map[string]any{"error": map[string]string{"code": detail.Code, "message": detail.Message}})
	} else {
		JSON(w, map[string]string{"error": detail.Message, "code": detail.Code, "message": detail.Message})
	}
}

// Fail writes a browser error. An unclassified error is a server failure.
func Fail(w http.ResponseWriter, err error) { fail(w, err, 500, false) }

// Respond ends a browser handler with its result: value as JSON, or err.
func Respond(w http.ResponseWriter, value any, err error) {
	if err != nil {
		Fail(w, err)
		return
	}
	JSON(w, value)
}

// FailAction writes a v1 action API error. Its actions report invalid input as
// unclassified errors, so those remain the caller's 400 until they use Invalid.
func FailAction(w http.ResponseWriter, err error) { fail(w, err, 400, true) }

func fail(w http.ResponseWriter, err error, fallback int, actionAPI bool) {
	detail := ErrorOf(err, fallback)
	if detail.Status >= 500 {
		RecordFailure(w, err)
	}
	WriteError(w, detail, actionAPI)
}

// RecordFailure gives the cause of a server error to the request log, which
// is the first writer in w's chain with a RecordFailure method.
func RecordFailure(w http.ResponseWriter, err error) {
	for w != nil {
		if log, ok := w.(interface{ RecordFailure(error) }); ok {
			log.RecordFailure(err)
			return
		}
		wrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = wrapper.Unwrap()
	}
}
func Decode(w http.ResponseWriter, r *http.Request, value any) bool {
	if r.Header.Get("Content-Type") != "application/json" && !hasJSONMediaType(r.Header.Get("Content-Type")) {
		WriteError(w, Error{415, "unsupported_media_type", "JSON required"}, false)
		return false
	}
	if err := jsonfile.Decode(r.Body, value); err != nil {
		detail := Error{400, "invalid_request", err.Error()}
		if tooLarge := new(http.MaxBytesError); errors.As(err, &tooLarge) {
			detail = ErrorOf(err, 400)
		}
		WriteError(w, detail, false)
		return false
	}
	return true
}

func hasJSONMediaType(value string) bool {
	media, _, err := mime.ParseMediaType(value)
	return err == nil && media == "application/json"
}
