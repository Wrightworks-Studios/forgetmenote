package automationrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/workspace"
)

type scriptExecution struct {
	ctx  context.Context
	run  *automation.Run
	save func() error
	info PackageInfo
}

type scriptRequest struct {
	Step      string          `json:"step"`
	Operation string          `json:"operation"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Service) scriptContract(spec automation.Spec) (string, error) {
	parts := []string{automation.Hash(spec)}
	for _, id := range spec.Connections {
		h, ok := s.hooks.DataConnections().Contract(id)
		if !ok {
			return "", errors.New("script connector unavailable")
		}
		parts = append(parts, h)
	}
	return automation.Hash(parts), nil
}

func (s *Service) runScript(ctx context.Context, r *automation.Run, save func() error) error {
	if s.hooks.Python() == nil {
		return errors.New("configure AUTOMATION_PYTHON to run administrator-installed packages")
	}
	ctx = automation.WithScriptHost(ctx)
	info, files, err := s.ScriptPackage(ctx, r.Spec, false)
	if err != nil {
		return err
	}
	// The definition's environment records its installation. This process runs
	// its own preparation of the same approved files for its Python.
	prepareCtx, stopPreparing := context.WithTimeout(ctx, time.Duration(r.Spec.TimeoutSeconds)*time.Second)
	environment, err := s.hooks.Python().Ensure(prepareCtx, files, info.Digest)
	stopPreparing()
	if err != nil {
		return err
	}
	contract, err := s.scriptContract(r.Spec)
	if err != nil {
		return err
	}
	if r.Script == nil {
		r.Script = &automation.ScriptCheckpoint{Digest: info.Digest, Revision: info.Revision, Environment: environment, Contract: contract, Steps: map[string]*automation.ScriptStep{}}
		if err = save(); err != nil {
			return err
		}
	}
	cp := r.Script
	if cp.Expired {
		return errors.New("private run journal expired; start a new run; unresolved effects remain blocked")
	}
	if cp.Digest != info.Digest || cp.Revision != info.Revision || cp.Environment != environment || cp.Contract != contract {
		return errors.New("script checkpoint does not match activated source, environment or connector contract")
	}
	if cp.Uncertain() {
		return errors.New("interrupted paid call or write has an unknown outcome; reconcile retained steps before another run")
	}
	if cp.Complete {
		return s.Notify(ctx, r, save)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.Spec.TimeoutSeconds)*time.Second)
	defer cancel()
	execution := &scriptExecution{ctx: ctx, run: r, save: save, info: info}
	if err = s.scriptAuthority(execution); err != nil {
		return err
	}
	var hostError error
	err = s.hooks.Python().Run(ctx, cp.Environment, cp.Digest, r.Spec.Script.Sandboxed(), func(raw []byte) (any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var request scriptRequest
		if err := automation.DecodeScriptRequest(raw, &request); err != nil {
			return nil, err
		}
		result, err := s.scriptStep(execution, request)
		if err != nil {
			hostError = err
		}
		return result, err
	})
	if err != nil {
		if hostError != nil {
			return fmt.Errorf("%v: %w", err, hostError)
		}
		return err
	}
	if !cp.Complete {
		return errors.New("script exited without a durable completion record")
	}
	return s.Notify(ctx, r, save)
}

func (s *Service) scriptAuthority(e *scriptExecution) error {
	if err := s.hooks.CheckEvidence(e.ctx, e.run, true); err != nil {
		return err
	}
	if err := s.checkScriptSources(e); err != nil {
		return err
	}
	cfg := e.run.Spec.Script
	if s.hooks.Python() == nil {
		return errors.New("Python automation runtime unavailable")
	}
	if err := s.hooks.Python().Ready(e.run.Script.Environment, cfg.Digest); err != nil {
		return err
	}
	if _, err := s.hooks.ClientAccess(e.ctx, e.run.Spec.Actor, cfg.SourceProject, "read"); err != nil {
		return err
	}
	// This rereads the retained package through the source owner, including current
	// exclusions and installation authority, before releasing data or making calls.
	info, _, err := s.ScriptPackage(e.ctx, e.run.Spec, false)
	if err != nil {
		return err
	}
	if info.Digest != e.run.Script.Digest {
		return errors.New("source package changed")
	}
	for alias, id := range cfg.Connections {
		for _, op := range e.run.Spec.Operations[id] {
			if !e.info.Manifest.RequestsOperation(alias, op) {
				continue
			}
			if !s.hooks.DataAllowed(e.ctx, id, op) {
				return workspace.ErrForbidden
			}
		}
	}
	contract, err := s.scriptContract(e.run.Spec)
	if err != nil {
		return err
	}
	if contract != e.run.Script.Contract {
		return errors.New("connector configuration changed")
	}
	return nil
}

func (s *Service) scriptStep(e *scriptExecution, in scriptRequest) (any, error) {
	r := e.run
	if err := s.scriptAuthority(e); err != nil {
		return nil, err
	}
	if r.Script.Expired {
		return nil, errors.New("private run journal expired")
	}
	if r.Script.Complete {
		return nil, errors.New("script already completed")
	}
	if in.Operation == "run.info" {
		if err := s.captureScriptSources(e); err != nil {
			return nil, err
		}
		deadline, _ := e.ctx.Deadline()
		return map[string]any{"deadlineAt": deadline, "capabilities": r.Spec.Script.Capabilities, "analysisIdentity": s.scriptAnalysisIdentity(r.Spec), "analysisProvider": s.scriptAnalysisProvider(r.Spec), "sources": r.Script.Sources, "packageDigest": r.Script.Digest, "packageRevision": r.Script.Revision, "id": r.ID, "dryRun": r.DryRun, "createdAt": r.Created, "now": time.Now().UTC(), "revision": r.Script.Revision, "runtime": r.Spec.Script.Runtime, "environment": r.Script.Environment, "parameters": r.Spec.Script.Parameters}, nil
	}
	if in.Operation == "checkpoint.get" {
		if !slices.Contains(r.Spec.Script.Capabilities, "checkpoint") {
			return nil, workspace.ErrForbidden
		}
		if len(r.Script.State) == 0 {
			return map[string]any{}, nil
		}
		return r.Script.State, nil
	}
	if in.Step == "" || len(in.Step) > 512 || strings.ContainsAny(in.Step, "\r\n\x00") {
		return nil, errors.New("stable step ID required")
	}
	if in.Operation == "run.id" {
		h := automation.Hash([]string{r.ID, in.Step})
		return fmt.Sprintf("%s-%s-4%s-a%s-%s", h[:8], h[8:12], h[13:16], h[17:20], h[20:32]), nil
	}
	capability := in.Operation
	if capability == "jobstate.get" || capability == "jobstate.set" {
		capability = "jobstate"
	}
	if capability == "checkpoint.set" {
		capability = "checkpoint"
	}
	if capability != "run.complete" && !slices.Contains(r.Spec.Script.Capabilities, capability) {
		return nil, workspace.ErrForbidden
	}
	if err := s.scriptCallAuthority(e, in); err != nil {
		return nil, err
	}
	var args any
	if json.Unmarshal(in.Arguments, &args) != nil {
		return nil, errors.New("invalid step arguments")
	}
	hash := automation.Hash([]any{in.Operation, args})
	if previous := r.Script.Steps[in.Step]; previous != nil {
		if previous.Hash != hash || previous.Operation != in.Operation {
			return nil, errors.New("step ID reused with different inputs")
		}
		if previous.State == "completed" {
			if previous.Error != "" {
				return nil, errors.New(previous.Error)
			}
			return previous.Result, nil
		}
		if previous.State == "failed" && previous.Effect {
			return nil, errors.New(previous.Error)
		}
		if previous.Effect {
			return nil, errors.New("step outcome unknown; automatic repeat suppressed")
		}
	}
	effect := slices.Contains([]string{"ai.structured", "ai.decisions", "decision.evaluate", "web.search", "connectors.submit", "notebooks.publish", "messages.post"}, in.Operation)
	if in.Operation == "web.fetch" {
		var capture struct {
			Render bool `json:"render"`
		}
		_ = json.Unmarshal(in.Arguments, &capture)
		effect = capture.Render
	}
	if effect && r.Script.Uncertain() {
		return nil, errors.New("an earlier paid call or write has an unknown outcome; reconcile it before starting another effect")
	}
	if in.Operation == "web.search" {
		if _, err := s.scriptSearchAdmission(e, in); err != nil {
			return nil, err
		}
	}
	step := &automation.ScriptStep{Operation: in.Operation, Input: in.Arguments, Hash: hash, State: "started", Effect: effect, Started: time.Now().UTC()}
	r.Script.Steps[in.Step] = step
	if err := e.save(); err != nil {
		return nil, err
	}
	result, err := s.scriptCachedOperation(e, in, step.Started)
	if err != nil {
		step.State = "failed"
		if receipt, ok := result.(chat.StructuredResearchResponse); ok {
			// A rejected result still owns a paid search receipt. Retain it for
			// private reconciliation; failed steps never return it to the script.
			step.Result, _ = json.Marshal(receipt)
		}
		if connectors.WriteRejected(err) || errors.Is(err, chat.ErrWebsiteCaptureNotAttempted) || errors.Is(err, messaging.ErrNotPosted) {
			step.Effect = false
		}
		step.Error = err.Error()
		if errors.Is(err, chat.ErrWebsiteCaptureSettled) || settledScriptAnalysis(in.Operation, err) {
			// The purchase has a definitive response. Retain its error as a
			// completed attempt: replay returns it without buying another capture,
			// while missing visual evidence can still be assessed as uncertain.
			step.State = "completed"
		}
		if se := e.save(); se != nil {
			return nil, se
		}
		return nil, err
	}
	if err = s.scriptAuthority(e); err != nil {
		return nil, err
	}
	step.Result, err = json.Marshal(result)
	if err != nil {
		return nil, err
	}
	step.State = "completed"
	if in.Operation == "run.complete" {
		if r.Script.Uncertain() {
			return nil, errors.New("cannot complete with unresolved external outcomes")
		}
		r.Script.Complete = true
	}
	if err = e.save(); err != nil {
		return nil, err
	}
	return result, nil
}
