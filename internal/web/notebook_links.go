package web

import (
	"context"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/notebooks"
	"forgetmenote/internal/workspace"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) notebookLinkRoutes() {
	s.handleProject("POST /api/notebook-links/resolve", "read", s.resolveNotebookLink)
	s.handleProject("GET /api/notebook-links/targets", "read", s.notebookLinkTargets)
}
func (s *Server) notebookLinkTools(server *mcp.Server) {
	addProjectTool(s, server, "resolve_notebook_link", "Resolve a notebook Markdown link, including a stable note ID or historical revision. Returns only currently authorized metadata and available heading anchors. Links never grant access.", "read", func(ctx context.Context, _ *workspace.Store, in struct {
		projectArg
		Path     string `json:"path,omitempty"`
		NoteID   string `json:"noteId,omitempty"`
		Revision string `json:"revision,omitempty"`
		Heading  string `json:"heading,omitempty"`
	}) (notebookTarget, error) {
		return s.resolveNotebookTarget(ctx, subject(ctx), notebookTarget{Project: in.Project, Path: in.Path, NoteID: in.NoteID, Revision: in.Revision, Heading: in.Heading})
	})
	addProjectTool(s, server, "find_notebook_link_targets", "Find up to 100 authorized note paths and stable identities for ordinary cross-notebook Markdown links. Reading never migrates or rewrites a note.", "read", func(ctx context.Context, _ *workspace.Store, in struct {
		projectArg
		Query string `json:"query,omitempty"`
	}) ([]notebookTarget, error) {
		return s.notebookTargets(ctx, subject(ctx), in.Project, in.Query)
	})
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
