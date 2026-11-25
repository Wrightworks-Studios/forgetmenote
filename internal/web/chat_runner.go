package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
)

type chatSubmission struct {
	SubmissionID string            `json:"submissionId"`
	Text         string            `json:"text"`
	Options      chat.Options      `json:"options"`
	Attachments  []chat.Attachment `json:"attachments,omitempty"`
}

func (s *Server) chatRunAuth(r *http.Request, c chat.Conversation) chat.RunAuth {
	a := chat.RunAuth{User: c.Owner, Project: c.Project, Epoch: s.catalog.Epoch(c.Owner)}
	if token, ok := sessionToken(r); ok {
		a.Session = digest(token)
	}
	return a
}
func (s *Server) checkRun(a chat.RunAuth) error {
	if !s.catalog.ValidSession(a.User, a.Epoch) {
		return workspace.ErrForbidden
	}
	if a.Session != "" {
		g, ok := s.oauth.Session(a.Session)
		if !ok || g.Subject != a.User || g.Expires <= time.Now().Unix() || g.Epoch != a.Epoch {
			return workspace.ErrForbidden
		}
	}
	_, err := s.catalog.Access(a.User, a.Project, "read")
	return err
}
func (s *Server) runContext(a chat.RunAuth) (context.Context, error) {
	account, err := s.catalog.Account(a.User)
	if err != nil {
		return nil, err
	}
	ctx := context.WithValue(context.Background(), identityKey{}, a.User)
	ctx = chat.WithRunAuth(ctx, a)
	return attribution.WithSource(ctx, account.Identity(), "chat", "embedded"), s.checkRun(a)
}
func (s *Server) chatTurn(w http.ResponseWriter, r *http.Request) {
	c, err := s.chatAuthorized(r)
	if err != nil {
		chatFail(w, err)
		return
	}
	var in chatSubmission
	if !decode(w, r, &in) {
		return
	}
	if len(in.SubmissionID) < 1 || len(in.SubmissionID) > 128 || strings.TrimSpace(in.Text) == "" || len(in.Text) > 16<<10 || !utf8.ValidString(in.Text) {
		http.Error(w, "Provide a submission ID and 1–16384 bytes of message text", 400)
		return
	}
	canonical, _ := json.Marshal(in)
	hash := chat.Hash(canonical)
	for _, prior := range c.Runs {
		if prior.SubmissionID != in.SubmissionID {
			continue
		}
		if prior.RequestHash != hash {
			chatFail(w, chat.ErrConflict)
			return
		}
		jsonResponse(w, map[string]any{"chat": c.ID, "run": prior.ID, "state": prior.State, "replayed": true})
		return
	}
	opts := in.Options
	if opts.Connection == "" {
		opts.Connection = c.Options.Connection
	}
	if opts.Model == "" {
		opts.Model = c.Options.Model
	}
	if opts.Mode == "" {
		opts.Mode = c.Options.Mode
	}
	if err = validateChatOptions(opts); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	cfg := s.chat.Snapshot().Config
	connection, err := cfg.Resolve(opts.Connection, opts.Model)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	opts.Connection = connection.ID
	if opts.Model == "" {
		opts.Model = connection.DefaultModel
	}
	if opts.Research && (cfg.Research == nil || !cfg.Research.Enabled || cfg.LocalOnly) {
		http.Error(w, "External research is disabled", 400)
		return
	}
	if opts.Mode == "edit" {
		if _, err = s.catalog.Access(c.Owner, c.Project, "edit"); err != nil {
			chatFail(w, err)
			return
		}
	}
	if opts.Mode == "suggest" {
		if _, err = s.catalog.Access(c.Owner, c.Project, "comment"); err != nil {
			chatFail(w, err)
			return
		}
	}
	// Older clients may still send a token budget; it does not constrain work.
	opts.BudgetTokens = 0
	if opts.Corpus == nil && c.Version == 2 && c.Options.Corpus == nil {
		http.Error(w, chat.ErrCorpus.Error(), 409)
		return
	}
	opts.Corpus, err = s.resolveChatCorpus(r.Context(), projectStore(r.Context()), opts.Corpus, c.Options.Corpus, c.Version == 1)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	attachments, err := s.chatAttachments(r.Context(), projectStore(r.Context()), in.Attachments)
	if err != nil {
		chatFail(w, err)
		return
	}
	auth := s.chatRunAuth(r, c)
	base, err := s.runContext(auth)
	if err != nil {
		chatFail(w, err)
		return
	}
	now := chat.Now()
	run := chat.Run{Provenance: chat.Provenance{CorpusID: opts.Corpus.ID()}, ID: chat.NewID(), SubmissionID: in.SubmissionID, RequestHash: hash, State: "queued", RoundLimit: connection.Limits.MaxRounds, ToolLimit: connection.Limits.MaxTools, Options: opts, Epoch: auth.Epoch, CreatedAt: now, UpdatedAt: now}
	turn := chat.Turn{ID: chat.NewID(), RunID: run.ID, Role: "user", Content: in.Text, Attachments: attachments}
	ctx, cancel := context.WithTimeout(base, time.Duration(connection.Limits.RunSeconds)*time.Second)
	var created chat.Conversation
	replayed, err := s.chat.Admit(cfg, connection, run.ID, cancel, c.Owner, s.pausedChatAllowed, func() (bool, error) {
		var replay bool
		var err error
		created, replay, err = s.chat.Store.Start(c.Owner, c.Project, c.ID, run, turn)
		return replay, err
	})
	if err != nil {
		cancel()
		if errors.Is(err, chat.ErrCapacity) {
			http.Error(w, "Chat is busy; retry shortly", 429)
		} else {
			chatFail(w, err)
		}
		return
	}
	if replayed {
		cancel()
		for _, prior := range created.Runs {
			if prior.SubmissionID == in.SubmissionID {
				jsonResponse(w, map[string]any{"chat": c.ID, "run": prior.ID, "state": prior.State, "replayed": true})
				return
			}
		}
		return
	}
	go s.executeChat(ctx, cancel, auth, c.ID, run.ID, connection)
	jsonResponse(w, map[string]any{"chat": c.ID, "run": run.ID, "state": "queued"})
}
func (s *Server) chatRunUpdate(a chat.RunAuth, id, runID string, fn func(*chat.Conversation, *chat.Run) error) error {
	return s.chat.Store.Update(a.User, a.Project, id, func(c *chat.Conversation) error {
		run := c.Run(runID)
		if run == nil {
			return chat.ErrNotFound
		}
		run.UpdatedAt = chat.Now()
		return fn(c, run)
	})
}
func (s *Server) chatTerminal(a chat.RunAuth, id, runID, state string, cause error, fits ...chatContextFit) {
	if state == "completed" {
		// Persist the pending check before clients can observe completion.
		// The checker independently requires operator enablement, personal remote
		// processing consent, and the personal citation-check preference.
		_ = s.startCitationCheck(a, id, runID, false)
	}
	s.settleChatRun(a, id, runID, state, cause, nil, fits...)
}

