package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/chatrun"
	"forgetmenote/internal/sources"
	"forgetmenote/internal/workspace"
)

func TestReleaseSecurityResumeCannotFinalizeOldResearchCoverage(t *testing.T) {
	var requests atomic.Int32
	var revision string
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		n := requests.Add(1)
		var name, args string
		switch n {
		case 1:
			name = "research_fetch"
			args = `{"url":"https://example.org/source.txt"}`
		case 2:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Original examined using dated research evidence."},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`)
			return
		case 3:
			name = "discover_actions"
			args = `{"names":["complete_workflow"]}`
		case 4:
			name = "complete_workflow"
			raw, _ := json.Marshal(map[string]any{"completion": map[string]any{"consumer": "security-research", "previous": "", "revision": revision, "summary": "Research-informed review complete", "reset": false}})
			args = string(raw)
		default:
			// Pause further provider work: the assertion records whether completion
			// has already dispatched before any fresh original-source review call.
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Re-examined current originals."},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": fmt.Sprintf("call-%d", n), "type": "function", "function": map[string]string{"name": name, "arguments": args}}}}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 20, "completion_tokens": 5}})
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	store, err := app.catalog.Access("alice", "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	note, err := store.Save(t.Context(), "Evidence.md", []byte("One committed original."), "", "operator")
	if err != nil {
		t.Fatal(err)
	}
	revision = note.Revision
	secret := filepath.Join(t.TempDir(), "synthetic-secret")
	if err = os.WriteFile(secret, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &chat.ResearchConfig{Enabled: true, Format: "brave", BaseURL: "https://search.invalid", SecretFile: secret, MaxSearches: 1, MaxFetches: 1}
	research, err := chat.NewResearch(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	app.chat.Mu.Lock()
	config := *app.chat.Config
	config.Connections = append([]chat.Connection(nil), config.Connections...)
	config.Research = cfg
	config.Connections[0].Limits.MaxTools = 2
	app.chat.Config, app.chat.Research = &config, research
	app.chat.PrepareSource = func(ctx context.Context, filename, originalURL string, data []byte) (workspace.SourceCapture, error) {
		return sources.Prepare(ctx, "source.txt", originalURL, []byte("Synthetic research original."))
	}
	app.chat.Mu.Unlock()
	var staleCompletion atomic.Bool
	action := app.actions["complete_workflow"]
	originalCall := action.Call
	action.Call = func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		current, e := app.chat.Store.Read("alice", "default", c.ID)
		if e != nil {
			return nil, e
		}
		run := current.Runs[0]
		if requests.Load() == 4 && run.ReviewAllowance > 0 && run.Review.Notes[0].Provenance.ExternalAllowance != chatrun.ReviewExternalAllowance(&run) {
			staleCompletion.Store(true)
		}
		return originalCall(ctx, args)
	}
	app.actions["complete_workflow"] = action
	path := "/p/default/api/chat/" + c.ID
	w := proxyCall(app, "alice", "POST", path+"/turns", `{"submissionId":"research-finalization","text":"Review this notebook with research from https://example.org/source.txt, then complete workflow security-research with previous empty.","options":{"mode":"edit","actions":["review"],"review":true,"research":true}}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	first := waitChatRun(t, app, c.ID)
	app.chat.Wait()
	run := first.Runs[0]
	if requests.Load() != 4 || run.State != "interrupted" || !run.Review.Complete || len(chatrun.PendingToolCalls(run.Messages)) != 1 || run.Review.Notes[0].Provenance.ExternalAllowance == "" {
		t.Fatalf("fixture failed to pause complete workflow at tool budget: requests=%d run=%+v", requests.Load(), run)
	}
	w = proxyCall(app, "alice", "POST", path+"/runs/"+run.ID+"/resume", `{}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	done := waitChatRun(t, app, c.ID)
	app.chat.Wait()
	engine, releaseEngine, err := app.acquireEngine(t.Context(), store)
	if releaseEngine != nil {
		t.Cleanup(releaseEngine)
	}

	if err != nil {
		t.Fatal(err)
	}
	cp, err := engine.Checkpoint(t.Context(), "alice", "security-research")
	if err != nil {
		t.Fatal(err)
	}
	if staleCompletion.Load() {
		t.Fatalf("checkpoint advanced from prior-allowance external coverage before any renewed provider review: checkpoint=%q requests=%d final=%s", cp.Revision, requests.Load(), done.Runs[0].State)
	}
}

func TestChatWorkflowResumeDistinguishesNewIntentsAndReceipts(t *testing.T) {
	for _, scenario := range []string{"committed-only", "external-unexecuted", "external-running", "external-acknowledged", "external-lost-ack", "external-sibling", "external-workflow-sibling"} {
		t.Run(scenario, func(t *testing.T) {
			var requests, dispatches atomic.Int32
			var revision string
			args := func() json.RawMessage {
				raw, _ := json.Marshal(map[string]any{"completion": map[string]any{"consumer": "receipt-review", "previous": "", "revision": revision, "summary": "Reviewed original inventory", "reset": false}})
				return raw
			}
			f := newReviewDependencyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var body struct {
					Messages []struct {
						Role       string `json:"role"`
						ToolCallID string `json:"tool_call_id"`
						ToolCalls  []struct {
							ID string `json:"id"`
						} `json:"tool_calls"`
						Content string `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				outstanding := map[string]bool{}
				finalizing := false
				for _, message := range body.Messages {
					if message.Role == "assistant" {
						if len(outstanding) > 0 {
							t.Error("native tool requests were not paired before next assistant")
						}
						for _, call := range message.ToolCalls {
							outstanding[call.ID] = true
						}
					}
					if message.Role == "tool" {
						if !outstanding[message.ToolCallID] {
							t.Error("orphaned native tool result after refreshed review")
						}
						delete(outstanding, message.ToolCallID)
					}
					finalizing = finalizing || strings.Contains(message.Content, "<review_finalization>")
				}
				if len(outstanding) > 0 {
					t.Error("unresolved old native tool call reached provider")
				}
				w.Header().Set("Content-Type", "application/json")
				if finalizing {
					json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "renewed-completion", "type": "function", "function": map[string]string{"name": "complete_workflow", "arguments": string(args())}}}}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 20, "completion_tokens": 5}})
				} else {
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Examined the refreshed committed original."},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`)
				}
			})
			if err := f.app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
				t.Fatal(err)
			}
			f.update(t, func(r *chat.Run) {
				r.Options.Mode = "edit"
				r.Options.Actions = []string{"review"}
				r.ToolNames = []string{"complete_workflow"}
			})
			for i := 0; i < 8; i++ {
				r := f.prepare(t)
				if r.Review.Complete {
					break
				}
				f.update(t, func(r *chat.Run) { chatrun.FinishReviewBatch(r, "Previously examined original.") })
			}
			initial := f.read(t)
			if !initial.Review.Complete {
				t.Fatal("fixture original inventory incomplete")
			}
			revision = initial.Review.Revision
			call := chat.ToolCall{ID: "pending-workflow", Name: "complete_workflow", Arguments: args()}
			f.update(t, func(r *chat.Run) {
				if scenario != "committed-only" {
					for i := range r.Review.Notes {
						r.Review.Notes[i].Provenance.ExternalAllowance = chatrun.ReviewExternalAllowance(r)
						r.Review.Notes[i].Provenance.ExternalSources = []chat.ExternalCapture{{URL: "https://example.org/source.txt", CapturedAt: "2026-09-14T12:00:00Z", Hash: strings.Repeat("a", 64)}}
					}
				}
				issued := []chat.ToolCall{call}
				if scenario == "external-sibling" {
					issued = append(issued, chat.ToolCall{ID: "pending-read", Name: "read_note", Arguments: json.RawMessage(`{"path":"notes/a.md"}`)})
				}
				if scenario == "external-workflow-sibling" {
					sibling := call
					sibling.ID = "pending-workflow-2"
					issued = append(issued, sibling)
				}
				r.Messages = append(r.Messages, chat.Message{Role: "assistant", ToolCalls: issued})
				r.Rounds = 1
				r.State = "interrupted"
				r.ToolCount = f.connection.Limits.MaxTools
				r.ToolLimit = r.ToolCount
			})
			action := f.app.actions["complete_workflow"]
			original := action.Call
			action.Call = func(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
				dispatches.Add(1)
				if scenario == "external-unexecuted" || scenario == "external-running" || strings.HasSuffix(scenario, "sibling") {
					current := f.read(t)
					if current.ReviewAllowance > 0 && requests.Load() == 0 {
						t.Error("new checkpoint dispatched before renewed external coverage")
					}
				}
				return original(ctx, arguments)
			}
			f.app.actions["complete_workflow"] = action
			engine, releaseEngine, err := f.app.acquireEngine(t.Context(), f.store)
			if releaseEngine != nil {
				t.Cleanup(releaseEngine)
			}

			if err != nil {
				t.Fatal(err)
			}
			if scenario == "external-acknowledged" || scenario == "external-lost-ack" {
				ctx, err := f.app.runContext(f.auth)
				if err != nil {
					t.Fatal(err)
				}
				if err = f.app.chatrun.ExecuteNotebookTool(ctx, f.auth, f.conversation, f.run, call, f.connection); err != nil {
					t.Fatal(err)
				}
				f.update(t, func(r *chat.Run) {
					r.Messages = r.Messages[:len(r.Messages)-1]
					if scenario == "external-lost-ack" {
						r.Operations[len(r.Operations)-1].State = "running"
						r.Operations[len(r.Operations)-1].Result = nil
					}
				})
			} else if scenario == "external-running" {
				f.update(t, func(r *chat.Run) {
					r.Operations = append(r.Operations, chat.Operation{ID: chatrun.OperationID(r.ID, r.Rounds, call.ID), ToolCallID: call.ID, Round: r.Rounds, Name: call.Name, Arguments: call.Arguments, Hash: chat.Hash(call.Arguments), State: "running"})
				})
			}
			checkpointBefore, err := engine.Checkpoint(t.Context(), "alice", "receipt-review")
			if err != nil {
				t.Fatal(err)
			}
			w := proxyCall(f.app, "alice", "POST", "/p/default/api/chat/"+f.conversation+"/runs/"+f.run+"/resume", `{}`)
			if w.Code != 200 {
				t.Fatal(w.Body)
			}
			waitChatRun(t, f.app, f.conversation)
			f.app.chat.Wait()
			done := f.read(t)
			checkpointAfter, err := engine.Checkpoint(t.Context(), "alice", "receipt-review")
			if err != nil {
				t.Fatal(err)
			}
			if done.State != "completed" || !done.Review.Complete || checkpointAfter.Revision != revision {
				t.Fatalf("legitimate completion/reconciliation failed: %+v checkpoint=%+v", done, checkpointAfter)
			}
			if len(chatrun.PendingToolCalls(done.Messages)) != 0 {
				t.Fatal("durable native completion pair remains unresolved")
			}
			if scenario == "committed-only" && (requests.Load() != 0 || dispatches.Load() != 1) {
				t.Fatal("unchanged committed continuation unnecessarily regenerated or repeated mutation")
			}
			if strings.HasPrefix(scenario, "external-") && requests.Load() < 3 {
				t.Fatal("old external review was not re-examined")
			}
			if scenario == "external-acknowledged" && dispatches.Load() != 1 {
				t.Fatal("acknowledged receipt was dispatched again")
			}
			if scenario == "external-lost-ack" && dispatches.Load() != 2 {
				t.Fatal("lost acknowledgement did not reconcile once through original idempotent operation")
			}
			if checkpointBefore.Revision != "" && checkpointAfter != checkpointBefore {
				t.Fatal("receipt reconciliation changed the original checkpoint")
			}
			if scenario == "external-unexecuted" || scenario == "external-running" || strings.HasSuffix(scenario, "sibling") {
				if dispatches.Load() != 1 {
					t.Fatal("deferred original completion mutated checkpoint or renewed operation duplicated")
				}
				deferred := false
				for _, op := range done.Operations {
					if op.ToolCallID == call.ID && strings.Contains(string(op.Result), `"executed":false`) {
						deferred = true
					}
				}
				if !deferred {
					t.Fatal("old pending request was discarded without durable unexecuted result")
				}
			}
		})
	}
}
