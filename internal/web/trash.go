package web

import (
	"context"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type trashInput struct {
	projectArg
	Paths    []string `json:"paths"`
	Revision string   `json:"revision"`
}
type restoreTrashInput struct {
	projectArg
	ID       string `json:"id"`
	To       string `json:"to,omitempty"`
	Revision string `json:"revision"`
}
type emptyTrashInput struct {
	projectArg
	Revision     string `json:"revision"`
	Confirmation string `json:"confirmation"`
}

func listTrash(ctx context.Context, store *workspace.Store, _ projectArg) (workspace.TrashListing, error) {
	return store.ListTrash(ctx)
}
func trashNotes(ctx context.Context, store *workspace.Store, in trashInput) (workspace.TrashResult, error) {
	return store.TrashNotes(ctx, in.Paths, in.Revision, subject(ctx))
}
func restoreTrash(ctx context.Context, store *workspace.Store, in restoreTrashInput) (workspace.TrashResult, error) {
	return store.RestoreTrash(ctx, in.ID, in.To, in.Revision, subject(ctx))
}
func emptyTrash(ctx context.Context, store *workspace.Store, in emptyTrashInput) (workspace.TrashResult, error) {
	return store.EmptyTrash(ctx, in.Revision, in.Confirmation)
}

func (s *Server) trashRoutes() {
	// Preserve pre-404 authorization for the historical Trash path prefix.
	s.routes.RejectPrefix("/api/trash", webcore.MethodRoles{Write: "owner"})
	notebookJSON(s, "GET /api/trash", "read", listTrash)
	notebookJSON(s, "POST /api/trash", "owner", trashNotes)
	notebookJSON(s, "POST /api/trash/restore", "owner", restoreTrash)
	notebookJSON(s, "POST /api/trash/empty", "owner", emptyTrash)
}
func (s *Server) trashTools(server *mcp.Server) {
	addProjectTool(s, server, "list_trash", "List recoverable notes in the notebook's system Trash. Returns original locations and a revision-bound emptying confirmation. Preview the count and obtain explicit user confirmation before empty_trash. Git history and backups survive emptying.", "read", listTrash)
	addProjectTool(s, server, "trash_notes", "Move 1–50 notes to recoverable Trash. Requires Owner/Admin, unlocked notes and the current project_status revision. Preserves accepted live edits and discussions. Ordinary searches exclude Trash; includeTrash opts in.", "owner", trashNotes)
	addProjectTool(s, server, "restore_trash", "Restore a Trash item by ID and current revision from list_trash. Empty to uses its original location; specify a new Markdown path on conflict. Never overwrites an occupied destination. Requires Owner/Admin.", "owner", restoreTrash)
	addProjectTool(s, server, "empty_trash", "Remove all currently trashed notes from the notebook. First call list_trash, show the item count and explain that Git history/backups remain, then obtain explicit user confirmation. Pass that preview's revision and confirmation verbatim. A stale preview is rejected; never silently refresh and retry. Requires Owner/Admin.", "owner", emptyTrash)
}

// Grouped/proposed deletes reach the same owner boundary as dedicated Trash
// operations; ordinary edits keep their existing Edit authority.
func (s *Server) authorizeGroupDeletes(ctx context.Context, store *workspace.Store, group workspace.EditGroup) error {
	deletes := false
	for _, edit := range group.Edits {
		deletes = deletes || edit.Delete
	}
	if !deletes {
		return nil
	}
	for _, project := range s.catalog.List(subject(ctx)) {
		candidate, err := s.catalog.Access(subject(ctx), project.ID, "owner")
		if err == nil && candidate == store {
			return nil
		}
	}
	return workspace.ErrForbidden
}
