package web

import "net/http"

func (s *Server) browse(w http.ResponseWriter, r *http.Request) {
	result, err := projectStore(r.Context()).BrowseCollection(r.Context(), r.URL.Query().Get("path"), r.URL.Query().Get("collection"))
	respond(w, result, err)
}
