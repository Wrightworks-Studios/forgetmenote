package messaginghttp

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/dataops"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/workspace"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Chat client conversations never run a write directly. A write operation the
// model calls becomes a proposal shown verbatim at the end of the reply. It runs
// at most once, as the conversation's actor with that actor's current grants,
// only when the chat user who asked replies with its code in the same
// conversation before it expires, or presses its Confirm button; Cancel or
// "cancel CODE" drops it. Only this server's text and buttons carry the code.
const writeProposalOperation = "data_write_proposal"
const writeProposalTTL = 30 * time.Minute
const maxWriteProposals = 3

var writeConfirmationPattern = regexp.MustCompile(`(?i)^\s*(?:<@[A-Z0-9]+>\s*)?(confirm|cancel)\s+([A-Z2-7]{8})\s*[.!]?\s*$`)
var writeCode = regexp.MustCompile(`^[A-Z2-7]{8}$`)

type writeProposalState struct {
	Requester string    `json:"requester"`
	Expires   time.Time `json:"expires"`
	Contract  string    `json:"contract"`
	Receipt   string    `json:"receipt,omitempty"`
	Link      string    `json:"link,omitempty"`
}

type confirmedWriteKey struct{}

func dataWriteHash(in dataops.Call) string {
	return automation.Hash([]any{in.Connection, in.Operation, in.Arguments})
}

// ConfirmedDataWrite admits exactly the call its requester confirmed.
func ConfirmedDataWrite(ctx context.Context, in dataops.Call) bool {
	confirmed, ok := ctx.Value(confirmedWriteKey{}).(string)
	return ok && confirmed == dataWriteHash(in)
}

// writeConfirmation returns the code a reply confirms or cancels, and whether
// it cancels.
func writeConfirmation(text string) (string, bool) {
	if m := writeConfirmationPattern.FindStringSubmatch(text); m != nil {
		return strings.ToUpper(m[2]), strings.EqualFold(m[1], "cancel")
	}
	return "", false
}

// ProposesWrites reports whether a run turns write calls into proposals that
// its requester must confirm, so it may be shown the writes it is granted.
func ProposesWrites(spec automation.Spec) bool {
	return spec.Kind == "assistant" && spec.Delivery != nil
}

// ProposeDataWrite reports whether raw names a write operation; if so it is
// recorded for confirmation, never executed.
func (s *Service) ProposeDataWrite(ctx context.Context, r *automation.Run, raw json.RawMessage) (json.RawMessage, bool, error) {
	if !ProposesWrites(r.Spec) || s.hooks.DataConnections() == nil {
		return nil, false, nil
	}
	// Decode the call as the action would: models send its arguments as a
	// JSON-encoded string. An undecodable call falls through to the action,
	// which reports the same error and never runs a write from chat.
	decoded, err := s.hooks.ActionArguments(r.Spec.Project, "call_data_operation", raw)
	var in dataops.Call
	if err != nil || json.Unmarshal(decoded, &in) != nil {
		return nil, false, nil
	}
	if op, ok := s.hooks.DataConnections().ConfiguredOperation(in.Connection, in.Operation); !ok || op.ReadOnly {
		return nil, false, nil
	}
	if len(r.WriteProposals) >= maxWriteProposals {
		return nil, true, fmt.Errorf("propose at most %d changes in one reply", maxWriteProposals)
	}
	if !s.hooks.DataAllowed(ctx, in.Connection, in.Operation) {
		return nil, true, workspace.ErrForbidden
	}
	if err := s.hooks.DataConnections().CheckArguments(in.Connection, in.Operation, in.Arguments); err != nil {
		return nil, true, err
	}
	contract, _ := s.hooks.DataConnections().Contract(in.Connection)
	call := dataops.Call{Connection: in.Connection, Operation: in.Operation, Arguments: in.Arguments, SchemaHash: in.SchemaHash}
	args, _ := json.Marshal(call)
	state, _ := json.Marshal(writeProposalState{Requester: MessageAddress(*r.Spec.Delivery).User, Expires: time.Now().Add(writeProposalTTL).UTC(), Contract: contract})
	code := make([]byte, 5)
	rand.Read(code)
	r.WriteProposals = append(r.WriteProposals, chat.Operation{ID: r.ID + "-write-" + strconv.Itoa(len(r.WriteProposals)), Name: writeProposalOperation, Arguments: args, Hash: chat.Hash(args), State: "proposed", Result: state, Confirmation: base32.StdEncoding.EncodeToString(code)})
	result, _ := json.Marshal(map[string]string{"status": "awaiting confirmation", "message": "Not run. Your reply will end with this exact action; nothing changes unless the requester confirms it in this conversation. Do not say it has run."})
	return result, true, nil
}

