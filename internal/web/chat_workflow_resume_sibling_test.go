package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/chatrun"
	"forgetmenote/internal/sources"
	"forgetmenote/internal/workspace"
)

func TestSecurityIndependentDeferredWorkflowPreservesSiblingPairs(t *testing.T) {
	var requests atomic.Int32
	var revision string
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role       string `json:"role"`
				ToolCallID string `json:"tool_call_id"`
				ToolCalls  []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		paired := map[string]bool{}
		for _, m := range body.Messages {
			for _, call := range m.ToolCalls {
				paired[call.ID] = true
			}
			if m.Role == "tool" && !paired[m.ToolCallID] {
				t.Errorf("native provider request contains orphaned tool result %q after stale completion deferral", m.ToolCallID)
			}
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
		calls := []any{map[string]any{"id": fmt.Sprintf("call-%d", n), "type": "function", "function": map[string]string{"name": name, "arguments": args}}}
		if n == 4 {
			calls = append(calls, map[string]any{"id": "pending-sibling", "type": "function", "function": map[string]string{"name": "discover_actions", "arguments": `{"names":["consumer_checkpoint"]}`}})
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": calls}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 20, "completion_tokens": 5}})
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
	if requests.Load() != 4 || run.State != "interrupted" || !run.Review.Complete || len(chatrun.PendingToolCalls(run.Messages)) != 2 || run.Review.Notes[0].Provenance.ExternalAllowance == "" {
		t.Fatalf("fixture failed to pause complete workflow at tool budget: requests=%d run=%+v", requests.Load(), run)
	}
	w = proxyCall(app, "alice", "POST", path+"/runs/"+run.ID+"/resume", `{}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	done := waitChatRun(t, app, c.ID)
	app.chat.Wait()
	if done.Runs[0].State != "completed" || requests.Load() < 5 {
		t.Fatalf("renewed review cannot continue: state=%s error=%s requests=%d messages=%+v", done.Runs[0].State, done.Runs[0].Error, requests.Load(), done.Runs[0].Messages)
	}
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
