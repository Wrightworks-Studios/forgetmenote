package web

import (
	"context"
	"net/http"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/notebooks"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type authorityPreviewInput = notebooks.PreviewInput
type authorityApplyInput = notebooks.ApplyInput

func (s *Server) authorityManagers() notebooks.Managers {
	var managers notebooks.Managers
	if s.repositories != nil {
		managers.Repositories = s.repositories
	}
	if s.backups != nil {
		managers.Backups = s.backups.manager
	}
	return managers
}
func (s *Server) previewAuthority(ctx context.Context, in authorityPreviewInput) (catalog.AuthorityTransfer, error) {
	return s.authority.Preview(ctx, in, s.authorityManagers())
}
func (s *Server) applyAuthority(ctx context.Context, in authorityApplyInput) (catalog.AuthorityTransfer, error) {
	return s.authority.Apply(ctx, in, s.authorityManagers())
}
func (s *Server) RecoverAuthority(ctx context.Context) error {
	return s.authority.Recover(ctx, s.authorityManagers())
}
func (s *Server) cancelAuthority(ctx context.Context, in authorityApplyInput) (actionResult, error) {
	ok, err := s.authority.Cancel(ctx, in, s.authorityManagers())
	return actionResult{ok}, err
}

type notebookAuthorityStatus struct {
	Project      string                        `json:"project"`
	Authority    catalog.NotebookAuthority     `json:"authority"`
	Capabilities *catalog.NotebookCapabilities `json:"capabilities"`
	Revision     string                        `json:"revision,omitempty"`
	Changing     bool                          `json:"changing"`
	Transfer     *catalog.AuthorityTransfer    `json:"transfer,omitempty"`
	Source       *reposource.State             `json:"source,omitempty"`
}

func (s *Server) authorityStatus(ctx context.Context, in projectArg) (notebookAuthorityStatus, error) {
	out := notebookAuthorityStatus{Project: in.Project}
	if !s.hasScope(ctx, "notes:read") {
		return out, workspace.ErrForbidden
	}
	var book *catalog.Project
	for _, p := range s.clientProjects(ctx, subject(ctx)) {
		if p.ID == in.Project {
			book = &p
			break
		}
	}
	if book == nil {
		return out, workspace.ErrForbidden
	}
	out.Authority = *book.Authority
	out.Capabilities = book.Capabilities
	out.Changing = s.catalog.AuthorityChanging(in.Project)
	if book.Role == "owner" {
		if p, err := s.catalog.AuthorityTransfer(subject(ctx), in.Project); err == nil && p.ID != "" {
			out.Transfer = &p
		}
	}
	if !out.Changing {
		store, err := s.toolStore(ctx, in.Project, "read")
		if err != nil {
			return out, err
		}
		st, err := store.Status(ctx)
		if err != nil {
			return out, err
		}
		out.Revision = st.Revision
	}
	if s.repositories != nil {
		state, err := s.repositories.Status(in.Project)
		if err != nil {
			return out, err
		}
		out.Source = &state
	}
	if !s.clientProjectAllowed(ctx, in.Project) {
		return notebookAuthorityStatus{}, workspace.ErrForbidden
	}
	// Membership must also be current when no content access was made.
	for _, p := range s.catalog.List(subject(ctx)) {
		if p.ID == in.Project {
			return out, nil
		}
	}
	return notebookAuthorityStatus{}, workspace.ErrForbidden
}
func (s *Server) authorityTools(server *mcp.Server) {
	addManagementTool(s, server, "preview_notebook_authority_change", "Preview a Markdown notebook authority handoff. GitHub requires a verified matching source and a completed managed backup on its default branch; confirm operator writers are stopped. Does not change content or authority.", false, s.previewAuthority)
	addManagementTool(s, server, "apply_notebook_authority_change", "Apply a current authority preview. A preparing result waits for backup worker acknowledgement; repeat with the same preview. Conflicts retain the original authority. A publishing result must be recovered, never rolled back.", false, s.applyAuthority)
	addManagementTool(s, server, "cancel_notebook_authority_change", "Cancel an unpublished authority handoff. Published handoffs must be completed.", false, s.cancelAuthority)
	addAction(s, server, &mcp.Tool{Name: "notebook_authority_status", Description: "Read notebook authority, current source revision and handoff status.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, in projectArg) (notebookAuthorityStatus, error) {
		out, err := s.authorityStatus(ctx, in)
		return out, err
	})

}
func (s *Server) authorityRoutes() {
	s.handleProject("POST /api/authority/preview", "owner", func(w http.ResponseWriter, r *http.Request) {
		var in authorityPreviewInput
		if !decode(w, r, &in) {
			return
		}
		in.Project = projectID(r.Context())
		out, err := s.previewAuthority(r.Context(), in)
		respond(w, out, err)
	})
	s.mux.HandleFunc("GET /api/authority/status", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.authorityStatus(r.Context(), projectArg{Project: r.URL.Query().Get("project")})
		respond(w, out, err)
	})
	s.mux.HandleFunc("POST /api/authority/cancel", func(w http.ResponseWriter, r *http.Request) {
		var in authorityApplyInput
		if !decode(w, r, &in) {
			return
		}
		out, err := s.cancelAuthority(r.Context(), in)
		respond(w, out, err)
	})
	// These use catalog owner checks inside the operation, so an active transfer
	// does not block its own completion/cancellation through route preflight.
	s.mux.HandleFunc("POST /api/authority/apply", func(w http.ResponseWriter, r *http.Request) {
		var in authorityApplyInput
		if !decode(w, r, &in) {
			return
		}
		out, err := s.applyAuthority(r.Context(), in)
		respond(w, out, err)
	})
}
