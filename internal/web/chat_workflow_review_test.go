package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReleaseCorrectnessReviewCanCompleteRequestedWorkflow(t *testing.T) {
	var requests atomic.Int32
	var revision string
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		var raw map[string]any
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"discover","type":"function","function":{"name":"discover_actions","arguments":"{\"names\":[\"complete_workflow\"]}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":10}}`)
			return
		}
		if n == 2 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Examined the only original passage. Next complete the explicitly requested workflow checkpoint after the server confirms coverage."},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":10}}`)
			return
		}
		args, _ := json.Marshal(map[string]any{"completion": map[string]any{"consumer": "release-probe", "previous": "", "revision": revision, "summary": "Examined all original passages", "reset": false}})
		response := map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "complete", "type": "function", "function": map[string]any{"name": "complete_workflow", "arguments": string(args)}}}}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 20, "completion_tokens": 10}}
		json.NewEncoder(w).Encode(response)
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	w := proxyCall(app, "alice", "POST", "/p/default/api/note", `{"path":"Evidence.md","content":"The only original notebook review passage.","revision":""}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	store, _ := app.catalog.Access("alice", "default", "read")
	snapshot, err := store.Snapshot(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	revision = snapshot.Revision
	if len(snapshot.Files) != 1 {
		t.Fatalf("fixture expected one note: %+v", snapshot.Files)
	}
	w = proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"workflow-probe","text":"Review the entire notebook, then record completion using complete_workflow for consumer release-probe with previous empty.","options":{"mode":"edit","actions":["review"],"review":true}}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	done := waitChatRun(t, app, c.ID)
	run := done.Runs[0]
	if run.Review == nil || !run.Review.Complete || run.State != "completed" {
		t.Fatalf("review not complete: %+v", run)
	}
	engine, releaseEngine, err := app.acquireEngine(t.Context(), store)
	if releaseEngine != nil {
		t.Cleanup(releaseEngine)
	}

	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := engine.Checkpoint(t.Context(), "alice", "release-probe")
	if err != nil {
		t.Fatal(err)
	}
	w = proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/runs/"+run.ID+"/resume", `{}`)
	if checkpoint.Revision != revision {
		t.Fatalf("requested checkpoint stranded: provider requests=%d, complete=%v, run=%s, checkpoint=%q, expected=%q, Continue=%d %s", requests.Load(), run.Review.Complete, run.State, checkpoint.Revision, revision, w.Code, strings.TrimSpace(w.Body.String()))
	}
}

func TestChatReviewWorkflowFinalizationBudgetsAndGuards(t *testing.T) {
	for _, scenario := range []string{"resume", "conflict", "no-rebase", "wrong-revision", "changed-snapshot", "no-operation"} {
		t.Run(scenario, func(t *testing.T) {
			var requests atomic.Int32
			var revision, previous string
			var store *workspace.Store
			prompts := make(chan string, 16)
			app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				var body json.RawMessage
				json.NewDecoder(r.Body).Decode(&body)
				prompts <- string(body)
				n := requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if n == 1 && scenario != "no-operation" {
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"discover-workflow","type":"function","function":{"name":"discover_actions","arguments":"{\"names\":[\"complete_workflow\"]}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":10}}`)
					return
				}
				if n == 2 || n == 4 && scenario != "no-rebase" || scenario == "no-operation" {
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Original examination complete; only finish the requested workflow when its preconditions hold."},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":10}}`)
					return
				}
				target := revision
				if scenario == "wrong-revision" {
					target = previous
				}
				if scenario == "changed-snapshot" && n == 3 {
					if _, err := store.Save(r.Context(), "Evidence.md", []byte("Changed while finalizing."), revision, "operator"); err != nil {
						t.Error(err)
					}
				}
				expected := ""
				if scenario == "no-rebase" && n == 4 {
					expected = previous
				}
				args, _ := json.Marshal(map[string]any{"completion": map[string]any{"consumer": "bounded-workflow", "previous": expected, "revision": target, "summary": "Completed original review", "reset": false}})
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": fmt.Sprintf("finish-%d", n), "type": "function", "function": map[string]any{"name": "complete_workflow", "arguments": string(args)}}}}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 20, "completion_tokens": 10}})
			})
			if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
				t.Fatal(err)
			}
			var err error
			store, err = app.catalog.Access("alice", "default", "edit")
			if err != nil {
				t.Fatal(err)
			}
			first, err := store.Save(t.Context(), "Evidence.md", []byte("Old committed original."), "", "operator")
			if err != nil {
				t.Fatal(err)
			}
			previous = first.Revision
			engine, releaseEngine, err := app.acquireEngine(t.Context(), store)
			if releaseEngine != nil {
				t.Cleanup(releaseEngine)
			}

			if err != nil {
				t.Fatal(err)
			}
			if scenario == "conflict" || scenario == "no-rebase" {
				if _, err = engine.Complete(t.Context(), "alice", "bounded-workflow", "", previous, "Other workflow writer", false); err != nil {
					t.Fatal(err)
				}
			}
			current, err := store.Save(t.Context(), "Evidence.md", []byte("Current committed original."), previous, "operator")
			if err != nil {
				t.Fatal(err)
			}
			revision = current.Revision
			if scenario == "resume" {
				app.chat.Config.Connections[0].Limits.MaxRounds = 2
			}
			path := "/p/default/api/chat/" + c.ID
			w := proxyCall(app, "alice", "POST", path+"/turns", `{"submissionId":"finalize-budget","text":"Review all notes and complete workflow bounded-workflow with previous empty if requested preconditions hold.","options":{"mode":"edit","actions":["review"],"review":true}}`)
			if w.Code != 200 {
				t.Fatal(w.Body)
			}
			firstRun := waitChatRun(t, app, c.ID)
			app.chat.Wait()
			run := firstRun.Runs[0]
			if run.Review == nil || !run.Review.Complete {
				t.Fatalf("finalization erased examined coverage: %+v", run)
			}
			if scenario == "resume" {
				cp, err := engine.Checkpoint(t.Context(), "alice", "bounded-workflow")
				if err != nil || cp.Revision != "" || run.State != "interrupted" || requests.Load() != 2 || !run.Review.Finalizing {
					t.Fatalf("spent beyond two rounds or stranded finalization: %+v checkpoint=%+v", run, cp)
				}
				w = proxyCall(app, "alice", "POST", path+"/runs/"+run.ID+"/resume", `{}`)
				if w.Code != 200 {
					t.Fatal(w.Body)
				}
				resumed := waitChatRun(t, app, c.ID)
				app.chat.Wait()
				run = resumed.Runs[0]
			}
			cp, err := engine.Checkpoint(t.Context(), "alice", "bounded-workflow")
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "resume":
				if run.State != "completed" || cp.Revision != revision || requests.Load() != 3 {
					t.Fatalf("explicit resume failed bounded checkpoint: %+v %+v", run, cp)
				}
			case "conflict", "no-rebase":
				if cp.Revision != previous || cp.Summary != "Other workflow writer" {
					t.Fatalf("checkpoint CAS was bypassed: %+v", cp)
				}
				saw := false
				for _, op := range run.Operations {
					if op.Name == "complete_workflow" && op.Error != "" {
						saw = true
					}
				}
				if !saw {
					t.Fatal("CAS conflict was not durable")
				}
				if scenario == "no-rebase" && !strings.Contains(run.Error, "precondition changed") {
					t.Fatalf("model silently rebased checkpoint conflict: %+v", run)
				}
			case "wrong-revision", "changed-snapshot":
				if cp.Revision != "" || run.State == "completed" {
					t.Fatalf("unexamined snapshot certified workflow: %+v %+v", run, cp)
				}
			case "no-operation":
				if cp.Revision != "" || run.State != "completed" || requests.Load() != 2 || len(run.Operations) != 0 {
					t.Fatalf("finalization automatically wrote a checkpoint: %+v %+v", run, cp)
				}
			}
			<-prompts
			if scenario != "no-operation" {
				<-prompts
			}
			finalPrompt := <-prompts
			if !strings.Contains(finalPrompt, "review_finalization") || !strings.Contains(finalPrompt, revision) || !strings.Contains(finalPrompt, "previous empty") {
				t.Fatalf("finalization lost original checkpoint/snapshot task: %s", finalPrompt)
			}
		})
	}
}
