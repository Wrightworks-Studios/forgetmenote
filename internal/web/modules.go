package web

import "forgetmenote/internal/modules"

// ConfigureModules is a startup setting: it decides which feature modules the
// instance runs, from instance.json's modules.off (internal/modules). Changing
// it means restarting.
func (s *Server) ConfigureModules(off []string) error {
	unlock, err := s.startupConfiguration()
	if err != nil {
		return err
	}
	defer unlock()
	set, err := modules.Parse(off)
	if err != nil {
		return err
	}
	s.modules = set
	return nil
}

// Modules is the module set the instance runs.
func (s *Server) Modules() modules.Set { return s.modules }
