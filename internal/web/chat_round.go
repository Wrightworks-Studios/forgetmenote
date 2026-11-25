package web

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"forgetmenote/internal/chat"
	"forgetmenote/internal/personal"
	"forgetmenote/internal/workspace"
)

// chatRunner executes one admitted chat run in phases: prepare, then rounds
// of fit, stream, persist and tools. The phases share the run's identity,
// connection and access monitor, and the last request's fit, which the
// terminal diagnostic reports.
type chatRunner struct {
	s          *Server
	auth       chat.RunAuth
	id, runID  string
	connection chat.Connection
	access     *chatRunAccess
	lastFit    chatContextFit
}

// errChatRunGone stops a runner whose run can no longer be read without
// reporting a failure; recoverChatRun settles the run.
var errChatRunGone = errors.New("chat run is unavailable")

// chatRoundRequest is one round's model request, fitted for sending.
type chatRoundRequest struct {
	provider *chat.Provider
	request  chat.Request
	fit      chatContextFit
}

// chatStreamed is what one streamed model request produced.
type chatStreamed struct {
	assistantID string
	response    chat.Response
	// err is the provider's failure, or the revocation that stopped the stream.
	err error
	// flushErr is the failure to persist the last streamed text.
	flushErr error
}

// settle reports how the runner stopped. A run that completed, waits for a
// confirmation or can no longer be read needs nothing more here.
func (r *chatRunner) settle(ctx context.Context, err error) {
	if err == nil || errors.Is(err, errChatConfirmation) || errors.Is(err, errChatRunGone) {
		return
	}
	r.s.failChatRun(ctx, r.auth, r.id, r.runID, err, r.lastFit)
}

// load reads the run.
func (r *chatRunner) load() (chat.Conversation, *chat.Run, error) {
	c, err := r.s.chat.Store.Read(r.auth.User, r.auth.Project, r.id)
	if err != nil {
		return c, nil, errChatRunGone
	}
	run := c.Run(r.runID)
	if run == nil {
		return c, nil, errChatRunGone
	}
	return c, run, nil
}

// prepare checks the run's access, binds ctx to its scope and access monitor
// and, for a new run, retrieves its initial evidence. It returns the context
// for the later phases, the function that stops the monitor, and the run as
// saved.
func (r *chatRunner) prepare(ctx context.Context) (context.Context, func(), *chat.Run, error) {
	stop := func() {}
	if err := r.s.checkRun(r.auth); err != nil {
		return ctx, stop, nil, err
	}
	c, run, err := r.load()
	if err != nil {
		return ctx, stop, nil, err
	}
	if err := run.Options.Corpus.Validate(); err != nil {
		return ctx, stop, nil, err
	}
	// A failed scope returns no context; report the failure with the run's own.
	scoped, err := chat.WithRunScope(ctx, c, *run)
	if err != nil {
		return ctx, stop, nil, err
	}
	r.access = &chatRunAccess{run: *run}
	ctx, stop = r.s.monitorChatAccess(scoped, r.auth, r.access)
	if len(run.Messages) == 0 && !run.Options.Review {
		run, err = r.retrieveInitialEvidence(ctx, c, *run)
	}
	return ctx, stop, run, err
}

// retrieveInitialEvidence builds a new run's first request from committed
// notebook evidence and returns the run as saved with it.
func (r *chatRunner) retrieveInitialEvidence(ctx context.Context, c chat.Conversation, run chat.Run) (*chat.Run, error) {
	if err := r.s.chatRunUpdate(r.auth, r.id, r.runID, func(c *chat.Conversation, current *chat.Run) error {
		current.State = "running"
		c.AddEvent(r.runID, "progress", map[string]string{"message": "Retrieving committed notebook evidence"})
		return nil
	}); err != nil {
		return nil, err
	}
	messages, citations, memory, err := r.s.initialChatMessages(ctx, c, run, r.connection)
	if err != nil {
		return nil, err
	}
	if err := r.s.checkRun(r.auth); err != nil {
		return nil, err
	}
	var saved chat.Run
	err = r.s.chatRunUpdate(r.auth, r.id, r.runID, func(c *chat.Conversation, current *chat.Run) error {
		current.Messages = messages
		current.Citations = citations
		current.Provenance = memory.Provenance
		c.Memory = memory
		c.AddEvent(r.runID, "citations", citations)
		saved = *current
		return nil
	})
	return &saved, err
}

