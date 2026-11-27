package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/automationrun"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Every managed connector writer (API, MCP and Python) enters here. A saved intent precedes
// provider submission. A crash leaves a barrier; a new step/run cannot erase it.
func (s *Server) dataSubmit(ctx context.Context, in dataCall, raw json.RawMessage) (connectors.Result, error) {
	if s.automations == nil {
		return connectors.Result{}, errors.New("durable automation storage unavailable")
	}
	action, _ := in.Arguments["actionID"].(string)
	if action == "" {
		return connectors.Result{}, connectors.Rejected("stable actionID required")
	}
	contract, ok := s.dataConnections.Contract(in.Connection)
	if !ok {
		return connectors.Result{}, connectors.Rejected("connection unavailable")
	}
	effect, created, err := s.automations.Queue.BeginEffect(automation.Effect{Scope: "connector:" + in.Connection, Action: action, InputHash: automation.Hash([]any{contract, in.Operation, in.Arguments}), Actor: subject(ctx), Connection: in.Connection, Operation: in.Operation})
	if err != nil {
		return connectors.Result{}, err
	}
	if !created {
		if err = s.dataConnections.CheckAccount(ctx, in.Connection); err != nil {
			return connectors.Result{}, err
		}
		var result connectors.Result
		err = json.Unmarshal(effect.Result, &result)
		return result, err
	}
	result, callErr := s.dataConnections.Call(connectors.WithSubmission(ctx), in.Connection, in.Operation, raw, in.SchemaHash)
	state := "accepted"
	if callErr != nil {
		state = "unknown"
		if connectors.WriteRejected(callErr) {
			state = "rejected"
		}
	}
	var retained any
	if callErr == nil {
		retained = result
	}
	if _, err = s.automations.Queue.FinishEffect(effect.ID, state, retained); err != nil {
		return connectors.Result{}, errors.New("submission outcome could not be saved; inspect retained effect " + effect.ID)
	}
	if callErr != nil {
		return connectors.Result{}, fmt.Errorf("%w; retained effect %s", callErr, effect.ID)
	}
	return result, nil
}
func (s *Server) dataEffectTools(server *mcp.Server) {
	addManagementTool(s, server, "automation_effects", "Inspect private cross-run analysis receipts for an installed automation.", true, func(ctx context.Context, in struct {
		ID string `json:"id"`
	}) ([]automation.Effect, error) {
		if !s.catalog.IsAdmin(subject(ctx)) {
			return nil, workspace.ErrForbidden
		}
		for _, spec := range s.automations.Definitions.Specs() {
			if spec.ID == in.ID {
				return s.automations.Queue.Effects(automationrun.AnalysisScope(spec))
			}
		}
		return nil, errors.New("automation not found")
	})

	addManagementTool(s, server, "data_connection_effects", "Inspect durable write outcomes. An unknown outcome blocks further writes to this connection until an administrator reconciles it.", true, func(ctx context.Context, in struct {
		Connection string `json:"connection"`
	}) ([]automation.Effect, error) {
		if !s.catalog.IsAdmin(subject(ctx)) {
			return nil, workspace.ErrForbidden
		}
		return s.automations.Queue.Effects("connector:" + in.Connection)
	})
	addManagementTool(s, server, "resolve_automation_effect", "Record a manually verified provider outcome with evidence. A negative search is not proof of non-submission. Resolution never repeats an action.", false, func(ctx context.Context, in struct {
		ID       string `json:"id"`
		State    string `json:"state"`
		Evidence string `json:"evidence"`
	}) (automation.Effect, error) {
		if !s.catalog.IsAdmin(subject(ctx)) {
			return automation.Effect{}, workspace.ErrForbidden
		}
		return s.automations.Queue.ResolveEffect(in.ID, in.State, in.Evidence, nil)
	})
}
