package personal

import (
	"errors"
	"time"
)

// ErrSourceChanged reports that a source changed after the worker read it, so a
// suggestion built on it needs fresh evidence.
var ErrSourceChanged = errors.New("personal source changed; suggestion requires fresh evidence")

// Superseded content is never retried as a paid generation. Queue the original
// read contract to observe the new revision, without trusting old model output.
func supersedePersonal(state *State, o Observation) {
	state.Seen[o.Key] = o.Revision
	if state.SeenAt != nil {
		state.SeenAt[o.Key] = time.Now().UTC()
	}
	resolvePersonal(state, o.Key, "superseded")
	if o.Data == nil {
		state.Reads = append(state.Reads, o.Source)
	}
}
func resolvePersonal(state *State, key, status string) {
	for id, d := range state.Drafts {
		if d.Source.Key == key && d.State == "ready" {
			d.State = status
			state.Drafts[id] = d
		}
	}
}
func observePersonal(state *State, o Observation) {
	if state.Seen[o.Key] == o.Revision {
		return
	}
	for _, previous := range state.Pending {
		if previous.Key == o.Key && previous.Revision == o.Revision {
			return
		}
	}
	resolvePersonal(state, o.Key, "superseded")
	pending := state.Pending[:0]
	for _, old := range state.Pending {
		if old.Key != o.Key {
			pending = append(pending, old)
		}
	}
	state.Pending = append(pending, o)
}