// rounds executes a resumed run's pending tool calls, then model rounds until
// the run completes or a tool call waits for confirmation.
func (r *chatRunner) rounds(ctx context.Context, run *chat.Run) error {
	r.access.set(*run)
	if err := r.executeTools(ctx, pendingToolCalls(run.Messages)); err != nil {
		return err
	}
	review := run.Options.Review
	for {
		done, err := r.round(ctx, review)
		if done || err != nil {
			return err
		}
	}
}

// round requests one model response and executes the tool calls it makes. It
// reports done once the run is terminal.
func (r *chatRunner) round(ctx context.Context, review bool) (bool, error) {
	if review {
		if done, err := r.prepareReview(ctx); done || err != nil {
			return done, err
		}
	}
	c, run, err := r.begin(ctx)
	if err != nil {
		return false, err
	}
	// Changed evidence replaces the run's context; fit only the request
	// that will be sent.
	if refreshed, err := r.s.refreshChatContext(ctx, r.auth, c, *run, r.connection); err != nil || refreshed {
		return false, err
	}
	next, err := r.fit(ctx, run)
	if err != nil {
		return false, err
	}
	streamed, err := r.stream(ctx, *run, next)
	if err != nil {
		return false, err
	}
	if err := r.persist(next.request, streamed); err != nil {
		return false, err
	}
	calls := streamed.response.Message.ToolCalls
	if len(calls) > 0 {
		return false, r.executeTools(ctx, calls)
	}
	if review && !run.Review.Finalizing {
		return false, nil
	}
	r.complete(ctx, c, streamed.response.Message.Content)
	return true, nil
}

// prepareReview prepares a review's next batch. Once the inventory is complete
// and no workflow remains to finalize, the run completes.
func (r *chatRunner) prepareReview(ctx context.Context) (bool, error) {
	complete, err := r.s.prepareChatReview(ctx, r.auth, r.id, r.runID, r.connection)
	if err != nil || !complete {
		return false, err
	}
	current, err := r.s.chat.Store.Read(r.auth.User, r.auth.Project, r.id)
	if err != nil {
		return false, err
	}
	run := current.Run(r.runID)
	if run.Review.Finalizing && !reviewWorkflowCompleted(run) {
		return false, nil
	}
	r.s.chatTerminal(r.auth, r.id, r.runID, "completed", errors.New("Review inventory completed at its recorded revisions; changed notes are rechecked on the next review."), r.lastFit)
	return true, nil
}

// begin loads the run for a round, which also observes tool results and
// cancellations from other gateways, and checks that the run may continue:
// its context is live, its access holds, nobody cancelled it and its round
// budget remains.
func (r *chatRunner) begin(ctx context.Context) (chat.Conversation, *chat.Run, error) {
	if err := ctx.Err(); err != nil {
		return chat.Conversation{}, nil, err
	}
	if err := r.s.checkRun(r.auth); err != nil {
		return chat.Conversation{}, nil, err
	}
	c, run, err := r.load()
	if err != nil {
		return c, nil, err
	}
	r.access.set(*run)
	if run.CancelRequested {
		return c, nil, context.Canceled
	}
	if run.Rounds >= max(run.RoundLimit, r.connection.Limits.MaxRounds) {
		return c, nil, chatFailureError(chatRoundBudget, "Model round budget reached; review progress before continuing")
	}
	return c, run, nil
}

