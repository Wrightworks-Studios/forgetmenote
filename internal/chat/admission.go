package chat

import (
	"context"
	"errors"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/workspace"
	"maps"
)

// Configuration is one immutable installed configuration and its clients.
// Returned provider maps are detached; configuration identity fences paid work.
type Configuration struct {
	WebsiteCapture *WebsiteCapture
	Config         *Config
	Providers      map[string]*Provider
	Research       *Research
	Judgments      *decision.Service
	Ledger         *decision.Ledger
	Closed         bool
	PrepareSource  func(context.Context, string, string, []byte) (workspace.SourceCapture, error)
	SearchResearch func(context.Context, *Research, string, int) ([]ResearchResult, Usage, error)
}

var ErrRunActive = errors.New("run is still active")

func (r *Runtime) Snapshot() Configuration {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	return Configuration{r.WebsiteCapture, r.Config, maps.Clone(r.Providers), r.Research, r.Judgments, r.JudgmentLedger, r.Closed, r.PrepareSource, r.SearchResearch}
}
func (r *Runtime) Cancel(id string) {
	r.Mu.Lock()
	cancel := r.Active[id]
	r.Mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Admit holds capacity, private run persistence and paused-run reconciliation
// in one transaction. A replay never consumes a second provider reservation.
func (r *Runtime) Admit(config *Config, connection Connection, id string, cancel context.CancelFunc, user string, allowed func(string, string, uint64) error, persist func() (bool, error)) (bool, error) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	if r.Active[id] != nil {
		return false, ErrRunActive
	}
	if config != r.Config || !r.available(config, connection) {
		return false, ErrCapacity
	}
	if user != "" {
		if err := r.reconcilePaused(user, allowed); err != nil {
			return false, err
		}
	}
	replayed := false
	if persist != nil {
		var err error
		replayed, err = persist()
		if err != nil {
			return false, err
		}
	}
	if !replayed {
		r.track(id, connection.ID, cancel)
	}
	return replayed, nil
}
func (r *Runtime) ReconcilePaused(user string, allowed func(string, string, uint64) error) error {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	return r.reconcilePaused(user, allowed)
}
func (r *Runtime) reconcilePaused(user string, allowed func(string, string, uint64) error) error {
	conversations, err := r.Store.activeConversations(user)
	if err != nil {
		return err
	}
	for _, conversation := range conversations {
		if conversation.Unavailable {
			continue
		}
		for _, run := range conversation.Runs {
			if run.State != "awaiting_confirmation" || r.Active[run.ID] != nil || allowed(user, conversation.Project, run.Epoch) == nil {
				continue
			}
			err := r.Store.Update(user, conversation.Project, conversation.ID, func(c *Conversation) error {
				run := c.Run(run.ID)
				if run == nil {
					return ErrNotFound
				}
				if run.State != "awaiting_confirmation" || allowed(user, conversation.Project, run.Epoch) == nil {
					return nil
				}
				run.UpdatedAt = Now()
				run.State = "cancelled"
				run.CancelRequested = true
				run.Error = "Notebook access or session was revoked while an action awaited confirmation."
				run.Usage.Reserved = 0
				for i := range run.Operations {
					if run.Operations[i].State == "awaiting_confirmation" {
						run.Operations[i].State = "declined"
						run.Operations[i].Confirmation = ""
					}
				}
				c.AddEvent(run.ID, "state", map[string]any{"state": run.State, "message": run.Error, "usage": run.Usage})
				return nil
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}
func (r *Runtime) RetainStream(user string) (func(), error) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	if r.Closed || r.StreamTotal >= 64 || r.Streams[user] >= 8 {
		return nil, ErrCapacity
	}
	r.StreamTotal++
	r.Streams[user]++
	return func() { r.Mu.Lock(); defer r.Mu.Unlock(); r.StreamTotal--; r.Streams[user]-- }, nil
}
func (r *Runtime) DeleteConversation(user, notebook, id string, authorize func() error) (Conversation, bool, error) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	c, err := r.Store.Read(user, notebook, id)
	if authErr := authorize(); authErr != nil {
		return c, false, authErr
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return c, false, err
	}
	for _, run := range c.Runs {
		if r.Active[run.ID] != nil {
			return c, false, ErrDeleteActive
		}
	}
	retired, err := r.Store.Delete(user, notebook, id, authorize)
	return c, retired, err
}
