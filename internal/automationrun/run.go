package automationrun

import (
	"context"
	"errors"
	"os"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/dataops"
	"forgetmenote/internal/workspace"
)

// Start runs the runtime's scheduler and workers with the engine as their
// runner.
func (s *Service) Start(ctx context.Context) {
	if s.runtime == nil {
		return
	}
	s.runtime.Start(ctx, automation.Hooks{RefreshPackages: s.hooks.RefreshPackages, Personal: s.hooks.PersonalSpecs, Policy: s.hooks.Policy, Run: s.runAutomation, RepositoryEvent: func(spec automation.Spec) (automation.Event, bool, error) {
		if s.hooks.Repositories() == nil {
			return automation.Event{}, false, nil
		}
		state, err := s.hooks.Repositories().Status(spec.Trigger.Project)
		if err != nil {
			return automation.Event{}, false, err
		}
		return automation.Event{ID: automation.Hash([]string{state.ID, state.Downloaded}), Project: state.Project, Revision: state.Downloaded}, state.Connected && !state.Paused && state.Downloaded != "", nil
	}})
}

// errGenerationUnknown is returned by a run that finds its model answer
// already started: the provider may have charged for it, so the run never
// asks again by itself.
var errGenerationUnknown = errors.New("AI generation outcome unknown; automatic repeat suppressed")

func (s *Service) runAutomation(ctx context.Context, r *automation.Run, save func() error) error {
	// Runs publish, deliver and call models as the instance.
	if err := s.outgoing.Check(); err != nil {
		return err
	}
	seconds, err := s.automationSeconds(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	ctx, err = s.hooks.RunContext(ctx, r.Spec)
	if err != nil {
		return err
	}
	if r.Spec.Repair != nil {
		return s.hooks.RunRepair(ctx, r, save)
	}
	ctx, err = s.SourceContext(ctx, r)
	if err != nil {
		return err
	}
	if r.Spec.Kind == "personal" {
		return s.hooks.Personal().Run(ctx, r, save)
	}
	if r.Spec.Kind == "learning" {
		return s.hooks.RunLearning(ctx, r, save)
	}
	if r.Publication != nil && r.Publication.Content != "" {
		if err = s.Publish(ctx, r); err != nil {
			return err
		}
		return s.Notify(ctx, r, save)
	}
	// A recipe may already satisfy its read-only checks. Defer its generation
	// fence until a repair is actually required.
	if r.GenerationStarted && r.Spec.Kind != "recipe" && r.Spec.Kind != "assistant" {
		return errGenerationUnknown
	}
	if r.Spec.Output != "" && (r.Spec.Kind == "checks" || r.Spec.Kind == "native") && r.Publication == nil {
		old, err := s.automationOutput(ctx, r)
		if err != nil {
			return err
		}
		r.Publication = PinPublication(r.Spec.Output, old)
		if err = save(); err != nil {
			return err
		}
	}
	if r.Spec.Kind == "monitor" && r.Answer != "" {
		return s.Notify(ctx, r, save)
	}
	switch r.Spec.Kind {
	case "script":
		return s.hooks.RunScript(ctx, r, save)
	case "drive-sync":
		return s.runDriveSync(ctx, r, save)
	case "native":
		return s.runNativeAutomation(ctx, r, save)
	case "checks":
		return s.runAutomationChecks(ctx, r, save)
	case "task":
		err = s.answerTask(ctx, r, save)
	case "recipe":
		var repaired bool
		if repaired, err = s.repairRecipe(ctx, r, save); err == nil && !repaired {
			return nil
		}
	case "monitor":
		return s.runMonitor(ctx, r, save)
	case "digest":
		err = s.answerDigest(ctx, r, save)
	case "assistant":
		return s.hooks.Messaging().RunMessageAutomation(ctx, r, save)
	default:
		return errors.New("unsupported automation kind")
	}
	if err != nil {
		return err
	}
	if err = save(); err != nil {
		return err
	}
	if err = s.Publish(ctx, r); err != nil {
		return err
	}
	return s.Notify(ctx, r, save)
}

// automationSeconds is a run's whole allowance, from preparation to delivery.
func (s *Service) automationSeconds(r *automation.Run) (int, error) {
	seconds := r.Spec.TimeoutSeconds
	if r.Spec.Kind == "assistant" && r.Answer == "" && r.DeliveryState == "" {
		_, connection, _, err := s.Provider(r.Spec)
		if err != nil {
			return 0, err
		}
		// Interactive messages use the same whole-run allowance as browser chat,
		// including context preparation; a job or caller may shorten it.
		if seconds == 0 || seconds > connection.Limits.RunSeconds {
			seconds = connection.Limits.RunSeconds
		}
	} else if seconds == 0 {
		seconds = 180
	}
	return seconds, nil
}

// Call runs one of a run's data calls, as at the run's creation, and keeps
// its receipt with the run.
func (s *Service) Call(ctx context.Context, r *automation.Run, call automation.Call) (connectors.Result, error) {
	at := r.Created
	if at.IsZero() {
		at = time.Now()
	}
	call, err := call.At(at)
	if err != nil {
		return connectors.Result{}, err
	}
	result, e := s.hooks.DataExecute(ctx, dataops.Call{Project: r.Spec.Project, Connection: call.Connection, Operation: call.Operation, Arguments: call.Arguments, SchemaHash: call.SchemaHash})
	if e == nil {
		r.Receipts = append(r.Receipts, result.Receipt)
	}
	return result.Result, e
}

// automationOutput reads the note a run publishes to, with edit access. A
// missing note reads as empty.
func (s *Service) automationOutput(ctx context.Context, r *automation.Run) (workspace.Document, error) {
	store, err := s.hooks.ClientAccess(ctx, r.Spec.Actor, r.Spec.Project, "edit")
	if err != nil {
		return workspace.Document{}, err
	}
	old, err := store.Read(ctx, r.Spec.Output, "")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return workspace.Document{}, err
	}
	return old, nil
}

// automationResults executes a run's configured calls; every result must be
// complete.
func (s *Service) automationResults(ctx context.Context, r *automation.Run) ([]connectors.Result, error) {
	results := []connectors.Result{}
	for _, call := range r.Spec.Calls {
		v, err := s.Call(ctx, r, call)
		if err != nil {
			return nil, err
		}
		if !v.Receipt.Complete {
			return nil, errors.New("digest input is incomplete")
		}
		results = append(results, v)
	}
	return results, nil
}