// fit builds the round's request from the run's messages, tools and the
// person's memory, and fits it to the connection's context.
func (r *chatRunner) fit(ctx context.Context, run *chat.Run) (chatRoundRequest, error) {
	provider := r.s.chat.Snapshot().Providers[r.connection.ID]
	if provider == nil {
		return chatRoundRequest{}, chat.ErrProviderRequest
	}
	memory, err := r.s.personalChatContext(ctx)
	if err != nil {
		return chatRoundRequest{}, err
	}
	if err := r.bindPersonalConsent(run, memory); err != nil {
		return chatRoundRequest{}, err
	}
	request := chat.Request{Model: run.Options.Model, System: chatSystem + memory, Messages: run.Messages, Tools: r.s.runTools(*run, r.connection), MaxOutputTokens: r.connection.Limits.OutputTokens}
	fitted, fit, err := fitChatRound(ctx, *run, request, r.connection, provider.EstimateRequest)
	r.lastFit = fit
	return chatRoundRequest{provider: provider, request: fitted, fit: fit}, err
}

// bindPersonalConsent binds the run to the person's current consent the first
// time it uses their memory; a changed consent stops the run.
func (r *chatRunner) bindPersonalConsent(run *chat.Run, memory string) error {
	if memory == "" {
		return nil
	}
	stamp := r.s.personalConsentStamp(r.auth.User)
	if run.PersonalConsent != "" && run.PersonalConsent != stamp {
		return workspace.ErrForbidden
	}
	if run.PersonalConsent != "" {
		return nil
	}
	if err := r.s.chatRunUpdate(r.auth, r.id, r.runID, func(c *chat.Conversation, current *chat.Run) error {
		current.PersonalConsent = stamp
		return nil
	}); err != nil {
		return err
	}
	run.PersonalConsent = stamp
	r.access.set(*run)
	return nil
}

// stream starts the round and streams the model's response, persisting its
// text as it arrives. A revocation found during or after the stream replaces
// the response with its error.
func (r *chatRunner) stream(ctx context.Context, run chat.Run, next chatRoundRequest) (chatStreamed, error) {
	if err := ctx.Err(); err != nil {
		return chatStreamed{}, err
	}
	if err := r.s.validateChatSources(ctx, run, true); err != nil {
		return chatStreamed{}, err
	}
	out := chatStreamed{assistantID: chat.NewID()}
	var pending strings.Builder
	lastFlush := time.Now()
	// The access monitor rechecks the session, consent and cited grants
	// while text streams; a flush only persists it.
	flush := func() error {
		if pending.Len() == 0 {
			return nil
		}
		text := pending.String()
		if err := r.s.chatRunUpdate(r.auth, r.id, r.runID, func(c *chat.Conversation, current *chat.Run) error {
			c.AddEvent(r.runID, "text_delta", map[string]string{"turnId": out.assistantID, "text": text})
			return nil
		}); err != nil {
			return err
		}
		pending.Reset()
		lastFlush = time.Now()
		return nil
	}
	if err := r.startRound(run, next); err != nil {
		return out, err
	}
	out.response, out.err = next.provider.Generate(ctx, next.request, func(event chat.Event) error {
		if event.Type == "text_delta" {
			pending.WriteString(event.Text)
			if pending.Len() >= 1024 || time.Since(lastFlush) > 150*time.Millisecond {
				return flush()
			}
		}
		return nil
	})
	sourceErr := chatAccessFailure(ctx)
	if sourceErr == nil {
		sourceErr = r.s.validateChatSources(ctx, run, true)
	}
	if sourceErr != nil {
		out.err = sourceErr
		out.response.Message = chat.Message{}
		pending.Reset()
	}
	out.flushErr = flush()
	return out, nil
}

