package web

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/capacity"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/personal"
	"forgetmenote/internal/workspace"
)

// The personal assistant worker (internal/personal) reaches these consent and
// access checks through its hooks; web binds them in initPersonal.

func (s *Server) personalSpec(user string, settings catalog.AssistantSettings) (automation.Spec, error) {
	account, err := s.catalog.Account(user)
	if err != nil {
		return automation.Spec{}, err
	}
	policy := s.catalog.AssistantPolicy()
	spec := automation.Spec{ID: "personal-" + account.ID, Kind: "personal", Actor: user, Project: settings.Project, Generation: settings.Revision, Personal: &automation.Personal{Account: account.ID, Settings: settings.Revision, Policy: policy.Revision}, IntervalSeconds: policy.IntervalSeconds, AIConnection: policy.AIConnection, Model: policy.Model, ReasoningEffort: policy.ReasoningEffort, ExternalProcessing: true}
	if spec.IntervalSeconds == 0 {
		spec.IntervalSeconds = 300
	}
	if spec.ReasoningEffort == "" {
		spec.ReasoningEffort = "low"
	}
	for _, p := range s.catalog.List(user) {
		if p.ID != spec.Project && p.ProtectedInbox == "" && p.AssistantOwner == "" && !slices.Contains(settings.ExcludedProjects, p.ID) {
			if p.Kind == "repository" {
				if s.repositories == nil {
					continue
				}
				state, err := s.repositories.Status(p.ID)
				if err != nil || !state.Connected || state.Paused || !state.IndexConfig.ExternalProcessing {
					continue
				}
			}
			spec.ReadProjects = append(spec.ReadProjects, p.ID)
		}
	}
	sort.Strings(spec.ReadProjects)
	for _, c := range s.dataConnections.List() {
		if c.ExternalProcessing && s.catalog.ConnectionAllowed(user, c.ID, "") {
			spec.Connections = append(spec.Connections, c.ID)
		}
	}
	sort.Strings(spec.Connections)
	return spec, spec.Validate()
}

func (s *Server) personalSpecs() []automation.Spec {
	specs := []automation.Spec{}
	for user, settings := range s.catalog.ActiveAssistants() {
		spec, err := s.personalSpec(user, settings)
		if err == nil {
			specs = append(specs, spec)
		}
	}
	return specs
}

func (s *Server) checkPersonalSpec(spec automation.Spec) error {
	settings, err := s.catalog.Assistant(spec.Actor)
	if err != nil {
		return err
	}
	if spec.Personal == nil || !s.catalog.AssistantPolicy().Enabled || !settings.Enabled || !settings.ExternalProcessing {
		return workspace.ErrForbidden
	}
	current, err := s.personalSpec(spec.Actor, settings)
	if err != nil {
		return err
	}
	if automation.Hash(current) != automation.Hash(spec) {
		return errors.New("personal assistant permissions or consent changed")
	}
	return nil
}

func (s *Server) checkPersonalSources(ctx context.Context, r *automation.Run, items []personal.Observation) error {
	if err := s.checkAutomationAuth(ctx); err != nil {
		return err
	}
	for _, o := range items {
		if o.Source.Provider != "" {
			if s.personalConnections == nil {
				return workspace.ErrForbidden
			}
			if err := s.personalConnections.Check(r.Spec.Personal.Account, o.Source.Provider, o.Source.Generation); err != nil {
				return err
			}
		}
	}
	return nil
}

// A revoked grant or disabled feature invalidates queued work, not the whole
// employee scheduler. Never pass its retained content to another model call.
func (s *Server) discardUnavailablePersonal(ctx context.Context, r *automation.Run, state *personal.State) error {
	settings, err := s.catalog.Assistant(r.Spec.Actor)
	if err != nil {
		return err
	}
	allowed := func(call connectors.PersonalOperation) bool {
		return s.personalConnections != nil && slices.Contains(settings.Features, connectors.PersonalFeature(call.Operation)) && s.personalConnections.Check(r.Spec.Personal.Account, call.Provider, call.Generation) == nil
	}
	available := func(o personal.Observation) bool {
		if o.Data != nil {
			return slices.Contains(settings.Features, "operations") && s.dataAllowed(ctx, o.Data.Connection, o.Data.Operation)
		}
		if o.Kind == "briefing" {
			return slices.Contains(settings.Features, "briefing")
		}
		return allowed(o.Source)
	}
	state.Reads = slices.DeleteFunc(state.Reads, func(call connectors.PersonalOperation) bool { return !allowed(call) })
	state.Pending = slices.DeleteFunc(state.Pending, func(o personal.Observation) bool { return !available(o) })
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
	return personal.Data(result)
}

func (s *Server) personalCurrent(ctx context.Context, o personal.Observation) error {
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
	if personal.ContentRevision(o.Kind, personal.Payload(data)) != o.Revision {
		return personal.ErrSourceChanged
	}
	return nil
}

func (s *Server) recordPersonalInteraction(ctx context.Context, user string, in personal.Interaction) error {
	if subject(ctx) != user || !s.catalog.Interactive(user) {
		return workspace.ErrForbidden
	}
	settings, err := s.catalog.Assistant(user)
	if err != nil {
		return err
	}
	if !settings.Enabled || !settings.ExternalProcessing || !settings.MaintainMemory || !s.catalog.AssistantPolicy().Enabled {
		return nil
	}
	if _, err = s.clientAccess(ctx, user, settings.Project, "read"); err != nil {
		return err
	}
	if in.ID == "" || in.UserText == "" {
		return nil
	}
	if len(in.UserText)+len(in.Response) > capacity.StateBytes.Limit()/4 {
		return capacity.StateBytes.Error()
	}
	spec, err := s.personalSpec(user, settings)
	if err != nil {
		return err
	}
	if in.At.IsZero() {
		in.At = time.Now().UTC()
	}
	_, err = s.automations.Queue.ChangeJobState(personal.MemorySpec(spec), func(raw json.RawMessage) (json.RawMessage, error) {
		var state personal.MemoryState
		if err := json.Unmarshal(raw, &state); err != nil {
			return nil, err
		}
		if state.Completed[in.ID] || slices.ContainsFunc(state.Pending, func(v personal.Interaction) bool { return v.ID == in.ID }) || slices.ContainsFunc(state.Batch, func(v personal.Interaction) bool { return v.ID == in.ID }) {
			return raw, nil
		}
		state.Pending = append(state.Pending, in)
		return json.Marshal(state)
	})
	return err
}
