package web

import (
	"context"
	"net/http"

	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) wikiRoutes() {
	s.handleProject("POST /api/wiki", "owner", s.setOpenAsWiki)
}

// setOpenAsWiki changes the notebook's default layout. The catalog rechecks
// ownership; members read the result with the notebook in /api/projects.
func (s *Server) setOpenAsWiki(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OpenAsWiki bool `json:"openAsWiki"`
	}
	if !decode(w, r, &in) {
		return
	}
	respond(w, map[string]bool{"openAsWiki": in.OpenAsWiki}, s.catalog.SetOpenAsWiki(subject(r.Context()), projectID(r.Context()), in.OpenAsWiki))
}

func (s *Server) wikiTools(server *mcp.Server) {
	addProjectTool(s, server, "set_notebook_wiki", "Choose whether members open a shared notes notebook as a wiki by default: its index.md as the home page, a page tree and links between pages. Requires Owner/Admin. Each person can still switch layouts; notebook content is unchanged. list_projects reports the current value as openAsWiki.", "owner", func(ctx context.Context, _ *workspace.Store, in struct {
		projectArg
		OpenAsWiki bool `json:"openAsWiki"`
	}) (map[string]bool, error) {
		if err := s.catalog.SetOpenAsWiki(subject(ctx), in.Project, in.OpenAsWiki); err != nil {
			return nil, err
		}
		return map[string]bool{"openAsWiki": in.OpenAsWiki}, nil
	})
}