// startRound counts the round and reserves its estimated tokens before the
// request is sent.
func (r *chatRunner) startRound(run chat.Run, next chatRoundRequest) error {
	fit, request := next.fit, next.request
	if err := r.s.chatRunUpdate(r.auth, r.id, r.runID, func(c *chat.Conversation, current *chat.Run) error {
		current.State = "running"
		current.Rounds++
		if fit.Changed {
			c.AddEvent(r.runID, "progress", map[string]any{"message": "Reduced optional context while preserving the task and tool results", "inputTokensBefore": fit.BeforeTokens, "inputTokensAfter": fit.AfterTokens})
		}
		current.InFlightTokens = int64(fit.AfterTokens + request.MaxOutputTokens)
		current.InFlightDay = chat.Day()
		progress := "Generating response"
		if request.FinalAnswer {
			progress = "Preparing the final answer from retained original evidence"
		}
		c.AddEvent(r.runID, "progress", map[string]any{"message": progress, "round": current.Rounds, "answerPhase": request.FinalAnswer})
		return nil
	}); err != nil {
		return err
	}
	diagnostic := run
	diagnostic.Rounds++
	diagnostic.State = "running"
	r.s.chatDiagnostic("request", r.id, diagnostic, fit, "generating", true)
	return nil
}

// persist settles the round's usage and records the assistant's turn and,
// unless the round failed, its message for the next request. A run whose
// access was revoked records only its usage. It then reports the round's
// failure, if any.
func (r *chatRunner) persist(request chat.Request, streamed chatStreamed) error {
	usage := streamed.response.Usage
	uncertain := int64(0)
	if streamed.err != nil && usage.Estimated {
		uncertain = max(0, int64(request.MaxOutputTokens)-usage.OutputTokens)
	}
	if err := r.s.chat.Store.SettleUsage(r.auth.User, r.auth.Project, r.id, r.runID, usage, uncertain); err != nil {
		return err
	}
	// Authorize before taking the conversation store's lock.
	authorized := r.s.checkRun(r.auth) == nil
	if err := r.s.chatRunUpdate(r.auth, r.id, r.runID, func(c *chat.Conversation, current *chat.Run) error {
		if !authorized {
			return nil
		}
		m := streamed.response.Message
		if m.Content != "" || len(m.ToolCalls) > 0 {
			c.Turns = append(c.Turns, chat.Turn{ID: streamed.assistantID, RunID: r.runID, Role: "assistant", Content: m.Content, Provenance: current.Provenance, Citations: append([]chat.Citation(nil), current.Citations...), Provider: &m})
			c.AddEvent(r.runID, "assistant", map[string]any{"id": streamed.assistantID, "role": "assistant", "intermediate": len(m.ToolCalls) > 0, "content": m.Content, "citations": current.Citations})
			if streamed.err == nil {
				current.Messages = append(current.Messages, m)
			}
		}
		if streamed.err == nil && len(m.ToolCalls) == 0 && current.Options.Review {
			finishChatReviewBatch(current, m.Content)
			c.AddEvent(r.runID, "review", reviewProgress(current.Review))
		}
		c.AddEvent(r.runID, "usage", current.Usage)
		return nil
	}); err != nil {
		return err
	}
	if streamed.err != nil {
		return streamed.err
	}
	return streamed.flushErr
}

// executeTools executes tool calls in order, stopping at the first that fails
// or waits for confirmation.
func (r *chatRunner) executeTools(ctx context.Context, calls []chat.ToolCall) error {
	for _, call := range calls {
		if err := r.s.executeChatTool(ctx, r.auth, r.id, r.runID, call, r.connection); err != nil {
			return err
		}
	}
	return nil
}

// complete settles a run whose model answered without tool calls and offers
// the exchange to the person's memory.
func (r *chatRunner) complete(ctx context.Context, c chat.Conversation, answer string) {
	r.s.chatTerminal(r.auth, r.id, r.runID, "completed", nil, r.lastFit)
	question := ""
	for _, turn := range c.Turns {
		if turn.RunID == r.runID && turn.Role == "user" {
			question = turn.Content
			break
		}
	}
	source := r.s.origin + "/?project=" + url.QueryEscape(r.auth.Project) + "&chat=" + url.QueryEscape(r.id)
	if err := r.s.recordPersonalInteraction(ctx, r.auth.User, personal.Interaction{ID: "chat-" + r.id + "-" + r.runID, UserText: question, Response: answer, Source: source}); err != nil {
		slog.Warn("personal memory capture failed", "error", err)
	}
}
