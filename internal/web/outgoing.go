package web

import "forgetmenote/internal/outgoing"

// ConfigureOutgoing is a startup setting. A paused instance, such as a restored
// copy beside the live one, keeps serving its notebooks while every owner that
// acts outward refuses; see docs/postgres.md#outgoing-paused.
func (s *Server) ConfigureOutgoing(value string) error {
	unlock, err := s.startupConfiguration()
	if err != nil {
		return err
	}
	defer unlock()
	paused, err := outgoing.Parse(value)
	if err != nil {
		return err
	}
	if paused {
		s.outgoing.Pause()
	}
	return nil
}

func (s *Server) OutgoingPaused() bool { return s.outgoing.Paused() }