var errChatSettled = errors.New("chat run already settled")

// settleChatRun records a terminal state, unless unsettled reports that the
// saved run no longer needs one; that decision and the write are one update.
func (s *Server) settleChatRun(a chat.RunAuth, id, runID, state string, cause error, unsettled func(chat.Run) bool, fits ...chatContextFit) {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	diagnostic := chat.Run{ID: runID, State: state}
	err := s.chatRunUpdate(a, id, runID, func(c *chat.Conversation, r *chat.Run) error {
		if unsettled != nil && !unsettled(*r) {
			return errChatSettled
		}
		if r.InFlightTokens > 0 {
			r.Usage.UncertainTokens += r.InFlightTokens
			r.Usage.Estimated = true
			r.Usage.Requests++
			r.InFlightTokens = 0
		}
		r.State = state
		r.Error = message
		if state != "awaiting_confirmation" {
			r.Usage.Reserved = 0
		}
		c.AddEvent(runID, "state", map[string]any{"state": state, "message": message, "usage": r.Usage})
		diagnostic = *r
		return nil
	})
	if errors.Is(err, errChatSettled) {
		return
	}
	fit := chatContextFit{}
	if len(fits) > 0 {
		fit = fits[0]
	}
	category := chatFailureCategory(state, cause)
	if err != nil {
		category = "persistence"
	}
	s.chatDiagnostic("terminal", id, diagnostic, fit, category, err == nil)
}
func (s *Server) failChatRun(ctx context.Context, auth chat.RunAuth, id, runID string, err error, fit chatContextFit) {
	state := "failed"
	if errors.Is(err, chatRoundBudget) || errors.Is(err, chatToolBudget) || errors.Is(err, chatResearchBudget) {
		state = "interrupted"
	}
	if errors.Is(err, errChatPending) {
		state = "interrupted"
	}
	if errors.Is(err, errChatUncertain) {
		state = "uncertain"
	}
	if revoked := chatAccessFailure(ctx); revoked != nil {
		// The access monitor stopped the work; report what it found, as a
		// check made during the work would have.
		if errors.Is(err, context.Canceled) {
			err = revoked
		}
	} else if ctx.Err() != nil {
		state = "interrupted"
	}
	c, e := s.chat.Store.Read(auth.User, auth.Project, id)
	if e == nil {
		if r := c.Run(runID); r != nil && r.CancelRequested {
			state = "cancelled"
		}
	}
	if s.checkRun(auth) != nil {
		err = chatFailureError(chatAccessRevoked, "Notebook access or session was revoked.")
	}
	s.chatTerminal(auth, id, runID, state, err, fit)
}

