package web

import "net/http"

func (s *Server) locks(w http.ResponseWriter, r *http.Request) {
	state, err := projectStore(r.Context()).Locks(r.Context(), r.URL.Query().Get("path"))
	respond(w, state, err)
}
func (s *Server) setLock(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path     string `json:"path"`
		Locked   bool   `json:"locked"`
		Revision string `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	state, err := projectStore(r.Context()).SetLock(r.Context(), in.Path, in.Locked, in.Revision, subject(r.Context()))
	respond(w, state, err)
}