// writeProposalText is appended to the model's answer, so the requester sees
// exactly what would run whatever the model wrote.
func writeProposalText(ops []chat.Operation) string {
	var b strings.Builder
	for _, op := range ops {
		var call dataops.Call
		json.Unmarshal(op.Arguments, &call)
		raw, _ := json.MarshalIndent(call.Arguments, "", "  ")
		args := []rune(string(raw))
		if len(args) > 2000 {
			args = append(args[:2000], []rune("\n…")...)
		}
		fmt.Fprintf(&b, "\n\n**Needs your confirmation. Nothing has changed yet.**\n`%s` / `%s` with:\n```\n%s\n```\nTo run it, reply `confirm %s` to me in this thread within %d minutes, or `cancel %s` to drop it; in a channel, mention me. Only the person who asked can confirm it.", call.Connection, call.Operation, string(args), op.Confirmation, int(writeProposalTTL/time.Minute), op.Confirmation)
	}
	return b.String()
}

// writeConfirmations lets a chat client offer buttons for the proposals.
func writeConfirmations(ops []chat.Operation) []messaging.Confirmation {
	var out []messaging.Confirmation
	for _, op := range ops {
		var call dataops.Call
		json.Unmarshal(op.Arguments, &call)
		out = append(out, messaging.Confirmation{Code: op.Confirmation, Label: "`" + call.Connection + "` / `" + call.Operation + "`"})
	}
	return out
}

// confirmDataWrite runs one confirmed proposal from this conversation, or
// cancels it, and returns the reply. An interrupted write is never repeated.
func (s *Service) confirmDataWrite(ctx context.Context, r *automation.Run, owner string, conversation chat.Conversation, d messaging.Address, code string, cancel bool) (string, error) {
	var call dataops.Call
	var state writeProposalState
	var opID, previous string
	// Claim the proposal before calling out: a crash after this point leaves it
	// "running", and its outcome is reported as unknown instead of repeated.
	err := s.hooks.ChatStore().Update(owner, r.Spec.Project, conversation.ID, func(v *chat.Conversation) error {
		for i := range v.Runs {
			for j := range v.Runs[i].Operations {
				op := &v.Runs[i].Operations[j]
				if op.Name != writeProposalOperation || op.Confirmation != code || json.Unmarshal(op.Arguments, &call) != nil || json.Unmarshal(op.Result, &state) != nil || state.Requester != d.User {
					continue
				}
				opID, previous = op.ID, op.State
				switch {
				case op.State == "proposed" && !time.Now().Before(state.Expires):
					op.State, previous = "expired", "expired"
				case op.State == "proposed" && cancel:
					op.State = "cancelled"
				case op.State == "proposed":
					op.State = "running"
				}
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if opID == "" {
		return "I couldn't find a change waiting for your confirmation with code `" + code + "` in this conversation. Nothing ran.", nil
	}
	label := "`" + call.Connection + "` / `" + call.Operation + "`"
	switch previous {
	case "expired":
		return "That change expired before it was confirmed. Nothing ran; ask again to get a new one.", nil
	case "cancelled":
		return label + " was cancelled. Nothing ran; ask again to get a new one.", nil
	case "running":
		return "The outcome of " + label + " is unknown because it was interrupted. It was not run again; check the result there before asking again.", nil
	case "succeeded":
		return label + " already ran." + linkSuffix(state.Link), nil
	case "failed":
		return label + " already failed and was not run again. Ask again to retry.", nil
	}
	if cancel {
		return "Cancelled " + label + ". Nothing ran.", nil
	}
	contract, _ := s.hooks.DataConnections().Contract(call.Connection)
	var result dataops.Result
	switch {
	case contract != state.Contract:
		err = errors.New("the connection's configuration changed after this was proposed")
	case !slices.Contains(r.Spec.Connections, call.Connection):
		err = workspace.ErrForbidden
	default:
		result, err = s.hooks.DataExecute(context.WithValue(ctx, confirmedWriteKey{}, dataWriteHash(call)), call)
	}
	outcome := "succeeded"
	if err != nil {
		outcome = "failed"
	} else {
		r.Receipts = append(r.Receipts, result.Receipt)
		state.Receipt = result.Receipt.ID
		if data, ok := result.Data.(map[string]any); ok {
			if link, ok := data["html_url"].(string); ok && strings.HasPrefix(link, "https://") {
				state.Link = link
			}
		}
	}
	raw, _ := json.Marshal(state)
	if e := s.hooks.ChatStore().Update(owner, r.Spec.Project, conversation.ID, func(v *chat.Conversation) error {
		for i := range v.Runs {
			for j := range v.Runs[i].Operations {
				if op := &v.Runs[i].Operations[j]; op.ID == opID {
					op.State, op.Result = outcome, raw
					if err != nil {
						op.Error = err.Error()
					}
				}
			}
		}
		return nil
	}); e != nil {
		return "", e
	}
	if err != nil {
		if errors.Is(err, workspace.ErrForbidden) {
			return label + " was not run: you no longer have access to it here.", nil
		}
		return label + " failed and was not retried: " + err.Error(), nil
	}
	return "Done: " + label + " ran." + linkSuffix(state.Link), nil
}

func linkSuffix(link string) string {
	if link == "" {
		return ""
	}
	return " " + link
}
