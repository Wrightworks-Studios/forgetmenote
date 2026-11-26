package chatrun

import (
	"context"
	"encoding/json"

	"forgetmenote/internal/chat"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
)

// Test-only exports: web's server-level tests reach these engine internals,
// and nothing else may. TestChatRunTestExportsStayInTests in web fails on a
// reference from production code.

type SourceContext = chatSourceContext
type Retrieval = chatRetrieval
type FailureReason = chatFailureReason

const (
	ContextLimit   = chatContextLimit
	RoundBudget    = chatRoundBudget
	ResearchBudget = chatResearchBudget
	AccessInterval = chatAccessInterval
)

func FitRequest(run chat.Run, request chat.Request, contextTokens int, estimators ...func(chat.Request) (int, error)) (chat.Request, ContextFit, error) {
	return fitChatRequest(run, request, contextTokens, estimators...)
}
func PendingToolCalls(messages []chat.Message) []chat.ToolCall { return pendingToolCalls(messages) }
func FinishReviewBatch(r *chat.Run, summary string)            { finishChatReviewBatch(r, summary) }
func EstimateTokens(v any) int                                 { return estimateTokens(v) }
func ReviewExternalAllowance(run *chat.Run) string             { return reviewExternalAllowance(run) }
func OptionalSources(messages []chat.Message, review bool) (int, string, map[string]json.RawMessage, []chat.Citation) {
	return optionalChatSources(messages, review)
}
func EvidenceValidator(snapshot workspace.Snapshot, instructionsHash string, turns []chat.Turn, corpusID string, historical ...func(chat.HistoricalDependency) bool) func(chat.Provenance, []chat.Citation) bool {
	return chatEvidenceValidator(snapshot, instructionsHash, turns, corpusID, historical...)
}
func BindDataContract(call chat.ToolCall, messages []chat.Message) chat.ToolCall {
	return bindChatDataContract(call, messages)
}
func InheritProvenance(target *chat.Provenance, turn chat.Turn) { inheritChatProvenance(target, turn) }
func ModelDataCall(call chat.ToolCall) chat.ToolCall            { return modelDataCall(call) }
func ModelSources(sources []chat.Citation) []chat.Citation      { return modelChatSources(sources) }
func LegacySourceResult(run chat.Run, name string, message chat.Message) (string, bool) {
	return legacyChatSourceResult(run, name, message)
}
func InheritExternalEvidence(target *chat.Provenance, source chat.Provenance) {
	inheritExternalEvidence(target, source)
}
func CloneProvenance(source chat.Provenance) chat.Provenance { return cloneChatProvenance(source) }
func ExcerptOriginal(text string) string                     { return chatExcerptOriginal(text) }

func (s *Service) ChatInstructions(ctx context.Context, store *workspace.Store, revision string) (map[string]any, string, error) {
	return s.chatInstructions(ctx, store, revision)
}
func (s *Service) InitialMessages(ctx context.Context, c chat.Conversation, run chat.Run, connection chat.Connection) ([]chat.Message, []chat.Citation, chat.Memory, error) {
	return s.initialChatMessages(ctx, c, run, connection)
}
func (s *Service) FailRun(ctx context.Context, auth chat.RunAuth, id, runID string, err error, fit ContextFit) {
	s.failChatRun(ctx, auth, id, runID, err, fit)
}
func (s *Service) AppendSources(ctx context.Context, run chat.Run, query knowledge.RetrievalQuery, result *Retrieval) error {
	return s.appendChatSources(ctx, run, query, result)
}
