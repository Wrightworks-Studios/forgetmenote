package web

import (
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
	"net/http"
)

func (s *Server) interactiveIdentity(r *http.Request) (string, error) {
	id := subject(r.Context())
	if id == "" || id == catalog.Bootstrap || !s.catalog.Interactive(id) {
		return "", workspace.ErrForbidden
	}
	return id, nil
}
