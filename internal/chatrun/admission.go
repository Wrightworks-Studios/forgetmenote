package chatrun

import (
	"forgetmenote/internal/chat"
)

func (s *Service) pausedChatAllowed(user, notebook string, epoch uint64) error {
	return s.hooks.CheckRun(chat.RunAuth{User: user, Project: notebook, Epoch: epoch})
}

func (s *Service) reconcilePausedChats(user string) error {
	return s.hooks.Chat().ReconcilePaused(user, s.pausedChatAllowed)
}
