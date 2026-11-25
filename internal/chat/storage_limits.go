package chat

import (
	"encoding/json"
	"errors"
	"forgetmenote/internal/capacity"
	"math"
)

type storageCaps struct{ Bytes, Events, Count int }

func capsFrom(v Limits) storageCaps {
	c := storageCaps{Bytes: maxRecordBytes, Events: maxEvents, Count: math.MaxInt}
	if v.MaxRecordBytes > 0 {
		c.Bytes = v.MaxRecordBytes
	}
	if v.MaxEvents > 0 {
		c.Events = v.MaxEvents
	}
	if v.MaxConversations > 0 {
		c.Count = v.MaxConversations
	}
	return c
}
func (s *Store) SetLimits(limits Limits, connections []Connection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limits = capsFrom(limits)
	s.connectionLimits = map[string]storageCaps{}
	for _, c := range connections {
		s.connectionLimits[c.ID] = capsFrom(c.Limits)
	}
	return s.recover(true)
}
func (s *Store) caps(connection string) storageCaps {
	c := s.limits
	if c.Bytes == 0 {
		c = capsFrom(Limits{})
	}
	if v, ok := s.connectionLimits[connection]; ok {
		c.Bytes = min(c.Bytes, v.Bytes)
		c.Events = min(c.Events, v.Events)
		c.Count = min(c.Count, v.Count)
	}
	return c
}
func (s *Store) fits(c Conversation, b []byte) bool {
	limit := s.caps(c.Options.Connection)
	for _, r := range c.Runs {
		if Active(r.State) {
			limit.Bytes -= min(16384, limit.Bytes/4)
			limit.Events -= min(16, limit.Events/4)
			break
		}
	}
	return len(b) <= limit.Bytes && len(c.Events) <= limit.Events
}

type recoveryRecord struct {
	Hash string   `json:"hash"`
	Runs []string `json:"runs"`
}

// A legacy full record needs no free transcript slots to stop an unfinished run.
// The small hash-bound overlay preserves every original byte and event.
func (s *Store) recoverFull(c Conversation, version int64) error {
	raw, _, stored, err := s.fetch(c.Owner, c.ID)
	if err != nil {
		return err
	}
	if stored != version {
		return ErrConflict
	}
	var original Conversation
	if err = json.Unmarshal(raw, &original); err != nil {
		return err
	}
	overlay := recoveryRecord{Hash: Hash(raw)}
	for _, r := range original.Runs {
		if needsChatRecovery(r) {
			overlay.Runs = append(overlay.Runs, r.ID)
		}
	}
	b, err := json.Marshal(overlay)
	if err != nil {
		return err
	}
	if len(b) > 65536 {
		return errors.New("too many unfinished chat runs to recover")
	}
	ctx, cancel := storageContext()
	defer cancel()
	tag, err := s.db.Pool.Exec(ctx, `UPDATE chat_conversations SET version = version + 1, active = $5, recovery = $6
		WHERE org = $1 AND account = $2 AND id = $3 AND version = $4`, s.db.Org, Hash([]byte(c.Owner)), c.ID, version, activeRuns(c), b)
	if err == nil && tag.RowsAffected() == 0 {
		err = ErrConflict
	}
	return err
}
func (s *Store) applyRecovery(c *Conversation, record, raw []byte) error {
	if raw == nil {
		return nil
	}
	if len(raw) > 65536 {
		return errors.New("chat recovery record is unsafe")
	}
	var overlay recoveryRecord
	if json.Unmarshal(raw, &overlay) != nil {
		return errors.New("chat recovery record is corrupt")
	}
	if overlay.Hash != Hash(record) {
		return nil
	}
	for _, id := range overlay.Runs {
		if r := c.Run(id); r != nil && needsChatRecovery(*r) {
			before := *r
			r.State = "interrupted"
			r.Error = "The server restarted; this full conversation remains readable. Start another conversation to continue."
			settleUnresolved(r)
			if err := trackUsage(before, r); err != nil {
				return err
			}
			r.InFlightDay = ""
			r.Usage.Reserved = 0
		}
	}
	return nil
}

func needsChatRecovery(r Run) bool {
	if r.State == "running" || r.State == "queued" || r.InFlightTokens > 0 {
		return true
	}
	if r.State == "awaiting_confirmation" {
		for _, op := range r.Operations {
			if op.State == "running" && op.Confirmation == "" {
				return true
			}
		}
	}
	return false
}

func (s *Store) readCapacity() int {
	n := max(capacity.StateBytes.Limit(), s.caps("").Bytes)
	for _, c := range s.connectionLimits {
		n = max(n, c.Bytes)
	}
	return n
}
