package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"forgetmenote/internal/chat"
	"forgetmenote/internal/chatrun"
	"forgetmenote/internal/webcore"
	"forgetmenote/internal/workspace"
)

func (s *Server) chatCancel(w http.ResponseWriter, r *http.Request) {
	c, err := s.chatAuthorized(r)
	if err != nil {
		chatFail(w, err)
		return
	}
	id := r.PathValue("run")
	run := c.Run(id)
	if run == nil {
		chatFail(w, chat.ErrNotFound)
		return
	}
	auth := s.chatRunAuth(r, c)
	if err = s.chatrun.RunUpdate(auth, c.ID, id, func(c *chat.Conversation, r *chat.Run) error {
		r.CancelRequested = true
		if r.State == "awaiting_confirmation" {
			r.State = "cancelled"
			r.Usage.Reserved = 0
			for i := range r.Operations {
				if r.Operations[i].State == "awaiting_confirmation" {
					r.Operations[i].State = "declined"
					r.Operations[i].Confirmation = ""
				}
			}
		}
		c.AddEvent(id, "state", map[string]any{"state": r.State, "cancelRequested": true})
		return nil
	}); err != nil {
		chatFail(w, err)
		return
	}
	s.chat.Cancel(id)
	jsonResponse(w, map[string]any{"run": id, "cancelRequested": true})
}
func (s *Server) chatResume(w http.ResponseWriter, r *http.Request) {
	c, err := s.chatAuthorized(r)
	if err != nil {
		chatFail(w, err)
		return
	}
	run := c.Run(r.PathValue("run"))
	if run == nil {
		chatFail(w, chat.ErrNotFound)
		return
	}
	if err := run.Options.Corpus.Validate(); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	var in struct {
		BudgetTokens int `json:"budgetTokens"` // Accepted from legacy clients; ignored.
	}
	if !decode(w, r, &in) {
		return
	}
	cfg := s.chat.Snapshot().Config
	connection, err := cfg.Resolve(run.Options.Connection, run.Options.Model)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	auth := s.chatRunAuth(r, c)
	base, err := s.runContext(auth)
	if err != nil {
		chatFail(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(base, time.Duration(connection.Limits.RunSeconds)*time.Second)
	_, err = s.chat.Admit(cfg, connection, run.ID, cancel, c.Owner, s.pausedChatAllowed, func() (bool, error) {
		_, err := s.chat.Store.Resume(c.Owner, c.Project, c.ID, run.ID, connection.Limits.MaxRounds, connection.Limits.MaxTools, auth.Epoch)
		return false, err
	})
	if err != nil {
		cancel()
		if errors.Is(err, chat.ErrCapacity) || errors.Is(err, chat.ErrRunActive) {
			http.Error(w, "Chat is busy; retry shortly", 429)
		} else {
			chatFail(w, err)
		}
		return
	}
	go s.executeChat(ctx, cancel, auth, c.ID, run.ID, connection)
	jsonResponse(w, map[string]any{"run": run.ID, "state": "queued"})
}
func (s *Server) chatSaveNote(w http.ResponseWriter, r *http.Request) {
	c, err := s.chatAuthorized(r)
	if err != nil {
		chatFail(w, err)
		return
	}
	store, err := s.catalog.Access(c.Owner, c.Project, "edit")
	if err != nil {
		chatFail(w, err)
		return
	}
	var in struct {
		SubmissionID string   `json:"submissionId"`
		Path         string   `json:"path"`
		Revision     string   `json:"revision"`
		TurnIDs      []string `json:"turnIds"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.SubmissionID) < 1 || len(in.SubmissionID) > 128 || len(in.TurnIDs) < 1 || len(in.TurnIDs) > 100 {
		http.Error(w, "Choose 1–100 messages and a submission ID", 400)
		return
	}
	selected := map[string]bool{}
	for _, id := range in.TurnIDs {
		selected[id] = true
	}
	var content strings.Builder
	content.WriteString("# " + c.Title + "\n\n")
	citations := map[string]bool{}
	for _, turn := range chatTranscript(c) {
		if !selected[turn.ID] {
			continue
		}
		if turn.Streaming {
			http.Error(w, "Stop generating before saving an unfinished answer", 409)
			return
		}
		delete(selected, turn.ID)
		label := "Assistant"
		if turn.Role == "user" {
			label = "User"
		}
		content.WriteString("## " + label + "\n\n" + turn.Content + "\n\n")
		for _, source := range turn.Citations {
			if !citations[source.URL] {
				content.WriteString("[" + source.ID + "](" + source.URL + ")\n\n")
				citations[source.URL] = true
			}
		}
	}
	if len(selected) > 0 {
		http.Error(w, "Selected message is unavailable", 400)
		return
	}
	if content.Len() > 1<<20 {
		http.Error(w, "Selected transcript exceeds 1 MiB", 413)
		return
	}
	hash := ""
	if in.Revision != "" {
		old, e := store.Read(r.Context(), in.Path, in.Revision)
		if e != nil {
			chatFail(w, e)
			return
		}
		hash = workspace.ContentHash(old.Content)
	}
	auth := s.chatRunAuth(r, c)
	ctx, err := s.runContext(auth)
	if err != nil {
		chatFail(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	group := workspace.EditGroup{ID: "chat-save-" + c.ID + "-" + chat.Hash([]byte(in.SubmissionID))[:24], Summary: "Save selected chat messages to note", Edits: []workspace.NoteEdit{{Path: in.Path, Content: content.String(), Revision: in.Revision, Hash: hash}}}
	contentFence, release := s.chat.RetainContent(c.Owner, c.ID)
	defer release()
	contentFence.RLock()
	defer contentFence.RUnlock()
	if _, err = s.chatAuthorized(r); err != nil {
		chatFail(w, err)
		return
	}
	// The store was opened before the body was read; recheck Edit inside the
	// fence so a demotion during the request cannot be outrun.
	if store, err = s.catalog.Access(c.Owner, c.Project, "edit"); err != nil {
		chatFail(w, err)
		return
	}
	result, err := store.ApplyGroup(ctx, group)
	if err != nil {
		chatFail(w, err)
		return
	}
	if err = s.checkRun(auth); err != nil {
		chatFail(w, err)
		return
	}
	err = s.chat.Store.Update(c.Owner, c.Project, c.ID, func(c *chat.Conversation) error {
		c.AddEvent("", "saved_note", map[string]any{"path": in.Path, "revision": result.Revision, "replayed": result.Replayed})
		return nil
	})
	if err != nil {
		chatFail(w, err)
		return
	}
	jsonResponse(w, map[string]any{"path": in.Path, "revision": result.Revision, "replayed": result.Replayed})
}

func (s *Server) prepareChatOperation(ctx context.Context, a chat.RunAuth, id, runID string, call chat.ToolCall, canonical json.RawMessage) error {
	c, err := s.chat.Store.Read(a.User, a.Project, id)
	if err != nil {
		return err
	}
	run := c.Run(runID)
	// A research fetch of an unprovided URL, or a change carrying personal
	// context, needs the same explicit approval in every mode that allows it;
	// notebook controls remain Edit-only.
	if run.Options.Mode != "edit" && call.Name != "research_fetch" && !allowedAction(run.Options.Mode, call.Name, canonical, run.Options.Actions, false) {
		return workspace.ErrForbidden
	}
	if call.Name != "research_fetch" {
		// Do not offer an approval that dispatch would refuse.
		if err = chatReadOnly(a.Project, *run); err != nil {
			return err
		}
	}
	var preview any
	if call.Name == "submit_change_group" {
		var obj map[string]json.RawMessage
		if json.Unmarshal(canonical, &obj) != nil {
			return errors.New("invalid change group")
		}
		var group workspace.EditGroup
		if json.Unmarshal(obj["group"], &group) != nil {
			return errors.New("invalid change group")
		}
		group.ID = chatrun.OperationID(run.ID, run.Rounds, call.ID)
		obj["group"], _ = json.Marshal(group)
		canonical, _ = json.Marshal(obj)
	}
	if call.Name == "empty_trash" {
		store, err := s.catalog.Access(a.User, a.Project, "owner")
		if err != nil {
			return err
		}
		listing, err := store.ListTrash(ctx)
		if err != nil {
			return err
		}
		canonical, _ = json.Marshal(map[string]any{"project": a.Project, "revision": listing.Revision, "confirmation": listing.Confirmation})
		preview = listing
	}
	op := chat.Operation{ID: chatrun.OperationID(run.ID, run.Rounds, call.ID), ToolCallID: call.ID, Round: run.Rounds, Name: call.Name, Arguments: canonical, Hash: chat.Hash(canonical), State: "awaiting_confirmation", Confirmation: chat.NewID()}
	if preview != nil {
		op.Result, _ = json.Marshal(preview)
	}
	if err = s.chatrun.RunUpdate(a, id, runID, func(c *chat.Conversation, r *chat.Run) error {
		r.Operations = append(r.Operations, op)
		r.ToolCount++
		r.State = "awaiting_confirmation"
		c.AddEvent(runID, "approval_required", op)
		c.AddEvent(runID, "state", map[string]string{"state": "awaiting_confirmation"})
		return nil
	}); err != nil {
		return err
	}
	return chatrun.ErrConfirmation
}
func (s *Server) chatAction(w http.ResponseWriter, r *http.Request) {
	c, err := s.chatAuthorized(r)
	if err != nil {
		chatFail(w, err)
		return
	}
	var in struct {
		Decision     string `json:"decision"`
		Confirmation string `json:"confirmation"`
	}
	if !decode(w, r, &in) {
		return
	}
	var run *chat.Run
	var op *chat.Operation
	for i := range c.Runs {
		for j := range c.Runs[i].Operations {
			if c.Runs[i].Operations[j].ID == r.PathValue("action") {
				run = &c.Runs[i]
				op = &run.Operations[j]
			}
		}
	}
	if run == nil || op == nil {
		chatFail(w, chat.ErrNotFound)
		return
	}
	if err := run.Options.Corpus.Validate(); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	if op.State != "awaiting_confirmation" || op.Confirmation == "" || op.Confirmation != in.Confirmation || in.Decision != "approve" && in.Decision != "decline" {
		chatFail(w, chat.ErrConflict)
		return
	}
	// A fresh login cannot approve a confirmation created by a revoked run.
	// Reconciliation is durable and releases only unused reservations.
	err = s.reconcilePausedChats(c.Owner)
	if err != nil {
		chatFail(w, err)
		return
	}
	auth := s.chatRunAuth(r, c)
	if auth.Epoch != run.Epoch {
		chatFail(w, workspace.ErrForbidden)
		return
	}
	base, err := s.runContext(auth)
	if err != nil {
		chatFail(w, err)
		return
	}
	if op.Name != "research_fetch" && !allowedAction(run.Options.Mode, op.Name, op.Arguments, run.Options.Actions, true) {
		chatFail(w, workspace.ErrForbidden)
		return
	}
	cfg := s.chat.Snapshot().Config
	connection, resolveErr := cfg.Resolve(run.Options.Connection, run.Options.Model)
	if resolveErr != nil {
		http.Error(w, resolveErr.Error(), 400)
		return
	}
	ctx, cancel := context.WithTimeout(base, time.Duration(min(30, connection.Limits.RunSeconds))*time.Second)
	if _, err := s.chat.Admit(cfg, connection, run.ID, cancel, "", nil, nil); err != nil {
		cancel()
		if errors.Is(err, chat.ErrRunActive) {
			http.Error(w, "Run is still pausing; retry shortly", 409)
		} else {
			http.Error(w, "Chat is busy; retry shortly", 429)
		}
		return
	}
	ctx, stopAccess := webcore.MonitorAccess(ctx, func(context.Context) error { return s.checkRun(auth) })
	defer func() {
		cancel()
		stopAccess()
		s.chat.Finish(run.ID, connection.ID)
	}()
	operationID := op.ID
	declined, _ := json.Marshal(map[string]string{"message": "The user declined this action."})
	err = s.chatrun.RunUpdate(auth, c.ID, run.ID, func(c *chat.Conversation, r *chat.Run) error {
		if r.Epoch != auth.Epoch || r.State != "awaiting_confirmation" || s.checkRun(auth) != nil {
			return workspace.ErrForbidden
		}
		for i := range r.Operations {
			o := &r.Operations[i]
			if o.ID != operationID {
				continue
			}
			if o.State != "awaiting_confirmation" || o.Confirmation != in.Confirmation {
				return chat.ErrConflict
			}
			o.Confirmation = ""
			o.Approved = in.Decision == "approve"
			o.State = "running"
			r.State = "running"
			r.Holder = s.db.Holder
			if !o.Approved {
				o.State = "completed"
				o.Result = declined
				chatrun.RecordToolResult(c, r, o.ToolCallID, string(declined), nil, true)
				r.State = "interrupted"
				r.Error = "Action declined. Continue explicitly to finish the response."
				r.Usage.Reserved = 0
				c.AddEvent(r.ID, "tool_result", *o)
				c.AddEvent(r.ID, "state", map[string]string{"state": r.State, "message": r.Error})
			}
			return nil
		}
		return chat.ErrNotFound
	})
	if err != nil {
		chatFail(w, err)
		return
	}
	if in.Decision == "decline" {
		jsonResponse(w, map[string]any{"run": run.ID, "state": "interrupted", "result": json.RawMessage(declined)})
		return
	}
	if err = s.checkRun(auth); err != nil {
		s.chatrun.Terminal(auth, c.ID, run.ID, "interrupted", chatrun.FailureError(chatrun.AccessRevoked, "Access changed before the approved action started."))
		chatFail(w, err)
		return
	}
	ctx, err = chat.WithRunScope(ctx, c, *run)
	if err != nil {
		chatFail(w, err)
		return
	}
	ctx = chat.WithApproval(ctx)
	if op.Name == "research_fetch" {
		if err = s.chatrun.ExecuteApprovedResearch(ctx, auth, c.ID, run.ID, *op, connection); err != nil {
			s.chatrun.Terminal(auth, c.ID, run.ID, "interrupted", err)
			chatFail(w, err)
			return
		}
		err = s.chatrun.RunUpdate(auth, c.ID, run.ID, func(c *chat.Conversation, r *chat.Run) error {
			r.State = "interrupted"
			r.Error = "Action reviewed. Continue explicitly to finish the response."
			r.Usage.Reserved = 0
			c.AddEvent(r.ID, "state", map[string]string{"state": r.State, "message": r.Error})
			return nil
		})
		if err != nil {
			chatFail(w, err)
			return
		}
		jsonResponse(w, map[string]any{"run": run.ID, "state": "interrupted"})
		return
	}
	result, actionErr := s.dispatchActionOutcome(ctx, c.Project, op.Name, op.Arguments)
	outcome := classifyChatActionOutcome(result, actionErr)
	if outcome.ambiguous {
		state, cause := "uncertain", chatrun.ErrUncertain
		if policy(op.Name).Replay {
			state = "interrupted"
			cause = chatrun.ErrPending
		}
		s.chatrun.Terminal(auth, c.ID, run.ID, state, cause)
		if err = s.checkRun(auth); err != nil {
			chatFail(w, err)
			return
		}
		http.Error(w, cause.Error(), 409)
		return
	}
	result = outcome.result
	authorizationErr := s.checkRun(auth)
	// Persist the known outcome even if access changed while the action ran; read
	// and SSE authorization still prevent disclosure after revocation.
	err = s.chatrun.RunUpdate(auth, c.ID, run.ID, func(c *chat.Conversation, r *chat.Run) error {
		if err := completeChatOperation(c, r, operationID, outcome, nil, false); err != nil {
			return err
		}
		r.Provenance.Complete = false
		chatrun.RecordToolResult(c, r, op.ToolCallID, chatrun.ControlResult(op.Name, result), nil, authorizationErr == nil)
		r.State = "interrupted"
		r.Error = "Action reviewed. Continue explicitly to finish the response."
		r.Usage.Reserved = 0
		c.AddEvent(r.ID, "state", map[string]string{"state": r.State, "message": r.Error})
		return nil
	})
	if err != nil {
		s.chatrun.Terminal(auth, c.ID, run.ID, "uncertain", errors.New("The action outcome could not be recorded; inspect the notebook before retrying."))
		chatFail(w, err)
		return
	}
	if err = s.checkRun(auth); err != nil {
		chatFail(w, err)
		return
	}
	jsonResponse(w, map[string]any{"run": run.ID, "state": "interrupted", "result": result})
}

func chatDeletionControl(name string, args json.RawMessage) bool {
	if name != "submit_change_group" {
		return false
	}
	var in struct {
		Mode  string              `json:"mode"`
		Group workspace.EditGroup `json:"group"`
	}
	if json.Unmarshal(args, &in) != nil || in.Mode != "apply" {
		return false
	}
	for _, edit := range in.Group.Edits {
		if edit.Delete {
			return true
		}
	}
	return false
}
