package chatrun

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"time"

	"forgetmenote/internal/actions"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/webcore"
)

func (s *Service) RunUpdate(a chat.RunAuth, id, runID string, fn func(*chat.Conversation, *chat.Run) error) error {
	return s.hooks.Chat().Store.Update(a.User, a.Project, id, func(c *chat.Conversation) error {
		run := c.Run(runID)
		if run == nil {
			return chat.ErrNotFound
		}
		run.UpdatedAt = chat.Now()
		return fn(c, run)
	})
}

func (s *Service) Terminal(a chat.RunAuth, id, runID, state string, cause error, fits ...ContextFit) {
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
func (s *Service) settleChatRun(a chat.RunAuth, id, runID, state string, cause error, unsettled func(chat.Run) bool, fits ...ContextFit) {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	diagnostic := chat.Run{ID: runID, State: state}
	err := s.RunUpdate(a, id, runID, func(c *chat.Conversation, r *chat.Run) error {
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
	fit := ContextFit{}
	if len(fits) > 0 {
		fit = fits[0]
	}
	category := chatFailureCategory(state, cause)
	if err != nil {
		category = "persistence"
	}
	s.chatDiagnostic("terminal", id, diagnostic, fit, category, err == nil)
}

func (s *Service) failChatRun(ctx context.Context, auth chat.RunAuth, id, runID string, err error, fit ContextFit) {
	state := "failed"
	if errors.Is(err, chatRoundBudget) || errors.Is(err, ToolBudget) || errors.Is(err, chatResearchBudget) {
		state = "interrupted"
	}
	if errors.Is(err, ErrPending) {
		state = "interrupted"
	}
	if errors.Is(err, ErrUncertain) {
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
	c, e := s.hooks.Chat().Store.Read(auth.User, auth.Project, id)
	if e == nil {
		if r := c.Run(runID); r != nil && r.CancelRequested {
			state = "cancelled"
		}
	}
	if s.hooks.CheckRun(auth) != nil {
		err = FailureError(AccessRevoked, "Notebook access or session was revoked.")
	}
	s.Terminal(auth, id, runID, state, err, fit)
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
func (s *Service) monitorChatAccess(ctx context.Context, auth chat.RunAuth, access *chatRunAccess) (context.Context, func()) {
	var checked time.Time
	return webcore.MonitorAccess(ctx, func(ctx context.Context) error {
		// Check on the first poll within one poll of the interval.
		if !checked.IsZero() && time.Since(checked) < chatAccessInterval-chatAccessPoll {
			return nil
		}
		checked = time.Now()
		if err := s.hooks.CheckRun(auth); err != nil {
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

var ErrConfirmation = errors.New("action needs explicit confirmation")

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

var ErrPending = errors.New("the original action acknowledgement is pending; continue explicitly to reconcile its durable receipt")

func OperationID(runID string, round int, callID string) string {
	return runID + "-" + strconv.Itoa(round) + "-" + chat.Hash([]byte(callID))[:16]
}

var ErrUncertain = errors.New("an earlier action has an uncertain outcome; inspect the notebook before retrying")

// This is the model projection of an explicit control, not the private receipt
// or confirmation preview. In particular Empty Trash never supplies source text,
// and a move reports how many notes in the Trash it updated or skipped, never
// which: the Trash is outside every chat corpus.
func ControlResult(name string, result json.RawMessage) string {
	if p := actions.LookupPolicy(name); !p.Control && !p.Edit && !p.Proposal {
		return TextLimit(string(result), 12000)
	}
	var value map[string]json.RawMessage
	_ = json.Unmarshal(result, &value)
	out := map[string]any{"action": name, "completed": true}
	for _, key := range []string{"error", "revision", "count", "removed", "replayed", "id", "status", "trashLinksUpdated", "trashLinksSkipped"} {
		if raw, ok := value[key]; ok {
			var scalar any
			if json.Unmarshal(raw, &scalar) == nil {
				switch scalar.(type) {
				case string, float64, bool:
					out[key] = scalar
				}
			}
		}
	}
	if _, ok := out["error"]; ok {
		out["completed"] = false
	}
	raw, _ := json.Marshal(out)
	return string(raw)
}
