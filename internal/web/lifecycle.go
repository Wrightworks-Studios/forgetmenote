package web

import (
	"context"
	"errors"
)

// Startup-only fields are frozen before schedulers or request admission.
func (s *Server) startupConfiguration() (func(), error) {
	s.lifecycleMu.Lock()
	if s.started || s.closed {
		s.lifecycleMu.Unlock()
		return nil, errors.New("instance configuration is available only before startup")
	}
	return s.lifecycleMu.Unlock, nil
}

// Start runs background work after operator configuration and authority recovery.
func (s *Server) Start(ctx context.Context) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closed {
		return errors.New("instance is closed")
	}
	if s.started {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.indexes.Start(ctx); err != nil {
		return err
	}
	if err := s.quicknotes.Start(); err != nil {
		return err
	}
	s.started = true
	// A paused instance takes no duty that acts outward: source refresh and its
	// paid indexing, backup token revocation, Jev reviews and automations stay
	// with the live instance it was copied from.
	if s.repositories != nil && !s.outgoing.Paused() {
		s.repositories.Start()
	}
	if s.backups != nil && !s.outgoing.Paused() {
		s.backups.manager.Start()
	}
	if !s.outgoing.Paused() {
		s.startJevScheduler(ctx)
		s.startAutomations(ctx)
	}
	if s.chat != nil {
		s.chat.StartRecovery()
	}
	s.purge.FinishStopped(ctx)
	return nil
}

func (s *Server) Close() {
	s.closeOnce.Do(func() {
		s.lifecycleMu.Lock()
		s.closed = true
		s.lifecycleMu.Unlock()
		s.closeResources()
	})
}

func (s *Server) closeResources() {
	if s.sourceJobs != nil {
		s.sourceJobs.Close()
	}
	if s.automations != nil {
		s.automations.Close()
	}
	s.dataConnections.Close()
	if s.backups != nil {
		s.backups.manager.Close()
	}
	if s.repositories != nil {
		s.repositories.Close()
	}
	s.citations.Close()
	s.jevState.close()
	if s.quicknotes != nil {
		s.quicknotes.Close()
	}
	if s.chat != nil {
		s.chat.Close()
	}
	s.closeExports()
	if s.legacy != nil {
		s.legacy.closeListener()
	}
	if s.revisions != nil {
		s.revisions.close()
	}
	if s.indexes != nil {
		s.indexes.Close()
	}
	if s.leave != nil {
		s.leave()
	}
	if s.closeBlobs != nil {
		s.closeBlobs()
	}
}
