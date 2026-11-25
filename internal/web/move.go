package web

import "net/http"

type moveInput struct {
	Project  string `json:"project,omitempty"`
	From     string `json:"from"`
	To       string `json:"to"`
	Revision string `json:"revision"`
}

func (s *Server) move(w http.ResponseWriter, r *http.Request) {
	var in moveInput
	if !decode(w, r, &in) {
		return
	}
	result, err := projectStore(r.Context()).Move(r.Context(), in.From, in.To, in.Revision, subject(r.Context()))
	respond(w, result, err)
}

func (s *Server) duplicate(w http.ResponseWriter, r *http.Request) {
	var in moveInput
	if !decode(w, r, &in) {
		return
	}
	result, err := projectStore(r.Context()).Duplicate(r.Context(), in.From, in.To, in.Revision, subject(r.Context()))
	respond(w, result, err)
}
