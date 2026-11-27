package web

import (
	"context"
	"net/http"
	"time"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
)

// A chat run's authorization stays here, beside the sessions it compares:
// chatRunAuth captures the request's session for a run, checkRun rechecks it
// and runContext is the identity the run works as. internal/chatrun reaches
// them through its hooks (chat_service.go).

func (s *Server) chatRunAuth(r *http.Request, c chat.Conversation) chat.RunAuth {
	a := chat.RunAuth{User: c.Owner, Project: c.Project, Epoch: s.catalog.Epoch(c.Owner)}
	if token, ok := sessionToken(r); ok {
		a.Session = digest(token)
	}
	return a
}
func (s *Server) checkRun(a chat.RunAuth) error {
	if !s.catalog.ValidSession(a.User, a.Epoch) {
		return workspace.ErrForbidden
	}
	if a.Session != "" {
		g, ok := s.oauth.Session(a.Session)
		if !ok || g.Subject != a.User || g.Expires <= time.Now().Unix() || g.Epoch != a.Epoch {
			return workspace.ErrForbidden
		}
	}
	_, err := s.catalog.Access(a.User, a.Project, "read")
	return err
}
func (s *Server) runContext(a chat.RunAuth) (context.Context, error) {
	account, err := s.catalog.Account(a.User)
	if err != nil {
		return nil, err
	}
	ctx := context.WithValue(context.Background(), identityKey{}, a.User)
	ctx = chat.WithRunAuth(ctx, a)
	return attribution.WithSource(ctx, account.Identity(), "chat", "embedded"), s.checkRun(a)
}
