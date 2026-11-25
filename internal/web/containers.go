package web

import "net/http"

func (s *Server) setContainer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path      string `json:"path"`
		Container string `json:"container"`
		Revision  string `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	doc, err := projectStore(r.Context()).SetContainer(r.Context(), in.Path, in.Container, in.Revision, subject(r.Context()))
	if err != nil {
		fail(w, err)
		return
	}
	jsonResponse(w, s.output(doc))
}
func (s *Server) collections(w http.ResponseWriter, r *http.Request) {
	result, err := projectStore(r.Context()).Collections(r.Context())
	respond(w, result, err)
}
