package web

import (
	"encoding/json"
	"errors"
	"forgetmenote/internal/automation"
	"time"
)

func (s *Server) scriptAnalysisIdentity(spec automation.Spec) string {
	// Configuration contains credential references, never credential values.
	return automation.Hash([]any{s.chat.Snapshot().Config, spec.AIConnection, spec.Model})
}

func (s *Server) scriptAnalysisProvider(spec automation.Spec) map[string]string {
	cfg := s.chat.Snapshot().Config
	if cfg == nil || spec.AIConnection == "" {
		return nil
	}
	c, err := cfg.Resolve(spec.AIConnection, spec.Model)
	if err != nil || c.Cloudflare == nil {
		return nil
	}
	model := spec.Model
	if model == "" {
		model = c.DefaultModel
	}
	return map[string]string{"provider": "cloudflare", "extractionModel": model, "decisionsModel": c.Cloudflare.DecisionModel, "searchProvider": c.Cloudflare.SearchProvider}
}
func (s *Server) scriptCachedOperation(e *scriptExecution, in scriptRequest, started time.Time) (any, error) {
	if in.Operation != "ai.structured" && in.Operation != "ai.decisions" && in.Operation != "decision.evaluate" {
		return s.scriptOperation(e, in, started)
	}
	var args map[string]any
	if json.Unmarshal(in.Arguments, &args) != nil {
		return nil, errors.New("invalid analysis arguments")
	}
	key, _ := args["cacheKey"].(string)
	if key == "" || e.run.DryRun {
		return s.scriptOperation(e, in, started)
	}
	if len(key) > 512 {
		return nil, errors.New("analysis cache key exceeds 512 characters")
	}
	spec := e.run.Spec
	hours := spec.Script.RetentionHours
	if hours == 0 {
		hours = 24
	}
	scope := scriptAnalysisScope(spec)
	effect, created, err := s.automations.Queue.BeginEffect(automation.Effect{Scope: scope, Action: automation.Hash([]string{in.Operation, key, s.scriptAnalysisIdentity(spec)}), InputHash: automation.Hash(args), Actor: spec.Actor, Operation: in.Operation, RetainUntil: started.Add(time.Duration(hours) * time.Hour)})
	if err != nil {
		return nil, err
	}
	if !created {
		return effect.Result, nil
	}
	result, err := s.scriptOperation(e, in, started)
	state := "accepted"
	if err != nil {
		state = "unknown"
		if settledScriptAnalysis(in.Operation, err) {
			state = "rejected"
		}
		result = nil
	}
	if _, saveErr := s.automations.Queue.FinishEffect(effect.ID, state, result); saveErr != nil {
		return nil, saveErr
	}
	return result, err
}

func scriptAnalysisScope(spec automation.Spec) string {
	return "analysis:" + automation.Hash([]string{spec.ID, spec.Actor, spec.Project})
}
