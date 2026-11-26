package chatrun

import (
	"errors"
	"net/http"
	"time"

	"forgetmenote/internal/auth"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
)

func (s *Service) chatDelete(w http.ResponseWriter, r *http.Request) {
	user, err := s.hooks.Interactive(r)
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	project := webcore.NotebookID(r.Context())
	if _, err = s.catalog.Access(user, project, "read"); err != nil {
		webcore.Fail(w, err)
		return
	}
	owner := s.hooks.RunAuth(r, chat.Conversation{Owner: user, Project: project})
	authorize := func() error {
		if !s.hooks.Authenticated(r) || auth.Subject(r.Context()) != user {
			return workspace.ErrForbidden
		}
		return s.hooks.CheckRun(owner)
	}
	content, release := s.hooks.Chat().RetainContent(user, r.PathValue("chat"))
	defer release()
	if !content.TryLock() {
		http.Error(w, "Conversation is busy; retry deletion shortly", http.StatusConflict)
		return
	}
	defer content.Unlock()
	list, releaseList := s.hooks.Chat().RetainContent(user, "")
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
	_, retired, err := s.hooks.Chat().DeleteConversation(user, project, r.PathValue("chat"), authorize)
	if retired {
		content.Deleted = true
	}
	if retired || errors.Is(err, chat.ErrNotFound) || err == nil {
		cleanupErr := s.hooks.Citations().ForgetConversation(user, r.PathValue("chat"))
		err = errors.Join(err, cleanupErr)
	}
	if err != nil {
		webcore.Fail(w, err)
		return
	}
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
	webcore.JSON(w, map[string]bool{"deleted": true})
}

// conversationLock is held shared by a citation check of a conversation on any
// gateway and exclusively by its deletion.
func conversationLock(user, id string) string { return "conversation " + auth.Digest(user+"\x00"+id) }