// chatAccessInterval bounds how long a run keeps working after its session,
// notebook access, personal consent or a data grant it cites is revoked.
// Checking on this interval instead of per streamed token keeps the run's
// authorization queries proportional to seconds, not tokens.
const chatAccessInterval = time.Second

// chatAccessPoll is how often webcore.MonitorAccess calls its authorize function.
const chatAccessPoll = 250 * time.Millisecond

// chatRunAccess holds the run state its access monitor rechecks: the consent
// the run is bound to and the evidence it has gained so far.
type chatRunAccess struct {
	mu  sync.Mutex
	run chat.Run
}

func (a *chatRunAccess) set(run chat.Run) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.run = run
}
func (a *chatRunAccess) get() chat.Run {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.run
}

// monitorChatAccess cancels ctx with the failure once the run may no longer
// use its session, notebooks, consent or cited evidence. ctx must carry the
// run's scope, which bounds the data grants a chat may use.
func (s *Server) monitorChatAccess(ctx context.Context, auth chat.RunAuth, access *chatRunAccess) (context.Context, func()) {
	var checked time.Time
	return webcore.MonitorAccess(ctx, func(ctx context.Context) error {
		// Check on the first poll within one poll of the interval.
		if !checked.IsZero() && time.Since(checked) < chatAccessInterval-chatAccessPoll {
			return nil
		}
		checked = time.Now()
		if err := s.checkRun(auth); err != nil {
			return err
		}
		return s.checkChatSourceAccess(ctx, access.get())
	})
}

// chatAccessFailure reports the revocation that made the access monitor stop
// ctx, or nil when ctx is live or was cancelled or timed out.
func chatAccessFailure(ctx context.Context) error {
	if ctx.Err() == nil {
		return nil
	}
	cause := context.Cause(ctx)
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return nil
	}
	return cause
}

// watchChatCancel stops a run when a cancellation is requested on another
// gateway, as a local request cancels it directly. It reads the transcript only
// when the conversation's version changes.
func (s *Server) watchChatCancel(ctx context.Context, cancel context.CancelFunc, auth chat.RunAuth, id, runID string) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	seen := s.chat.Store.Generation(auth.User, id)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		generation := s.chat.Store.Generation(auth.User, id)
		if generation == 0 || generation == seen {
			continue
		}
		seen = generation
		if c, err := s.chat.Store.Read(auth.User, auth.Project, id); err == nil {
			if r := c.Run(runID); r != nil && r.CancelRequested {
				cancel()
				return
			}
		}
	}
}

