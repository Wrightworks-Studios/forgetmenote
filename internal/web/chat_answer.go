package web

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/chat"
	"time"
)

func ordinaryChatAsk(run chat.Run) bool { return run.Options.Mode == "ask" && !run.Options.Review }

func chatAnswerDue(ctx context.Context, run chat.Run, connection chat.Connection) bool {
	if !ordinaryChatAsk(run) {
		return false
	}
	if chatAnswerBudgetDue(ctx, run.Rounds, max(run.RoundLimit, connection.Limits.MaxRounds), run.ToolCount, max(run.ToolLimit, connection.Limits.MaxTools), connection.Limits.RunSeconds) {
		return true
	}
	// Notebook retrieval gets two exploration requests. Data workflows must be
	// able to discover a connection, inspect its schema, then execute; they use
	// the configured allowance while still reserving the last request to answer.
	allowance := max(0, max(run.RoundLimit, connection.Limits.MaxRounds)-1)
	exploratory := min(2, allowance)
	for _, op := range run.Operations {
		if op.State != "completed" || op.Error != "" {
			continue
		}
		switch op.Name {
		case "find_recipes", "list_data_connections", "list_data_operations", "call_data_operation", "assistant_status", "assistant_context", "read_personal_source":
			exploratory = allowance
		}
	}
	return run.Rounds >= exploratory
}

func chatAnswerBudgetDue(ctx context.Context, rounds, roundLimit, tools, toolLimit, runSeconds int) bool {
	if rounds >= max(0, roundLimit-1) || tools >= toolLimit {
		return true
	}
	if deadline, ok := ctx.Deadline(); ok {
		margin := min(30*time.Second, time.Duration(runSeconds)*time.Second/3)
		if time.Until(deadline) <= margin {
			return true
		}
	}
	return false
}

func finalChatRequest(request chat.Request) chat.Request {
	request.FinalAnswer = true
	request.System += "\n\nFinish the user's current question now. No further tools are available for this answer. Answer naturally and concisely from retained original excerpts. Put Markdown links [source title](exact supplied URL) beside factual summaries or table entries, using the supplied titles and pinned URLs. Do not substitute bare filenames or invent citations. Mention uncertainty, gaps or source distinctions only when they affect the question; omit unrelated retrieval narration. An omitted result does not mean the evidence is absent. Give the useful supported answer even when coverage is partial; do not claim a complete notebook review or invent missing facts. Do not respond with another plan to search."
	return request
}

// An excess native read call must receive an honest result before an Ask answer
// can continue. The omission and its matching native result are one durable
// update; no workspace operation is dispatched and no tool allowance is spent.
func (s *Server) omitChatBudgetRead(ctx context.Context, a chat.RunAuth, id string, run chat.Run, call chat.ToolCall, connection chat.Connection) error {
	read := false
	switch call.Name {
	case "search_passages", "discover_actions":
		read = true
	case "research_search", "research_fetch":
		read = run.Options.Research
	default:
		read = policy(call.Name).Read && allowedAction(run.Options.Mode, call.Name, call.Arguments, run.Options.Actions, false)
	}
	if !ordinaryChatAsk(run) || !read || !offersChatTool(s.runTools(run, connection), call.Name) {
		return chatFailureError(chatToolBudget, "Tool-call budget reached")
	}
	key := operationID(run.ID, run.Rounds, call.ID)
	const message = "Tool-call budget reached. This call was not executed; use retained original evidence and report what remains unexamined."
	result := json.RawMessage(`{"error":"` + message + `","executed":false,"omitted":true}`)
	// Authorize before taking the conversation store's lock.
	if err := s.checkRun(a); err != nil {
		return err
	}
	return s.chatRunUpdate(a, id, run.ID, func(c *chat.Conversation, r *chat.Run) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.CancelRequested {
			return context.Canceled
		}
		for _, prior := range r.Operations {
			if prior.ID == key {
				if prior.State == "omitted" && prior.Name == call.Name && prior.Hash == chat.Hash(call.Arguments) {
					return nil
				}
				return chat.ErrConflict
			}
		}
		pending := false
		for _, native := range pendingToolCalls(r.Messages) {
			if native.ID == call.ID && native.Name == call.Name {
				pending = true
				break
			}
		}
		if !pending {
			return chat.ErrConflict
		}
		op := chat.Operation{ID: key, ToolCallID: call.ID, Round: r.Rounds, Name: call.Name, Arguments: call.Arguments, Hash: chat.Hash(call.Arguments), State: "omitted", Result: result, Error: message}
		r.Operations = append(r.Operations, op)
		recordChatToolResult(c, r, call.ID, string(result), nil, true)
		c.AddEvent(run.ID, "tool_result", op)
		return nil
	})
}
