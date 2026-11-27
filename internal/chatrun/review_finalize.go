package chatrun

import (
	"context"
	"encoding/json"
	"errors"

	"forgetmenote/internal/actions"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/knowledge"
)

func reviewFinalizationEnabled(run *chat.Run, connection chat.Connection) bool {
	return connection.Capabilities.Tools && actions.Allowed(run.Options.Mode, "complete_workflow", nil, run.Options.Actions, false)
}

// Completion is a separate bounded phase for the user-authorized Edit/Reviews
// task. It can call ordinary tools, but never writes a checkpoint automatically.
func reviewFinalizationMessages(run chat.Run, purpose string, guidance any, coverage *chat.Coverage) []chat.Message {
	checkpoints := []json.RawMessage{}
	size := 0
	for _, op := range run.Operations {
		if op.Name != "consumer_checkpoint" && op.Name != "complete_workflow" {
			continue
		}
		raw, _ := json.Marshal(map[string]any{"name": op.Name, "arguments": op.Arguments, "result": op.Result, "error": op.Error})
		if len(checkpoints) < 8 && size+len(raw) <= 12000 {
			checkpoints = append(checkpoints, raw)
			size += len(raw)
		}
	}
	payload, _ := json.Marshal(map[string]any{"progress": reviewProgress(coverage), "processedRevision": coverage.Revision, "notebookGuidance": guidance, "priorWorkflowReceipts": checkpoints})
	content := purpose + "\nThe server has verified original-note coverage at the recorded snapshot. Finish only remaining workflow operations explicitly requested in the original user task. Do not create or advance a checkpoint unless the user requested it. Preserve the original consumer, previous checkpoint and reset choice; do not replace them to bypass a conflict. Use processedRevision for completion. Earlier summaries cannot justify new factual claims or edits; read original evidence when needed. If no operation remains, respond briefly. This phase uses the remaining explicit rounds/time/tool calls; no larger allowance is granted.\n<review_finalization>" + string(payload) + "</review_finalization>"
	return []chat.Message{{Role: "user", Content: content}}
}

func reviewWorkflowCompleted(run *chat.Run) bool {
	for _, op := range run.Operations {
		if op.Name != "complete_workflow" || op.State != "completed" || op.Error != "" {
			continue
		}
		var in struct {
			Completion knowledge.CompletionInput `json:"completion"`
		}
		if json.Unmarshal(op.Arguments, &in) == nil && in.Completion.Revision == run.Review.Revision {
			return true
		}
	}
	return false
}

func ValidateReviewWorkflowArguments(run *chat.Run, arguments json.RawMessage) error {
	var in struct {
		Completion knowledge.CompletionInput `json:"completion"`
	}
	if json.Unmarshal(arguments, &in) != nil || in.Completion.Revision != run.Review.Revision {
		return errors.New("workflow completion must use the examined review snapshot")
	}
	for _, op := range run.Operations {
		if op.Name != "complete_workflow" {
			continue
		}
		var previous struct {
			Completion knowledge.CompletionInput `json:"completion"`
		}
		if json.Unmarshal(op.Arguments, &previous) == nil && previous.Completion.Consumer == in.Completion.Consumer && (previous.Completion.Previous != in.Completion.Previous || previous.Completion.Reset != in.Completion.Reset) {
			return errors.New("workflow checkpoint precondition changed; start a new user-directed task instead of rebasing a conflict")
		}
	}
	return nil
}

