package chatrun

import (
	"context"
	"encoding/json"
	"errors"

	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
)

type chatActionOutcome struct {
	result    json.RawMessage
	failure   error
	ambiguous bool
}

func classifyChatActionOutcome(result json.RawMessage, failure error) chatActionOutcome {
	outcome := chatActionOutcome{result: result, failure: failure}
	outcome.ambiguous = failure != nil && (errors.Is(failure, context.Canceled) || errors.Is(failure, context.DeadlineExceeded) || errors.Is(failure, workspace.ErrOwnerUnavailable))
	if failure != nil && !outcome.ambiguous {
		outcome.result, _ = json.Marshal(map[string]string{"error": failure.Error()})
	}
	return outcome
}

// completeChatOperation runs inside the caller's RunUpdate transaction.
// A known acknowledgement must survive subsequent revocation: persist its private
// receipt regardless of disclosure authority. Callers separately gate model/HTTP
// results and retain their own evidence construction and run transitions.
func completeChatOperation(c *chat.Conversation, run *chat.Run, id string, outcome chatActionOutcome, citations []chat.Citation, tracked bool) error {
	if outcome.ambiguous {
		return outcome.failure
	}
	for i := range run.Operations {
		operation := &run.Operations[i]
		if operation.ID != id {
			continue
		}
		operation.State = "completed"
		operation.Result = outcome.result
		operation.Citations = citations
		operation.ProvenanceComplete = tracked
		if outcome.failure != nil {
			operation.Error = outcome.failure.Error()
		}
		c.AddEvent(run.ID, "tool_result", *operation)
		return nil
	}
	return chat.ErrNotFound
}
