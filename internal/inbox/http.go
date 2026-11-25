// Package inbox owns the HTTP transport for private Inbox captures. Notebook
// authorization, content, consent and revision checks remain in quicknotes.
package inbox

import (
	"forgetmenote/internal/quicknotes"
	"forgetmenote/internal/webcore"
	"net/http"
)

// HTTP binds the concrete capture service to the shared interactive identity
// check. Browser and API requests continue through the instance auth middleware.
type HTTP struct {
	store    *quicknotes.Service
	identity func(*http.Request) (string, error)
}

func NewHTTP(store *quicknotes.Service, identity func(*http.Request) (string, error)) *HTTP {
	return &HTTP{store: store, identity: identity}
}

func (h *HTTP) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/quicknotes", h.list)
	mux.HandleFunc("POST /api/quicknotes", h.capture)
	mux.HandleFunc("GET /api/quicknotes/settings", h.settings)
	mux.HandleFunc("POST /api/quicknotes/settings", h.settings)
	mux.HandleFunc("GET /api/quicknotes/destinations", h.destinations)
	mux.HandleFunc("POST /api/quicknotes/file", h.file)
	mux.HandleFunc("POST /api/quicknotes/undo", h.transition)
	mux.HandleFunc("POST /api/quicknotes/recover", h.transition)
	mux.HandleFunc("POST /api/quicknotes/sort", h.transition)
}

func (h *HTTP) list(w http.ResponseWriter, r *http.Request) {
	subject, err := h.identity(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	result := map[string]any{}
	if r.URL.Query().Get("summary") != "1" {
		captures, err := h.store.List(r.Context(), subject)
		if err != nil {
			webcore.Fail(w, err)
			return
		}
		if captures == nil {
			captures = []quicknotes.Capture{}
		}
		result["captures"] = captures
	}
	count, err := h.store.InboxCount(r.Context(), subject)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	result["inboxCount"] = count
	webcore.JSON(w, result)
}

func (h *HTTP) capture(w http.ResponseWriter, r *http.Request) {
	subject, err := h.identity(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	var input struct {
		OperationID string `json:"operationId"`
		Content     string `json:"content"`
	}
	if !webcore.Decode(w, r, &input) {
		return
	}
	capture, err := h.store.Capture(r.Context(), subject, input.OperationID, input.Content)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	webcore.JSON(w, capture)
}

func (h *HTTP) settings(w http.ResponseWriter, r *http.Request) {
	subject, err := h.identity(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	var settings quicknotes.Settings
	if r.Method == "POST" {
		if !webcore.Decode(w, r, &settings) {
			return
		}
		settings, err = h.store.UpdateSettings(subject, settings)
	} else {
		settings, err = h.store.Settings(subject)
	}
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	webcore.JSON(w, settings)
}

func (h *HTTP) destinations(w http.ResponseWriter, r *http.Request) {
	subject, err := h.identity(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	destinations, err := h.store.Destinations(r.Context(), subject)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	if destinations == nil {
		destinations = []quicknotes.Destination{}
	}
	webcore.JSON(w, map[string]any{"destinations": destinations})
}

func (h *HTTP) file(w http.ResponseWriter, r *http.Request) {
	subject, err := h.identity(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	var input struct {
		ID          string                 `json:"id"`
		Destination quicknotes.Destination `json:"destination"`
		Version     string                 `json:"version"`
	}
	if !webcore.Decode(w, r, &input) {
		return
	}
	capture, err := h.store.File(r.Context(), subject, input.ID, input.Destination, input.Version)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	webcore.JSON(w, capture)
}

func (h *HTTP) transition(w http.ResponseWriter, r *http.Request) {
	subject, err := h.identity(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	var input struct {
		ID string `json:"id"`
	}
	if !webcore.Decode(w, r, &input) {
		return
	}
	var capture quicknotes.Capture
	if r.URL.Path == "/api/quicknotes/undo" {
		capture, err = h.store.Undo(r.Context(), subject, input.ID)
	} else if r.URL.Path == "/api/quicknotes/sort" {
		capture, err = h.store.Sort(r.Context(), subject, input.ID)
	} else {
		capture, err = h.store.Recover(r.Context(), subject, input.ID)
	}
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	webcore.JSON(w, capture)
}