// executeChat runs an admitted chat run until it is terminal or waits for a
// confirmation. Each round loads the run, fits one model request, streams it,
// persists the answer and then executes the tool calls it made.
func (s *Server) executeChat(ctx context.Context, cancel context.CancelFunc, auth chat.RunAuth, id, runID string, connection chat.Connection) {
	defer func() {
		cancel()
		s.chat.Finish(runID, connection.ID)
	}()
	// Every return below, including one after a failed read of the
	// conversation, leaves the run terminal or deliberately awaiting confirmation.
	defer s.recoverChatRun(auth, id, runID)
	go s.watchChatCancel(ctx, cancel, auth, id, runID)
	runner := &chatRunner{s: s, auth: auth, id: id, runID: runID, connection: connection}
	ctx, stop, run, err := runner.prepare(ctx)
	defer stop()
	if err == nil {
		err = runner.rounds(ctx, run)
	}
	runner.settle(ctx, err)
}

var errChatConfirmation = errors.New("action needs explicit confirmation")

// executeChatTool executes one of the model's tool calls. Passage search and
// action discovery run within the run; research, notebook actions and a
// review's workflow checkpoint have their own executors.
func (s *Server) executeChatTool(ctx context.Context, a chat.RunAuth, id, runID string, call chat.ToolCall, connection chat.Connection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.checkRun(a); err != nil {
		return err
	}
	c, err := s.chat.Store.Read(a.User, a.Project, id)
	if err != nil {
		return err
	}
	run := c.Run(runID)
	if run == nil {
		return chat.ErrNotFound
	}
	ctx, err = chat.WithRunScope(ctx, c, *run)
	if err != nil {
		return err
	}
	if run.CancelRequested {
		return context.Canceled
	}
	if !chatToolReplayed(*run, call) {
		if call, err = prepareChatToolCall(s.runTools(*run, connection), connection, call, run.Messages); err != nil {
			return err
		}
	}
	if call.Name == "complete_workflow" && run.Options.Review {
		var handled bool
		if run, handled, err = s.gateReviewWorkflow(ctx, a, id, run, call, connection); handled {
			return err
		}
	}
	if run.ToolCount >= max(run.ToolLimit, connection.Limits.MaxTools) {
		if ordinaryChatAsk(*run) {
			return s.omitChatBudgetRead(ctx, a, id, *run, call, connection)
		}
		return chatFailureError(chatToolBudget, "Tool-call budget reached")
	}
	var result json.RawMessage
	var citations []chat.Citation
	switch call.Name {
	case "research_search", "research_fetch":
		return s.executeResearchTool(ctx, a, id, runID, call, connection)
	case "search_passages":
		result, citations, err = s.searchChatPassages(ctx, a, id, *run, call, connection)
	case "discover_actions":
		result, err = s.discoverChatActions(a, id, *run, call)
	default:
		return s.executeNotebookTool(ctx, a, id, runID, call, connection)
	}
	if err != nil {
		return err
	}
	if err = s.checkRun(a); err != nil {
		return err
	}
	return s.chatRunUpdate(a, id, runID, func(c *chat.Conversation, r *chat.Run) error {
		r.ToolCount++
		recordChatToolResult(c, r, call.ID, chatToolResultContext(call.Name, result, citations), citations, true)
		op := chat.Operation{ID: operationID(runID, r.Rounds, call.ID), ToolCallID: call.ID, Round: r.Rounds, Name: call.Name, Arguments: call.Arguments, Hash: chat.Hash(call.Arguments), State: "completed", Result: result}
		r.Operations = append(r.Operations, op)
		c.AddEvent(runID, "tool_result", op)
		return nil
	})
}

// chatToolReplayed reports whether call replays an operation this round
// already recorded. A replay uses its already validated durable arguments.
// Refreshed model projections deliberately contain only receipt identity,
// never stale content.
func chatToolReplayed(run chat.Run, call chat.ToolCall) bool {
	for _, operation := range run.Operations {
		if operation.ID == operationID(run.ID, run.Rounds, call.ID) && operation.Name == call.Name {
			return true
		}
	}
	return false
}

