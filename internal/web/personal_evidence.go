package web

import (
	"context"
	"errors"
	"slices"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/workspace"
)

// A revoked grant or disabled feature invalidates queued work, not the whole
// employee scheduler. Never pass its retained content to another model call.
func (s *Server) discardUnavailablePersonal(ctx context.Context, r *automation.Run, state *personalState) error {
	settings, err := s.catalog.Assistant(r.Spec.Actor)
	if err != nil {
		return err
	}
	allowed := func(call connectors.PersonalOperation) bool {
		return s.personalConnections != nil && slices.Contains(settings.Features, connectors.PersonalFeature(call.Operation)) && s.personalConnections.Check(r.Spec.Personal.Account, call.Provider, call.Generation) == nil
	}
	available := func(o personalObservation) bool {
		if o.Data != nil {
			return slices.Contains(settings.Features, "operations") && s.dataAllowed(ctx, o.Data.Connection, o.Data.Operation)
		}
		if o.Kind == "briefing" {
			return slices.Contains(settings.Features, "briefing")
		}
		return allowed(o.Source)
	}
	state.Reads = slices.DeleteFunc(state.Reads, func(call connectors.PersonalOperation) bool { return !allowed(call) })
	state.Pending = slices.DeleteFunc(state.Pending, func(o personalObservation) bool { return !available(o) })
	for id, d := range state.Drafts {
		if d.State == "ready" && !available(d.Source) {
			d.State = "disconnected"
			d.Source.Content = nil
			state.Drafts[id] = d
		}
	}
	return nil
}

func (s *Server) personalEvidence(ctx context.Context, call connectors.PersonalOperation) (map[string]any, error) {
	result, err := s.personalSource(ctx, call)
	if err != nil {
		return nil, err
	}
	return personalData(result)
}

var errPersonalSourceChanged = errors.New("personal source changed; suggestion requires fresh evidence")

func (s *Server) personalCurrent(ctx context.Context, o personalObservation) error {
	if o.Kind == "briefing" {
		return s.checkCredential(ctx)
	}
	if o.Data != nil {
		if !s.dataAllowed(ctx, o.Data.Connection, o.Data.Operation) {
			return workspace.ErrForbidden
		}
		return nil
	}
	data, err := s.personalEvidence(ctx, o.Source)
	if err != nil {
		return err
	}
	if personalContentRevision(o.Kind, personalPayload(data)) != o.Revision {
		return errPersonalSourceChanged
	}
	return nil
}

// Superseded content is never retried as a paid generation. Queue the original
// read contract to observe the new revision, without trusting old model output.
func supersedePersonal(state *personalState, o personalObservation) {
	state.Seen[o.Key] = o.Revision
	if state.SeenAt != nil {
		state.SeenAt[o.Key] = time.Now().UTC()
	}
	resolvePersonal(state, o.Key, "superseded")
	if o.Data == nil {
		state.Reads = append(state.Reads, o.Source)
	}
}
func resolvePersonal(state *personalState, key, status string) {
	for id, d := range state.Drafts {
		if d.Source.Key == key && d.State == "ready" {
			d.State = status
			state.Drafts[id] = d
		}
	}
}
func observePersonal(state *personalState, o personalObservation) {
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
