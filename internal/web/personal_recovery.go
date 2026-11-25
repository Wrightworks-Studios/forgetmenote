package web

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"forgetmenote/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Recovery authorizes a new paid attempt, never an automatic retry or a change
// to a provider's unknown external-write receipt.
func (s *Server) personalRecoveryTools(server *mcp.Server) {
	addAction(s, server, &mcp.Tool{Name: "recover_assistant_work", Description: "Account UI only: explicitly retry or discard your interrupted assistant generation. Always refused over MCP and API credentials; never retries an external write."}, func(ctx context.Context, in struct {
		ID       string `json:"id"`
		Revision string `json:"revision"`
		Retry    bool   `json:"retry"`
	}) (map[string]any, error) {
		if err := s.accountUIRequest(ctx); err != nil {
			return nil, err
		}
		spec, stored, state, err := s.personalUIState(ctx)
		if err != nil {
			return nil, err
		}
		if in.ID == "memory" {
			memorySpec := personalMemorySpec(spec)
			stored, e := s.automations.Queue.JobState(memorySpec)
			if e != nil {
				return nil, e
			}
			if stored.Revision != in.Revision {
				return nil, workspace.ErrConflict
			}
			var memory personalMemoryState
			if err = json.Unmarshal(stored.Data, &memory); err != nil {
				return nil, err
			}
			if memory.Status == "" {
				return nil, errors.New("no interrupted memory work")
			}
			if in.Retry {
				memory.Pending = append(memory.Batch, memory.Pending...)
			} else {
				if memory.Completed == nil {
					memory.Completed = map[string]bool{}
				}
				for _, item := range memory.Batch {
					memory.Completed[item.ID] = true
				}
			}
			memory.Status = ""
			memory.Batch = nil
			memory.Content = nil
			memory.Hashes = nil
			raw, _ := json.Marshal(memory)
			_, err = s.automations.Queue.SaveJobState(memorySpec, in.Revision, raw, false)
		} else {
			if stored.Revision != in.Revision {
				return nil, workspace.ErrConflict
			}
			draft, ok := state.Drafts[in.ID]
			if !ok || draft.State != "generation-unknown" {
				return nil, errors.New("no interrupted suggestion")
			}
			if in.Retry {
				if err = s.personalCurrent(ctx, draft.Source); err != nil {
					return nil, err
				}
				delete(state.Attempts, in.ID)
				state.Pending = slices.DeleteFunc(state.Pending, func(o personalObservation) bool { return o.Key == draft.Source.Key })
				state.Pending = append(state.Pending, draft.Source)
				delete(state.Drafts, in.ID)
			} else {
				draft.State = "dismissed"
				state.Drafts[in.ID] = draft
			}
			raw, _ := json.Marshal(state)
			_, err = s.automations.Queue.SaveJobState(spec, stored.Revision, raw, false)
		}
		return map[string]any{"scheduled": in.Retry}, err
	}, "notes:write")
}
