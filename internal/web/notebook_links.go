package web

import (
	"context"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/notebooks"
	"net/http"
)

func (s *Server) notebookLinkRoutes() {
	s.handleProject("POST /api/notebook-links/resolve", "read", s.resolveNotebookLink)
	s.handleProject("GET /api/notebook-links/targets", "read", s.notebookLinkTargets)
}
func (s *Server) resolveNotebookLink(w http.ResponseWriter, r *http.Request) {
	var in notebookTarget
	if !decode(w, r, &in) {
		return
	}
	out, err := s.resolveNotebookTarget(r.Context(), subject(r.Context()), in)
	respond(w, out, err)
}
func (s *Server) notebookLinkTargets(w http.ResponseWriter, r *http.Request) {
	out, err := s.notebookTargets(r.Context(), subject(r.Context()), r.URL.Query().Get("target"), r.URL.Query().Get("q"))
	respond(w, out, err)
}

var errNotebookHeading = notebooks.ErrHeading

type notebookTarget = notebooks.Target

func (s *Server) resolveNotebookTarget(ctx context.Context, actor string, in notebookTarget) (notebookTarget, error) {
	return s.links.Resolve(ctx, actor, in)
}
func (s *Server) notebookTargets(ctx context.Context, actor, target, query string) ([]notebookTarget, error) {
	return s.links.Targets(ctx, actor, target, query)
}
func (s *Server) notebookEvidence(ctx context.Context, actor, project, path string) (knowledge.Evidence, error) {
	return s.links.Evidence(ctx, actor, project, path, s.authorityManagers().Repositories)
}
