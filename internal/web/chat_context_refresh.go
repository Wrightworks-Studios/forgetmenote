package web

import (
	"context"
	"encoding/json"

	"forgetmenote/internal/chat"
)

// Provider projections are replaceable; original turns and operation receipts
// are not. Rebuild changed evidence without re-running any completed mutation.
func (s *Server) refreshChatContext(ctx context.Context, a chat.RunAuth, c chat.Conversation, run chat.Run, connection chat.Connection) (bool, error) {
	if err := run.Options.Corpus.Validate(); err != nil {
		return false, err
	}
	if run.Options.Review {
		return false, nil
	} // Review batches are revalidated by prepareChatReview.
	store, err := s.catalog.Access(a.User, a.Project, "read")
	if err != nil {
		return false, err
	}
	snapshot, err := chatCorpusSnapshot(ctx, store, run, "")
	if err != nil {
		return false, err
	}
	_, guidanceHash, err := s.chatInstructions(ctx, store, snapshot.Revision)
	if err != nil {
		return false, err
	}
	proof := run.Provenance
	if proof.CorpusID == run.Options.Corpus.ID() && proof.Revision == snapshot.Revision && proof.Visibility == snapshot.Visibility && proof.InstructionsHash == guidanceHash {
		return false, nil
	}
	if len(pendingToolCalls(run.Messages)) > 0 {
		return false, errChatPending
	}
	messages, citations, memory, err := s.initialChatMessages(ctx, c, run, connection)
	if err != nil {
		return false, err
	}

	for _, operation := range run.Operations {
		if operation.State != "completed" {
			continue
		}
		state := "completed"
		if operation.Error != "" {
			state = "failed"
		}
		receipt, _ := json.Marshal(map[string]string{"id": operation.ID, "name": operation.Name, "state": state, "message": "This operation already finished. Do not repeat it. Original arguments and results remain in the private receipt; current evidence supersedes earlier content."})
		messages = append(messages, chat.Message{Role: "assistant", Content: "Previously completed operation; this projection retains only its receipt identity.", ToolCalls: []chat.ToolCall{{ID: operation.ToolCallID, Name: operation.Name, Arguments: json.RawMessage(`{}`)}}}, chat.Message{Role: "tool", ToolCallID: operation.ToolCallID, Content: string(receipt)})
	}

	err = s.chatRunUpdate(a, c.ID, run.ID, func(saved *chat.Conversation, current *chat.Run) error {
		current.Messages = messages
		current.Citations = citations
		current.Provenance = memory.Provenance
		saved.Memory = memory
		saved.AddEvent(run.ID, "progress", map[string]string{"message": "Notebook evidence changed; refreshed the current run's context"})
		return nil
	})
	return err == nil, err
}
