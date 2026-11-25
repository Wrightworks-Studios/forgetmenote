package web

import "forgetmenote/internal/chat"

func (s *Server) pausedChatAllowed(user, notebook string, epoch uint64) error {
	return s.checkRun(chat.RunAuth{User: user, Project: notebook, Epoch: epoch})
}
func (s *Server) reconcilePausedChats(user string) error {
	return s.chat.ReconcilePaused(user, s.pausedChatAllowed)
}