// A completed receipt is a replay, not a new mutation. A running intent may have
// committed before its acknowledgement was lost: only the original checkpoint's
// exact idempotency identity can bypass fresh coverage for reconciliation.
func (s *Service) ReconcileReviewWorkflow(ctx context.Context, a chat.RunAuth, run *chat.Run, call chat.ToolCall) (bool, error) {
	key := OperationID(run.ID, run.Rounds, call.ID)
	for _, op := range run.Operations {
		if op.ID != key || op.Name != "complete_workflow" {
			continue
		}
		if op.State == "completed" {
			return true, nil
		}
		if op.State != "running" {
			return false, nil
		}
		var in struct {
			Completion knowledge.CompletionInput `json:"completion"`
		}
		if json.Unmarshal(op.Arguments, &in) != nil {
			return false, errors.New("invalid original workflow intent")
		}
		store, err := s.catalog.Access(a.User, a.Project, "read")
		if err != nil {
			return false, err
		}
		checkpoint, err := withEngine(s, ctx, store, func(e *knowledge.Engine) (knowledge.Checkpoint, error) {
			return e.Checkpoint(ctx, a.User, in.Completion.Consumer)
		})
		if err != nil {
			return false, err
		}
		return checkpoint.Revision != "" && checkpoint.Revision == in.Completion.Revision && checkpoint.Summary == in.Completion.Summary, nil
	}
	return false, nil
}

// Refresh may rebuild context. Preserve the exact native assistant tool block
// and its already received sibling results when the refreshed coverage is valid.
func (s *Service) RetainPendingWorkflowPair(a chat.RunAuth, id string, prior *chat.Run, call chat.ToolCall) error {
	return s.RunUpdate(a, id, prior.ID, func(c *chat.Conversation, r *chat.Run) error {
		for _, pending := range pendingToolCalls(r.Messages) {
			if pending.ID == call.ID {
				return nil
			}
		}
		for i := len(prior.Messages) - 1; i >= 0; i-- {
			m := prior.Messages[i]
			if m.Role != "assistant" {
				continue
			}
			for _, issued := range m.ToolCalls {
				if issued.ID == call.ID {
					r.Messages = append(r.Messages, prior.Messages[i:]...)
					return nil
				}
			}
		}
		return errors.New("pending workflow lost its original provider tool block")
	})
}

// Resolve the old request explicitly as unexecuted before renewed review. Keep
// its native block until pending siblings receive their results. Mark that
// context invalid so the next review preparation rebuilds from fresh originals.
func (s *Service) DeferReviewWorkflow(a chat.RunAuth, id string, prior *chat.Run, call chat.ToolCall) error {
	message := "Review evidence changed or belongs to a prior allowance. No checkpoint was written. Complete the renewed original-source review before requesting workflow completion again."
	result, _ := json.Marshal(map[string]any{"error": message, "deferred": true, "executed": false})
	key := OperationID(prior.ID, prior.Rounds, call.ID)
	return s.RunUpdate(a, id, prior.ID, func(c *chat.Conversation, r *chat.Run) error {
		op := chat.Operation{ID: key, ToolCallID: call.ID, Round: prior.Rounds, Name: call.Name, Arguments: call.Arguments, Hash: chat.Hash(call.Arguments), State: "completed", Result: result, Error: message}
		found := false
		for i := range r.Operations {
			if r.Operations[i].ID == key {
				op.Arguments, op.Hash = r.Operations[i].Arguments, r.Operations[i].Hash
				r.Operations[i] = op
				found = true
				break
			}
		}
		if !found {
			r.Operations = append(r.Operations, op)
			r.ToolCount++
		}
		r.Messages = append(append([]chat.Message(nil), prior.Messages...), chat.Message{Role: "tool", ToolCallID: call.ID, Content: string(result)})
		r.Citations = append([]chat.Citation(nil), prior.Citations...)
		r.Provenance = cloneChatProvenance(prior.Provenance)
		r.Provenance.Complete = false
		if r.Review != nil {
			r.Review.BatchPath = ""
		}
		c.AddEvent(prior.ID, "tool_result", op)
		return nil
	})
}

func ReviewWorkflowWasDeferred(run *chat.Run) bool {
	for _, op := range run.Operations {
		if op.Round != run.Rounds || op.Name != "complete_workflow" || op.State != "completed" {
			continue
		}
		var result struct {
			Deferred bool `json:"deferred"`
			Executed bool `json:"executed"`
		}
		if json.Unmarshal(op.Result, &result) == nil && result.Deferred && !result.Executed {
			return true
		}
	}
	return false
}
