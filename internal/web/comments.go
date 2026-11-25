package web

import (
	"context"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type commentsInput struct {
	projectArg
	Path string `json:"path"`
}
type addCommentInput struct {
	Anchor  *workspace.CommentAnchor `json:"anchor,omitempty"`
	ReplyTo string                   `json:"replyTo,omitempty"`
	projectArg
	Path     string `json:"path"`
	Text     string `json:"text"`
	Revision string `json:"revision"`
}
type resolveCommentInput struct {
	projectArg
	Path     string `json:"path"`
	ID       string `json:"id"`
	Revision string `json:"revision"`
}
type deleteCommentInput struct {
	projectArg
	Path     string `json:"path"`
	ID       string `json:"id"`
	ReplyID  string `json:"replyId,omitempty"`
	Revision string `json:"revision"`
}

// Authors act on their own comments. Moderating someone else's comment takes
// Edit to resolve it and Owner/Admin to delete it, within the caller's client
// restrictions (browser sessions have none).
func (s *Server) commentModerator(ctx context.Context, project string, deleting bool) bool {
	role := "edit"
	if deleting {
		role = "owner"
	}
	_, err := s.toolStore(ctx, project, role)
	return err == nil
}

func (s *Server) listComments(ctx context.Context, store *workspace.Store, in commentsInput) (webcore.DiscussionOutput, error) {
	if chatScopeFrom(ctx) != nil {
		v, err := scopedChatDiscussion(ctx, store, in.Path)
		return s.presentDiscussion(ctx, in.Project, v, err)
	}
	v, err := store.Comments(ctx, in.Path)
	return s.presentDiscussion(ctx, in.Project, v, err)
}
func (s *Server) addComment(ctx context.Context, store *workspace.Store, in addCommentInput) (webcore.DiscussionOutput, error) {
	v, err := store.AddComment(ctx, in.Path, in.Text, in.Revision, subject(ctx), workspace.CommentOptions{Anchor: in.Anchor, ReplyTo: in.ReplyTo})
	return s.presentDiscussion(ctx, in.Project, v, err)
}
func (s *Server) resolveComment(ctx context.Context, store *workspace.Store, in resolveCommentInput) (webcore.DiscussionOutput, error) {
	v, err := store.ResolveComment(ctx, in.Path, in.ID, in.Revision, subject(ctx), s.commentModerator(ctx, in.Project, false))
	return s.presentDiscussion(ctx, in.Project, v, err)
}
func (s *Server) deleteComment(ctx context.Context, store *workspace.Store, in deleteCommentInput) (webcore.DiscussionOutput, error) {
	v, err := store.DeleteComment(ctx, in.Path, in.ID, in.ReplyID, in.Revision, subject(ctx), s.commentModerator(ctx, in.Project, true))
	return s.presentDiscussion(ctx, in.Project, v, err)
}

func (s *Server) commentRoutes() {
	notebookJSON(s, "GET /api/comments", "read", s.listComments)
	notebookJSON(s, "POST /api/comments", "comment", s.addComment)
	notebookJSON(s, "POST /api/comments/resolve", "comment", s.resolveComment)
	notebookJSON(s, "POST /api/comments/delete", "comment", s.deleteComment)
}
func (s *Server) commentTools(server *mcp.Server) {
	addProjectTool(s, server, "list_comments", "Read a note's discussion and revision.", "read", s.listComments)
	addProjectTool(s, server, "add_comment", "Add feedback without editing the note. Requires Comment or Edit access. Read comments first and pass the returned revision. Optionally anchor a new thread using the exact rendered quote and surrounding prefix/suffix, or reply using replyTo.", "comment", s.addComment)
	addProjectTool(s, server, "resolve_comment", "Resolve an ordinary discussion comment. Read the discussion first and provide its revision. Commenters may resolve their own comments; Editors may resolve others'.", "comment", s.resolveComment)
	addProjectTool(s, server, "delete_comment", "Delete a comment or individual reply. Requires current Comment access and authorship of the selected content, or Owner/Admin moderation authority. Pass the discussion revision and parent id, plus replyId for a reply. Other replies survive parent deletion; Git history remains. Separate from resolving a thread.", "comment", s.deleteComment)
}
