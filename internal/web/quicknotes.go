package web

import (
	"forgetmenote/internal/inbox"
	"forgetmenote/internal/quicknotes"
)

func (s *Server) initQuicknotes() error {
	var err error
	s.quicknotes, err = quicknotes.Open(s.db, s.catalog)
	if err == nil {
		s.quicknotes.Outgoing = s.outgoing
	}
	return err
}
func (s *Server) quicknotesRoutes() {
	inbox.NewHTTP(s.quicknotes, s.interactiveIdentity).Register(s.mux)
}
