package web

import (
	"time"

	"forgetmenote/internal/catalog"
)

// Operational previews are disposable. Published Markdown and external-effect
// barriers have independent owners and are never deleted by this cleanup.
func prunePersonal(state *personalState, policy catalog.AssistantPolicy, now time.Time) {
	days := policy.RetainDays
	if days == 0 {
		days = 30
	}
	lookback := policy.LookbackHours
	if lookback == 0 {
		lookback = 48
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	if horizon := now.Add(-time.Duration(lookback) * time.Hour).Add(-24 * time.Hour); horizon.Before(cutoff) {
		cutoff = horizon
	}
	protected := map[string]bool{}
	for _, o := range state.Pending {
		protected[o.Key] = true
	}
	for id, d := range state.Drafts {
		if d.State == "generation-unknown" || d.NoteRevision == "" && d.State == "ready" {
			protected[d.Source.Key] = true
			continue
		}
		if !d.Source.Observed.IsZero() && d.Source.Observed.Before(cutoff) {
			delete(state.Drafts, id)
			delete(state.Attempts, id)
			delete(state.Decisions, id)
		}
	}
	for key, at := range state.SeenAt {
		if !protected[key] && at.Before(cutoff) {
			fp := personalFingerprint(personalObservation{Key: key, Revision: state.Seen[key]})
			delete(state.Attempts, fp)
			delete(state.Decisions, fp)
			delete(state.Seen, key)
			delete(state.SeenAt, key)
		}
	}
	// A changed source replaces its current Seen revision. Retain decisions for
	// recoverable/published drafts and pending work; release orphaned revisions.
	live := map[string]bool{}
	for key, revision := range state.Seen {
		live[personalFingerprint(personalObservation{Key: key, Revision: revision})] = true
	}
	for id := range state.Drafts {
		live[id] = true
	}
	for _, o := range state.Pending {
		live[personalFingerprint(o)] = true
	}
	for id := range state.Decisions {
		if !live[id] {
			delete(state.Decisions, id)
		}
	}
	for id, status := range state.Attempts {
		if !live[id] && status != "generation-started" {
			delete(state.Attempts, id)
		}
	}
}