// gateReviewWorkflow lets a review advance its workflow checkpoint only from a
// complete inventory at an unchanged snapshot, with arguments matching it. It
// reports handled when it executed, deferred or refused the call itself;
// otherwise it returns the run as it now stands.
func (s *Server) gateReviewWorkflow(ctx context.Context, a chat.RunAuth, id string, run *chat.Run, call chat.ToolCall, connection chat.Connection) (*chat.Run, bool, error) {
	reconcile, err := s.reconcileReviewWorkflow(ctx, a, run, call)
	if err != nil {
		return run, true, err
	}
	if reconcile {
		return run, true, s.executeNotebookTool(ctx, a, id, run.ID, call, connection)
	}
	if run.Review == nil || !run.Review.Complete || !reviewComplete(run.Review) {
		if reviewWorkflowWasDeferred(run) {
			return run, true, s.deferReviewWorkflow(a, id, run, call)
		}
		return run, true, errors.New("incomplete review cannot advance a workflow checkpoint")
	}
	complete, err := s.prepareChatReview(ctx, a, id, run.ID, connection)
	if err != nil {
		return run, true, err
	}
	if !complete {
		return run, true, s.deferReviewWorkflow(a, id, run, call)
	}
	if err = s.retainPendingWorkflowPair(a, id, run, call); err != nil {
		return run, true, err
	}
	refreshed, err := s.chat.Store.Read(a.User, a.Project, id)
	if err != nil {
		return run, true, err
	}
	run = refreshed.Run(run.ID)
	store, err := s.catalog.Access(a.User, a.Project, "read")
	if err != nil {
		return run, true, err
	}
	snapshot, err := chatCorpusSnapshot(ctx, store, *run, "")
	if err != nil {
		return run, true, err
	}
	if snapshot.Revision != run.Review.Revision || snapshot.Visibility != run.Review.Visibility {
		return run, true, errors.New("review snapshot changed; re-examine before completing the workflow")
	}
	if err = validateReviewWorkflowArguments(run, call.Arguments); err != nil {
		return run, true, err
	}
	return run, false, nil
}

// searchChatPassages retrieves passages for the model's query and cites them,
// pinned to their committed references. A failed retrieval is the call's
// result, not the run's failure.
func (s *Server) searchChatPassages(ctx context.Context, a chat.RunAuth, id string, run chat.Run, call chat.ToolCall, connection chat.Connection) (json.RawMessage, []chat.Citation, error) {
	var in struct {
		Text  string `json:"text"`
		Scope string `json:"scope"`
	}
	if err := strictChatArguments(call.Arguments, &in); err != nil || strings.TrimSpace(in.Text) == "" || (in.Scope != "" && in.Scope != "current" && in.Scope != "history") {
		return nil, nil, errors.New("invalid passage search arguments")
	}
	store, err := s.catalog.Access(a.User, a.Project, "read")
	if err != nil {
		return nil, nil, err
	}
	e, releaseEngine, err := s.acquireEngine(ctx, store)
	if err != nil {
		return nil, nil, err
	}
	defer releaseEngine()
	cfg := s.chat.Snapshot().Config
	found, err := s.chatRetrieve(ctx, id, run, e, knowledge.RetrievalQuery{PerNotePassages: connection.Limits.PerNotePassages, Text: in.Text, Scope: in.Scope, Limit: connection.Limits.MaxPassages, TokenBudget: connection.Limits.EvidenceTokens, LocalOnly: cfg.LocalOnly && !cfg.EmbeddingsLocal}, connection, false)
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": err.Error()})
		return result, nil, nil
	}
	result, _ := json.Marshal(found)
	citations := chatRetrievalCitations(a.Project, operationID(run.ID, run.Rounds, call.ID), found)
	if err := pinChatCitationReferences(ctx, store, citations); err != nil {
		return nil, nil, err
	}
	return result, citations, nil
}

