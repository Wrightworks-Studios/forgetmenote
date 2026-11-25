package web

import (
	"forgetmenote/internal/workspace"
	"net/http"
)

type metadataInput struct {
	Project  string             `json:"project,omitempty"`
	Path     string             `json:"path"`
	Revision string             `json:"revision"`
	Metadata workspace.Metadata `json:"metadata"`
}

func (s *Server) setMetadata(w http.ResponseWriter, r *http.Request) {
	var in metadataInput
	if !decode(w, r, &in) {
		return
	}
	doc, err := projectStore(r.Context()).SetMetadata(r.Context(), in.Path, in.Metadata, in.Revision, subject(r.Context()))
	if err != nil {
		fail(w, err)
		return
	}
	jsonResponse(w, s.output(doc))
}
