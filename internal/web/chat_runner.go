package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"forgetmenote/internal/attribution"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/chatrun"
	"forgetmenote/internal/knowledge"
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
	attachments, err := s.chatrun.Attachments(r.Context(), projectStore(r.Context()), in.Attachments)
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
	defer s.chatrun.RecoverRun(auth, id, runID)
	go s.watchChatCancel(ctx, cancel, auth, id, runID)
	runner := s.chatrun.NewRunner(auth, id, runID, connection)
	ctx, stop, run, err := runner.Prepare(ctx)
	defer stop()
	if err == nil {
		err = runner.Rounds(ctx, run)
	}
	runner.Settle(ctx, err)
}

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
		if call, err = chatrun.PrepareToolCall(s.chatrun.RunTools(*run, connection), connection, call, run.Messages); err != nil {
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
		if chatrun.OrdinaryAsk(*run) {
			return s.chatrun.OmitBudgetRead(ctx, a, id, *run, call, connection)
		}
		return chatrun.FailureError(chatrun.ToolBudget, "Tool-call budget reached")
	}
	var result json.RawMessage
	var citations []chat.Citation
	switch call.Name {
	case "research_search", "research_fetch":
		return s.chatrun.ExecuteResearchTool(ctx, a, id, runID, call, connection)
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
	return s.chatrun.RunUpdate(a, id, runID, func(c *chat.Conversation, r *chat.Run) error {
		r.ToolCount++
		chatrun.RecordToolResult(c, r, call.ID, chatrun.ToolResultContext(call.Name, result, citations), citations, true)
		op := chat.Operation{ID: chatrun.OperationID(runID, r.Rounds, call.ID), ToolCallID: call.ID, Round: r.Rounds, Name: call.Name, Arguments: call.Arguments, Hash: chat.Hash(call.Arguments), State: "completed", Result: result}
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
		if operation.ID == chatrun.OperationID(run.ID, run.Rounds, call.ID) && operation.Name == call.Name {
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
	reconcile, err := s.chatrun.ReconcileReviewWorkflow(ctx, a, run, call)
	if err != nil {
		return run, true, err
	}
	if reconcile {
		return run, true, s.executeNotebookTool(ctx, a, id, run.ID, call, connection)
	}
	if run.Review == nil || !run.Review.Complete || !chatrun.ReviewComplete(run.Review) {
		if chatrun.ReviewWorkflowWasDeferred(run) {
			return run, true, s.chatrun.DeferReviewWorkflow(a, id, run, call)
		}
		return run, true, errors.New("incomplete review cannot advance a workflow checkpoint")
	}
	complete, err := s.chatrun.PrepareReview(ctx, a, id, run.ID, connection)
	if err != nil {
		return run, true, err
	}
	if !complete {
		return run, true, s.chatrun.DeferReviewWorkflow(a, id, run, call)
	}
	if err = s.chatrun.RetainPendingWorkflowPair(a, id, run, call); err != nil {
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
	snapshot, err := chatrun.CorpusSnapshot(ctx, store, *run, "")
	if err != nil {
		return run, true, err
	}
	if snapshot.Revision != run.Review.Revision || snapshot.Visibility != run.Review.Visibility {
		return run, true, errors.New("review snapshot changed; re-examine before completing the workflow")
	}
	if err = chatrun.ValidateReviewWorkflowArguments(run, call.Arguments); err != nil {
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
	if err := chatrun.StrictArguments(call.Arguments, &in); err != nil || strings.TrimSpace(in.Text) == "" || (in.Scope != "" && in.Scope != "current" && in.Scope != "history") {
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
	found, err := s.chatrun.Retrieve(ctx, id, run, e, knowledge.RetrievalQuery{PerNotePassages: connection.Limits.PerNotePassages, Text: in.Text, Scope: in.Scope, Limit: connection.Limits.MaxPassages, TokenBudget: connection.Limits.EvidenceTokens, LocalOnly: cfg.LocalOnly && !cfg.EmbeddingsLocal}, connection, false)
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": err.Error()})
		return result, nil, nil
	}
	result, _ := json.Marshal(found)
	citations := chatrun.RetrievalCitations(a.Project, chatrun.OperationID(run.ID, run.Rounds, call.ID), found)
	if err := chatrun.PinCitationReferences(ctx, store, citations); err != nil {
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
	if chatrun.StrictArguments(call.Arguments, &in) != nil || len(in.Names) > 3 || len(in.Names) == 0 {
		return nil, errors.New("discovery arguments must name one to three actions")
	}
	available := []chat.Tool{}
	for _, name := range in.Names {
		p := s.actions[name].Policy
		if !p.ChatAvailable() || p.Control && run.Options.Mode != "edit" || run.Options.Mode == "ask" && !p.Read {
			continue
		}
		available = append(available, s.chatrun.ActionTools(name)...)
	}
	result, _ := json.Marshal(available)
	return result, s.chatrun.RunUpdate(a, id, run.ID, func(c *chat.Conversation, r *chat.Run) error {
		for _, tool := range available {
			if !slices.Contains(r.ToolNames, tool.Name) && len(r.ToolNames) < 12 {
				r.ToolNames = append(r.ToolNames, tool.Name)
			}
		}
		return nil
	})
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
	key := chatrun.OperationID(runID, run.Rounds, call.ID)
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
			return chatrun.ErrConfirmation
		}
		if !allowedAction(run.Options.Mode, name, canonical, run.Options.Actions, approved) {
			return workspace.ErrForbidden
		}
		if approved {
			ctx = chat.WithApproval(ctx)
		}
		if prior.State == "completed" {
			return s.chatrun.RunUpdate(a, id, runID, func(c *chat.Conversation, r *chat.Run) error {
				r.Provenance.Complete = r.Provenance.Complete && prior.ProvenanceComplete
				chatrun.RecordToolResult(c, r, call.ID, chatrun.ToolResultContext(name, prior.Result, prior.Citations), prior.Citations, true)
				return nil
			})
		}
		if !policy(name).Replay {
			_ = s.chatrun.RunUpdate(a, id, runID, func(c *chat.Conversation, r *chat.Run) error {
				r.State = "uncertain"
				r.Error = chatrun.ErrUncertain.Error()
				r.Usage.Reserved = 0
				c.AddEvent(runID, "state", map[string]string{"state": r.State, "message": r.Error})
				return nil
			})
			return chatrun.ErrUncertain
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
		if err = s.chatrun.RunUpdate(a, id, runID, func(c *chat.Conversation, r *chat.Run) error {
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
			return chatrun.ErrPending
		}
		return chatrun.ErrUncertain
	}
	result = outcome.result
	evidence, tracked := s.chatrun.ToolEvidence(ctx, a, key, name, result)
	authorizationErr := s.checkRun(a)
	err = s.chatrun.RunUpdate(a, id, runID, func(c *chat.Conversation, r *chat.Run) error {
		if actionErr == nil {
			r.Provenance.Complete = r.Provenance.Complete && tracked
		}
		chatrun.RecordToolResult(c, r, call.ID, chatrun.ToolResultContext(name, result, evidence), evidence, authorizationErr == nil)
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
