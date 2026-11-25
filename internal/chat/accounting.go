package chat

import (
	"errors"
	"time"
)

func Day() string { return time.Now().UTC().Format("2006-01-02") }
func originalDay(r Run) string {
	if len(r.CreatedAt) >= 10 {
		if _, err := time.Parse("2006-01-02", r.CreatedAt[:10]); err == nil {
			return r.CreatedAt[:10]
		}
	}
	return Day()
}
func ensureUsageDays(r *Run) {
	if r.UsageDays == nil {
		r.UsageDays = map[string]Accounting{}
		old := r.Usage
		old.Reserved = 0
		r.UsageDays[originalDay(*r)] = old
	}
	if r.ReservationDay == "" && r.Usage.Reserved > 0 {
		r.ReservationDay = originalDay(*r)
	}
}
func accountingFields(a *Accounting) []*int64 {
	return []*int64{&a.InputTokens, &a.OutputTokens, &a.CachedTokens, &a.ReasoningTokens, &a.CacheWriteTokens, &a.UncertainTokens}
}

// Attribute only the new cost to the request day; never rewrite prior-day costs
// when a long-lived run receives another explicit allowance.
func trackUsage(before Run, after *Run) error {
	ensureUsageDays(&before)
	if after.UsageDays == nil {
		after.UsageDays = before.UsageDays
	}
	day := before.InFlightDay
	if day == "" {
		day = before.ReservationDay
	}
	if day == "" {
		day = Day()
	}
	bucket := after.UsageDays[day]
	a, b, target := accountingFields(&after.Usage), accountingFields(&before.Usage), accountingFields(&bucket)
	changed := false
	for i := range a {
		if *a[i] < *b[i] || *a[i] < 0 {
			return errors.New("durable chat usage cannot decrease")
		}
		delta := *a[i] - *b[i]
		*target[i] += delta
		changed = changed || delta != 0
	}
	if after.Usage.Requests < before.Usage.Requests {
		return errors.New("durable chat request count cannot decrease")
	}
	requests := after.Usage.Requests - before.Usage.Requests
	bucket.Requests += requests
	bucket.Estimated = bucket.Estimated || after.Usage.Estimated
	if changed || requests > 0 {
		after.UsageDays[day] = bucket
	}
	if after.Usage.Reserved > 0 && after.ReservationDay == "" {
		after.ReservationDay = Day()
	}
	return nil
}
func settleUnresolved(r *Run) {
	if r.InFlightTokens <= 0 {
		return
	}
	r.Usage.UncertainTokens += r.InFlightTokens
	r.Usage.Estimated = true
	r.Usage.Requests++
	r.InFlightTokens = 0
}

// SettleUsage uses reserved record headroom independently of transcript growth.
// No provider work is acknowledged as settled until this record is durable.
func (s *Store) SettleUsage(user, project, id, runID string, usage Usage, uncertain int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, version, err := s.loadVersion(user, project, id)
	if err != nil {
		return err
	}
	r := c.Run(runID)
	if r == nil {
		return ErrNotFound
	}
	before := *r
	r.Usage.Add(usage)
	r.Usage.UncertainTokens += max(0, uncertain)
	r.InFlightTokens = 0
	r.Usage.Reserved = 0
	r.UpdatedAt = Now()
	c.UpdatedAt = r.UpdatedAt
	if err = trackUsage(before, r); err != nil {
		return err
	}
	r.InFlightDay = ""
	return s.persistReserved(c, version)
}
