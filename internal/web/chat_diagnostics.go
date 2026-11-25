package web

import (
	"context"
	"errors"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/knowledge"
	"log/slog"
	"time"
)

type chatFailureReason string

const (
	chatContextLimit    chatFailureReason = "context_limit"
	chatRoundBudget     chatFailureReason = "round_budget"
	chatToolBudget      chatFailureReason = "tool_budget"
	chatResearchBudget  chatFailureReason = "research_budget"
	chatAccessRevoked   chatFailureReason = "access_revoked"
	chatInternalFailure chatFailureReason = "internal_failure"
)

func (r chatFailureReason) Error() string { return string(r) }

type chatFailure struct {
	reason  chatFailureReason
	message string
}

func (e *chatFailure) Error() string { return e.message }
func (e *chatFailure) Unwrap() error { return e.reason }
func chatFailureError(reason chatFailureReason, message string) error {
	return &chatFailure{reason, message}
}

func chatFailureCategory(state string, err error) string {
	if state == "completed" || state == "cancelled" {
		return state
	}
	var reason chatFailureReason
	if errors.As(err, &reason) {
		switch reason {
		case chatContextLimit, chatRoundBudget, chatToolBudget, chatResearchBudget, chatAccessRevoked, chatInternalFailure:
			return string(reason)
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, chat.ErrProviderRequest):
		return "provider_request"
	case errors.Is(err, chat.ErrProviderContract):
		return "provider_output"
	case state == "uncertain":
		return "uncertain_operation"
	default:
		return "operation_failed"
	}
}

// Only structural, server-controlled fields cross this logging boundary. Never
// include error strings, endpoints, model text, arguments, results or note paths.
func (s *Server) chatDiagnostic(stage, id string, run chat.Run, fit chatContextFit, category string, persisted bool) {
	format := "unknown"
	cfg := s.chat.Snapshot().Config
	if cfg != nil {
		if connection, err := cfg.Resolve(run.Options.Connection, run.Options.Model); err == nil {
			format = connection.Format
		}
	}
	omitted := 0
	for _, op := range run.Operations {
		if op.State == "omitted" {
			omitted++
		}
	}
	attributes := []any{"chat", id, "run", run.ID, "mode", run.Options.Mode, "connection", run.Options.Connection, "format", format, "state", run.State, "rounds", run.Rounds, "tools", run.ToolCount, "omitted_tools", omitted, "requests", run.Usage.Requests, "input_tokens", fit.AfterTokens, "input_tokens_before", fit.BeforeTokens, "sources_kept", fit.SourcesAfter, "sources_total", fit.SourcesBefore, "answer_phase", fit.FinalAnswer, "usage_input_tokens", run.Usage.InputTokens, "usage_output_tokens", run.Usage.OutputTokens, "usage_uncertain_tokens", run.Usage.UncertainTokens, "category", category, "persisted", persisted}
	if stage == "terminal" && run.State != "completed" {
		slog.Warn("chat run terminal", attributes...)
		return
	}
	slog.Info("chat run "+stage, attributes...)
}

// recoverChatRun settles every runner exit, including a panic or a failed read
// of the run's own conversation: while a run is unfinished its owner can start
// no other conversation, and recovery skips the runs of a live gateway. Only a
// deliberate pause for confirmation outlives the runner.
func (s *Server) recoverChatRun(auth chat.RunAuth, id, runID string) {
	panicked := recover() != nil
	// A panic value or stack can contain arbitrary notebook/provider content.
	// Persist a generic terminal outcome and let ordinary structural logging cover it.
	s.settleChatRun(auth, id, runID, "failed", chatFailureError(chatInternalFailure, "Chat runner stopped unexpectedly; inspect the saved run before continuing."), func(run chat.Run) bool {
		return chat.Active(run.State) && (panicked || run.State != "awaiting_confirmation")
	})
}

func (s *Server) chatRetrievalDiagnostic(id string, run chat.Run, connection chat.Connection, result knowledge.RetrievalResult, err error, initial bool, queries int, elapsed time.Duration) {
	mode := "unknown"
	if result.Mode == "bm25" || result.Mode == "hybrid" {
		mode = result.Mode
	}
	semantic := "unknown"
	switch result.Semantic {
	case "disabled", "local_only", "unindexed", "ready", "unavailable", "timeout", "changed":
		semantic = result.Semantic
	}
	if err != nil {
		semantic = "failed"
		if errors.Is(err, context.Canceled) {
			semantic = "cancelled"
		} else if errors.Is(err, context.DeadlineExceeded) {
			semantic = "deadline"
		}
	}
	phase := "tool"
	if initial {
		phase = "initial"
	}
	slog.Info("chat retrieval", "chat", id, "run", run.ID, "mode", run.Options.Mode, "connection", connection.ID, "format", connection.Format, "phase", phase, "retrieval_mode", mode, "semantic", semantic, "elapsed_ms", elapsed.Milliseconds(), "queries", queries, "passages", len(result.Passages), "evidence_tokens", result.EstimatedTokens, "weak", result.Weak)
}