// discoverChatActions returns the schemas of up to three named actions the
// run's mode allows. Discovery cannot grant action authority; the run keeps
// offering the discovered tools, up to twelve.
func (s *Server) discoverChatActions(a chat.RunAuth, id string, run chat.Run, call chat.ToolCall) (json.RawMessage, error) {
	var in struct {
		Names []string `json:"names"`
	}
	if strictChatArguments(call.Arguments, &in) != nil || len(in.Names) > 3 || len(in.Names) == 0 {
		return nil, errors.New("discovery arguments must name one to three actions")
	}
	available := []chat.Tool{}
	for _, name := range in.Names {
		p := s.actions[name].Policy
		if !p.ChatAvailable() || p.Control && run.Options.Mode != "edit" || run.Options.Mode == "ask" && !p.Read {
			continue
		}
		available = append(available, s.chatActionTools(name)...)
	}
	result, _ := json.Marshal(available)
	return result, s.chatRunUpdate(a, id, run.ID, func(c *chat.Conversation, r *chat.Run) error {
		for _, tool := range available {
			if !slices.Contains(r.ToolNames, tool.Name) && len(r.ToolNames) < 12 {
				r.ToolNames = append(r.ToolNames, tool.Name)
			}
		}
		return nil
	})
}
func pendingToolCalls(messages []chat.Message) []chat.ToolCall {
	pending := []chat.ToolCall{}
	for _, m := range messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			pending = append([]chat.ToolCall(nil), m.ToolCalls...)
		}
		if m.Role == "tool" {
			for i, c := range pending {
				if c.ID == m.ToolCallID {
					pending = append(pending[:i], pending[i+1:]...)
					break
				}
			}
		}
	}
	return pending
}
func (s *Server) executeNotebookTool(ctx context.Context, a chat.RunAuth, id, runID string, call chat.ToolCall, connection chat.Connection) error {
	c, err := s.chat.Store.Read(a.User, a.Project, id)
	if err != nil {
		return err
	}
	run := c.Run(runID)
	if run == nil {
		return chat.ErrNotFound
	}
	ctx, err = chat.WithRunScope(ctx, c, *run)
	if err != nil {
		return err
	}
	key := operationID(runID, run.Rounds, call.ID)
	var prior *chat.Operation
	for i := range run.Operations {
		if run.Operations[i].ID == key {
			prior = &run.Operations[i]
			break
		}
	}
	name := call.Name
	var canonical json.RawMessage
	approved := false
	if prior != nil {
		name = prior.Name
		canonical = prior.Arguments
		approved = prior.Approved
		if prior.State == "awaiting_confirmation" {
			return errChatConfirmation
		}
		if !allowedAction(run.Options.Mode, name, canonical, run.Options.Actions, approved) {
			return workspace.ErrForbidden
		}
		if approved {
			ctx = chat.WithApproval(ctx)
		}
		if prior.State == "completed" {
			return s.chatRunUpdate(a, id, runID, func(c *chat.Conversation, r *chat.Run) error {
				r.Provenance.Complete = r.Provenance.Complete && prior.ProvenanceComplete
				recordChatToolResult(c, r, call.ID, chatToolResultContext(name, prior.Result, prior.Citations), prior.Citations, true)
				return nil
			})
		}
		if !policy(name).Replay {
			_ = s.chatRunUpdate(a, id, runID, func(c *chat.Conversation, r *chat.Run) error {
				r.State = "uncertain"
				r.Error = errChatUncertain.Error()
				r.Usage.Reserved = 0
				c.AddEvent(runID, "state", map[string]string{"state": r.State, "message": r.Error})
				return nil
			})
			return errChatUncertain
		}
	} else {
		p := policy(name)
		if p.Control || chatDeletionControl(name, call.Arguments) {
			_, canonical, e := s.actionArguments(a.Project, name, call.Arguments)
			if e != nil {
				return e
			}
			return s.prepareChatOperation(ctx, a, id, runID, call, canonical)
		}
		if !allowedAction(run.Options.Mode, name, call.Arguments, run.Options.Actions, false) {
			return workspace.ErrForbidden
		}
		_, canonical, err = s.actionArguments(a.Project, name, call.Arguments)
		if err != nil {
			if !p.Read {
				return err
			}
			canonical = call.Arguments
		}
		var obj map[string]json.RawMessage
		json.Unmarshal(canonical, &obj)
		// The shared dispatch boundary checks read arguments and scope before any
		// access. Record a denied read as a tool result so other reads can continue.
		// Mutations still require preflight before a durable operation is prepared;
		// a change carrying personal context is preflighted as the approved
		// operation it must become.
		personal := !p.Read && s.chatPersonalApproval(a.User, a.Project, chat.RunScopeFrom(ctx).Personal)
		if !p.Read {
			preflight := ctx
			if personal {
				preflight = chat.WithApproval(ctx)
			}
			if _, err = s.guardChatAction(preflight, a.Project, name, canonical); err != nil {
				return err
			}
		}
		if name == "save_note" {
			var in noteInput
			if json.Unmarshal(canonical, &in) != nil {
				return errors.New("invalid note edit")
			}
			store, err := s.catalog.Access(a.User, a.Project, "edit")
			if err != nil {
				return err
			}
			hash := ""
			if in.Revision != "" {
				before, err := store.Read(ctx, in.Path, in.Revision)
				if err != nil {
					return err
				}
				hash = workspace.ContentHash(before.Content)
			}
			name = "submit_change_group"
			group := workspace.EditGroup{ID: key, Summary: "User-directed chat note edit", Edits: []workspace.NoteEdit{{Path: in.Path, Content: in.Content, Revision: in.Revision, Hash: hash}}}
			canonical, _ = json.Marshal(map[string]any{"project": a.Project, "mode": "apply", "group": group})
		} else {
			if name == "submit_change_group" {
				var group workspace.EditGroup
				if json.Unmarshal(obj["group"], &group) != nil {
					return errors.New("invalid change group")
				}
				group.ID = key
				obj["group"], _ = json.Marshal(group)
			}
			if name == "patch_live_note" {
				obj["operationId"], _ = json.Marshal(key)
			}
			if name == "file_review_finding" {
				var finding map[string]json.RawMessage
				if json.Unmarshal(obj["finding"], &finding) != nil {
					return errors.New("invalid finding")
				}
				finding["key"], _ = json.Marshal(key)
				obj["finding"], _ = json.Marshal(finding)
			}
			canonical, _ = json.Marshal(obj)
		}
		if personal {
			// Other readers see personal context only in a change the person approves.
			return s.prepareChatOperation(ctx, a, id, runID, chat.ToolCall{ID: call.ID, Name: name}, canonical)
		}
		op := chat.Operation{ID: key, ToolCallID: call.ID, Round: run.Rounds, Name: name, Arguments: canonical, Hash: chat.Hash(canonical), State: "running"}
		if err = s.chatRunUpdate(a, id, runID, func(c *chat.Conversation, r *chat.Run) error {
			r.ToolCount++
			r.Operations = append(r.Operations, op)
			c.AddEvent(runID, "tool_started", map[string]string{"id": key, "name": name})
			return nil
		}); err != nil {
			return err
		}
	}
	if err = s.checkRun(a); err != nil {
		return err
	}
	result, actionErr := s.dispatchActionOutcome(ctx, a.Project, name, canonical)
	outcome := classifyChatActionOutcome(result, actionErr)
	if outcome.ambiguous {
		if policy(name).Replay {
			return errChatPending
		}
		return errChatUncertain
	}
	result = outcome.result
	evidence, tracked := s.chatToolEvidence(ctx, a, key, name, result)
	authorizationErr := s.checkRun(a)
	err = s.chatRunUpdate(a, id, runID, func(c *chat.Conversation, r *chat.Run) error {
		if actionErr == nil {
			r.Provenance.Complete = r.Provenance.Complete && tracked
		}
		recordChatToolResult(c, r, call.ID, chatToolResultContext(name, result, evidence), evidence, authorizationErr == nil)
		return completeChatOperation(c, r, key, outcome, evidence, tracked)
	})
	if err != nil {
		return err
	}
	if authorizationErr != nil {
		return authorizationErr
	}
	return s.checkRun(a)
}

var errChatPending = errors.New("the original action acknowledgement is pending; continue explicitly to reconcile its durable receipt")
