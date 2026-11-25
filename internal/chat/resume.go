package chat

import (
	"errors"
)

func (s *Store) Resume(user, project, id, runID string, rounds, tools int, epoch uint64) (Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, version, err := s.loadVersion(user, project, id)
	if err != nil {
		return c, err
	}
	r := c.Run(runID)
	if r == nil {
		return c, ErrNotFound
	}
	if err := r.Options.Corpus.Validate(); err != nil {
		return c, err
	}
	if r.State == "completed" || r.State == "running" || r.State == "queued" || r.State == "awaiting_confirmation" {
		return c, ErrConflict
	}
	running, err := s.anyActive(user)
	if err != nil {
		return c, err
	}
	if running {
		return c, errors.New("another chat run is active")
	}
	ensureUsageDays(r)
	r.ReservationDay = ""
	r.Options.BudgetTokens = 0
	r.Usage.Reserved = 0
	r.RoundLimit = r.Rounds + rounds
	r.ToolLimit = r.ToolCount + tools
	r.ReviewAllowance++
	r.State = "queued"
	r.Holder = s.db.Holder
	r.Error = ""
	r.CancelRequested = false
	r.Epoch = epoch
	r.UpdatedAt = Now()
	c.AddEvent(runID, "state", map[string]string{"state": "queued"})
	err = s.persist(c, version)
	return c, err
}
