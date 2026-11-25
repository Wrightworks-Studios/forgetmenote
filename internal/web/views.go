package web

import (
	"forgetmenote/internal/workspace"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// A view block runs in a frame whose document has an opaque origin: it cannot
// read the app's cookies or storage, call its API, submit forms or reach the
// network. Its only channel is the bridge that fmn-view.js opens to the app
// page, which answers a fixed list of read calls with the reader's session.
func (s *Server) viewPolicy() string {
	return "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline' " + s.origin + "/static/views/; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; connect-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'self'"
}

func (s *Server) viewHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", s.viewPolicy())
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

func (s *Server) writeView(w http.ResponseWriter, source string) {
	s.viewHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><script src="/static/views/fmn-view.js"></script></head><body>`+"\n")
	io.WriteString(w, source)
	io.WriteString(w, "\n</body></html>\n")
}

// view serves one view block of a note revision, numbered in document order.
func (s *Server) view(w http.ResponseWriter, r *http.Request) {
	s.viewHeaders(w)
	q := r.URL.Query()
	path := q.Get("path")
	if !strings.HasSuffix(path, ".md") {
		http.Error(w, "Views live in notes", 400)
		return
	}
	block, err := strconv.Atoi(q.Get("block"))
	if err != nil || block < 0 {
		http.NotFound(w, r)
		return
	}
	doc, err := projectStore(r.Context()).Read(r.Context(), path, q.Get("revision"))
	if err != nil {
		fail(w, err)
		return
	}
	blocks := workspace.ViewBlocks(doc.Content)
	if block >= len(blocks) {
		http.NotFound(w, r)
		return
	}
	s.writeView(w, blocks[block])
}

// viewPreview serves a draft's view block, posted by the app page into the
// frame, under the same policy as a saved one.
func (s *Server) viewPreview(w http.ResponseWriter, r *http.Request) {
	s.viewHeaders(w)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid preview", 400)
		return
	}
	s.writeView(w, r.PostForm.Get("source"))
}
