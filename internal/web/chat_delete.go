package web

import (
	"errors"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
	"net/http"
	"time"
)

func (s *Server) chatDelete(w http.ResponseWriter, r *http.Request) {
	user, err := s.interactiveIdentity(r)
	if err != nil {
		chatFail(w, err)
		return
	}
	project := projectID(r.Context())
	if _, err = s.catalog.Access(user, project, "read"); err != nil {
		chatFail(w, err)
		return
	}
	auth := s.chatRunAuth(r, chat.Conversation{Owner: user, Project: project})
	authorize := func() error {
		if !s.authenticated(r) || subject(r.Context()) != user {
			return workspace.ErrForbidden
		}
		return s.checkRun(auth)
	}
	content, release := s.chat.RetainContent(user, r.PathValue("chat"))
	defer release()
	if !content.TryLock() {
		http.Error(w, "Conversation is busy; retry deletion shortly", http.StatusConflict)
		return
	}
	defer content.Unlock()
	list, releaseList := s.chat.RetainContent(user, "")
	defer releaseList()
	if !list.TryLock() {
		http.Error(w, "Conversation list is busy; retry deletion shortly", http.StatusConflict)
		return
	}
	defer list.Unlock()
	// Another gateway's citation check of this conversation holds it shared.
	held, ok, err := s.db.TryExclusive(r.Context(), conversationLock(user, r.PathValue("chat")))
	if err != nil {
		http.Error(w, "Conversations are unavailable; retry shortly", http.StatusServiceUnavailable)
		return
	}
	if !ok {
		http.Error(w, "Conversation is busy; retry deletion shortly", http.StatusConflict)
		return
	}
	defer held()
	// Admission and provider teardown use this same lock. A terminal disk state
	// alone does not prove a provider callback or approved mutation has finished.
	_, retired, err := s.chat.DeleteConversation(user, project, r.PathValue("chat"), authorize)
	if retired {
		content.Deleted = true
	}
	if retired || errors.Is(err, chat.ErrNotFound) || err == nil {
		cleanupErr := s.citations.ForgetConversation(user, r.PathValue("chat"))
		err = errors.Join(err, cleanupErr)
	}
	if err != nil {
		chatFail(w, err)
		return
	}
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
	jsonResponse(w, map[string]bool{"deleted": true})
}

// conversationLock is held shared by a citation check of a conversation on any
// gateway and exclusively by its deletion.
func conversationLock(user, id string) string { return "conversation " + digest(user+"\x00"+id) }
