package web

import (
	"encoding/json"
	"errors"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/automationrun"
	"forgetmenote/internal/chat"
)

// Completed calls consume their actual tool count. Started/failed calls reserve
// their maximum, including the current step, so interrupted paid work cannot
// release capacity for another search.
func scriptResearchCalls(r *automation.Run) int {
	count := 0
	for _, step := range r.Script.Steps {
		if step.Operation == "web.search" {
			count++
		}
		if step.Operation != "ai.structured" {
			continue
		}
		var v struct {
			Research *struct {
				MaxSearches int `json:"maxSearches"`
			} `json:"research"`
		}
		if json.Unmarshal(step.Input, &v) != nil || v.Research == nil {
			continue
		}
		used := v.Research.MaxSearches
		if step.State == "completed" {
			var result struct {
				ToolCalls int `json:"toolCalls"`
			}
			if json.Unmarshal(step.Result, &result) == nil && result.ToolCalls >= 1 && result.ToolCalls <= used {
				used = result.ToolCalls
			}
		}
		count += max(used, 1)
	}
	return count
}

func (s *Server) scriptResearchConfiguration() (chat.Configuration, error) {
	snapshot := s.chat.Snapshot()
	cfg := snapshot.Config
	if cfg == nil || cfg.LocalOnly || cfg.Research == nil || !cfg.Research.Enabled || cfg.Research.Format != "openai" || snapshot.Research == nil {
		return snapshot, errors.New("structured research requires configured OpenAI research")
	}
	if policy, managed := s.catalog.AISettings(); managed && !policy.Writing {
		return snapshot, errInstanceAI
	}
	return snapshot, nil
}

func (s *Server) scriptStructuredResearch(e *scriptExecution, request chat.StructuredResearchRequest) (any, error) {
	r := e.run
	snapshot, err := s.scriptResearchConfiguration()
	if err != nil {
		return nil, err
	}
	cfg, research := snapshot.Config, snapshot.Research
	if scriptResearchCalls(r) > cfg.Research.MaxSearches {
		return nil, errors.New("configured research search budget exhausted")
	}
	request.Model, request.ReasoningEffort = r.Spec.Model, r.Spec.ReasoningEffort
	ctx, model, err := s.automationrun.ModelCall(e.ctx, r, func(model automationrun.Model) error {
		if model.Connection.ID != cfg.Research.Connection {
			return errors.New("structured research must use the automation's configured research connection")
		}
		if automation.Hash(model.Config) != automation.Hash(cfg) {
			return errors.New("research configuration changed before provider admission")
		}
		estimate, err := research.EstimateStructured(request)
		if err != nil {
			return err
		}
		return automationrun.TokenBudget(r, estimate)
	})
	if err != nil {
		return nil, err
	}
	defer model.Release()
	if err = s.checkAutomationEvidence(ctx, r, true); err != nil {
		return nil, err
	}
	result, err := research.StructuredUsage(ctx, request)
	r.InputTokens += int(result.Usage.InputTokens)
	r.OutputTokens += int(result.Usage.OutputTokens)
	if err != nil {
		return result, err
	}
	if r.Spec.MaxTokens > 0 && r.InputTokens+r.OutputTokens > r.Spec.MaxTokens {
		return result, errors.New("research exceeded the automation token budget")
	}
	if err = s.checkAutomationEvidence(ctx, r, true); err != nil {
		return result, err
	}
	var value map[string]any
	if json.Unmarshal(result.Value, &value) != nil {
		return result, errors.New("research did not return a JSON object")
	}
	if _, err = validateStructuredJSON(request.Schema, value); err != nil {
		return result, err
	}
	return result, nil
}
