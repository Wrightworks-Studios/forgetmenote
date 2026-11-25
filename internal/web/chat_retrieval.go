package web

import (
	"context"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
	"strings"
	"time"
)

// Query inference shares the explicit run deadline and leaves most remaining
// time for reading and answering. This includes waiting for a cold model's gate.
func chatEmbeddingWait(ctx context.Context, connection chat.Connection) time.Duration {
	allowance := min(30*time.Second, time.Duration(connection.Limits.RunSeconds)*time.Second/3)
	if deadline, ok := ctx.Deadline(); ok {
		allowance = min(allowance, time.Until(deadline)/3)
	}
	return max(time.Nanosecond, allowance)
}

func (s *Server) chatRetrieve(ctx context.Context, id string, run chat.Run, engine *knowledge.Engine, query knowledge.RetrievalQuery, connection chat.Connection, initial bool) (result chatRetrieval, err error) {
	ctx = knowledge.WithQueryCache(ctx)
	query, err = bindChatRetrieval(run, query)
	if err != nil {
		return result, err
	}
	started := time.Now()
	queries := 1
	defer func() {
		s.chatRetrievalDiagnostic(id, run, connection, result.RetrievalResult, err, initial, queries, time.Since(started))
	}()
	if scope := chatScopeFrom(ctx); scope != nil {
		judge, localOnly := s.jevConfigured()
		if judge != nil && !localOnly {
			prefs, prefErr := s.decisionPreferences(scope.Owner)
			if prefErr != nil {
				return result, prefErr
			}
			if prefs.Consent && prefs.Rerank {
				account, accountErr := s.catalog.Account(scope.Owner)
				if accountErr != nil {
					return result, accountErr
				}
				auth, hasAuth := ctx.Value(chatAuthContextKey{}).(runAuth)
				query.Jev = &knowledge.JevRanking{Service: judge, Scope: decision.Scope{Actor: account.ID, Notebook: scope.Project, Feature: "rerank", Run: run.ID, Chat: id, Operation: "retrieval"}, Check: func(checkCtx context.Context) error {
					if hasAuth {
						if err := s.checkRun(auth); err != nil {
							return err
						}
					}
					if err := s.jevAuthorized(checkCtx, scope.Owner, scope.Project, "rerank", judge); err != nil {
						return err
					}
					store, err := s.catalog.Access(scope.Owner, scope.Project, "read")
					if err != nil {
						return err
					}
					if store != engine.Store {
						return workspace.ErrConflict
					}
					return nil
				}}
			}
		}
	}
	query.EmbeddingWait = chatEmbeddingWait(ctx, connection)
	until := time.Now().Add(query.EmbeddingWait)
	result.RetrievalResult, err = engine.Retrieve(ctx, query)
	if err != nil {
		return result, err
	}
	focused := knowledge.FocusedRetrievalQuery(query.Text)
	remaining := time.Until(until)
	// A valid semantic query can return no candidates for conversational framing.
	// One focused query may recover them; another identical BM25 pass or another
	// timeout cannot. Never replenish the original embedding allowance.
	if initial && !query.LocalOnly && result.Mode == "hybrid" && result.Weak && len(result.Passages) == 0 && focused != "" && !strings.EqualFold(focused, query.Text) && remaining > 0 {
		queries++
		query.Text = focused
		query.EmbeddingWait = remaining
		result.RetrievalResult, err = engine.Retrieve(ctx, query)
		if err == nil {
			result.Message = "One focused query was tried after the initial semantic query returned no candidates. " + result.Message
		}
	}
	if err == nil {
		err = s.appendChatSources(ctx, run, query, &result)
	}
	return result, err
}
