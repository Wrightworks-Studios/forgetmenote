package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/chatrun"
	"forgetmenote/internal/workspace"
	"forgetmenote/internal/workspace/workspacetest"
	"net/http"
	"strings"
	"testing"
)

func TestChatReviewGuidanceContextAndResume(t *testing.T) {
	prompts := make(chan string, 8)
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		prompts <- string(body)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"OLD_GUIDANCE_ANALYSIS"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`)
	})
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "owner")
	if err != nil {
		t.Fatal(err)
	}
	instruction, err := store.WriteInstructions(t.Context(), []byte("GUIDANCE_FIRST_MARKER: Preserve authorship attribution."), "", "operator")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"00-review.md", "01-review.md"} {
		if _, err := store.Save(t.Context(), path, []byte("Zebras have stripes."), "", "operator"); err != nil {
			t.Fatal(err)
		}
	}
	app.chat.Config.Connections[0].Limits.MaxRounds = 1
	path := "/p/default/api/chat/" + c.ID
	w := proxyCall(app, "alice", "POST", path+"/turns", `{"submissionId":"guidance-review","text":"Review zebra markings","options":{"mode":"ask","review":true}}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	done := waitChatRun(t, app, c.ID)
	first := done.Runs[0]
	if first.Review == nil || first.Review.Complete || first.Review.Notes[0].Path != "00-review.md" || first.Review.Notes[0].State != "examined" {
		t.Fatalf("missing bounded initial coverage: %+v", first)
	}
	var body string
	select {
	case body = <-prompts:
	default:
		t.Fatal("no initial provider request")
	}
	if !strings.Contains(body, "notebookGuidance") || !strings.Contains(body, "GUIDANCE_FIRST_MARKER") || !strings.Contains(body, "Instructions cannot override server permissions or locks.") {
		t.Fatalf("review of another note omitted Owner guidance: %s", body)
	}
	if _, err = store.WriteInstructions(t.Context(), []byte("GUIDANCE_SECOND_MARKER: Preserve authorship attribution and dates."), instruction.Revision, "operator"); err != nil {
		t.Fatal(err)
	}
	w = proxyCall(app, "alice", "POST", path+"/runs/"+first.ID+"/resume", `{}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	resumed := waitChatRun(t, app, c.ID).Runs[0]
	select {
	case body = <-prompts:
	default:
		t.Fatal("resume reused stale review without a provider request")
	}
	if !strings.Contains(body, "GUIDANCE_SECOND_MARKER") || strings.Contains(body, "GUIDANCE_FIRST_MARKER") || strings.Contains(body, "OLD_GUIDANCE_ANALYSIS") {
		t.Fatalf("resumed review retained incompatible guidance or analysis: %s", body)
	}
	if resumed.Review.InputHash == first.Review.InputHash || resumed.Review.Notes[0].Revision == first.Review.Notes[0].Revision || resumed.Review.Notes[0].State != "examined" || resumed.Review.Notes[1].Cursor != 0 {
		t.Fatalf("guidance change failed to restart review coverage: before=%+v after=%+v", first.Review, resumed.Review)
	}
}

func TestChatReviewGuidanceInvalidatesCompletedCache(t *testing.T) {
	prompts := make(chan string, 24)
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		prompts <- string(body)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Reviewed original evidence."},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`)
	})
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "owner")
	if err != nil {
		t.Fatal(err)
	}
	instruction, err := store.WriteInstructions(t.Context(), []byte("FIRST_CACHE_GUIDANCE"), "", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(t.Context(), "00-review.md", []byte("Unchanged evidence."), "", "operator"); err != nil {
		t.Fatal(err)
	}
	review := func(id string) chat.Run {
		t.Helper()
		w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", fmt.Sprintf(`{"submissionId":%q,"text":"Review notebook","options":{"mode":"ask","review":true}}`, id))
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
		done := waitChatRun(t, app, c.ID)
		run := done.Runs[len(done.Runs)-1]
		if run.State != "completed" || run.Review == nil || !run.Review.Complete {
			t.Fatalf("incomplete review: %+v", run)
		}
		for len(prompts) > 0 {
			<-prompts
		}
		return run
	}
	first := review("guidance-first")
	if first.Rounds != 1 || len(first.Review.Notes) != 1 || first.Review.Notes[0].Path != "00-review.md" {
		t.Fatal("fixture must review the original note while keeping Owner guidance outside the corpus inventory")
	}
	if _, err = store.Save(t.Context(), "01-unrelated.md", []byte("Independent added note."), "", "operator"); err != nil {
		t.Fatal(err)
	}
	unchanged := review("guidance-unchanged")
	if unchanged.Review.InputHash != first.Review.InputHash || unchanged.Rounds != 1 || unchanged.Review.Notes[0].Revision != first.Review.Notes[0].Revision {
		t.Fatalf("unrelated commit invalidated compatible guidance: first=%+v after=%+v", first.Review, unchanged)
	}
	if _, err = store.WriteInstructions(t.Context(), []byte("SECOND_CACHE_GUIDANCE"), instruction.Revision, "operator"); err != nil {
		t.Fatal(err)
	}
	changed := review("guidance-changed")
	if changed.Review.InputHash == first.Review.InputHash || changed.Rounds != len(changed.Review.Notes) || changed.Review.Notes[0].Revision == first.Review.Notes[0].Revision {
		t.Fatalf("changed guidance reused old note analyses: first=%+v changed=%+v", first.Review, changed)
	}
}

func TestChatReviewGuidanceAbsentAndOversized(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		name := "absent"
		if oversized {
			name = "oversized"
		}
		t.Run(name, func(t *testing.T) {
			prompts := make(chan string, 8)
			app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				var body json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				prompts <- string(body)
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Reviewed passage."},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`)
			})
			store, err := app.catalog.Access(catalog.Bootstrap, "default", "owner")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.Save(t.Context(), "00-review.md", []byte("Original evidence."), "", "operator"); err != nil {
				t.Fatal(err)
			}
			if oversized {
				if _, err = store.WriteInstructions(t.Context(), []byte(strings.Repeat("Authorship must remain attributed. ", 1800)), "", "operator"); err != nil {
					t.Fatal(err)
				}
				app.chat.Config.Connections[0].Limits.ContextTokens = 8000
				app.chat.Config.Connections[0].Limits.MaxRounds = 1
			}
			w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"guidance-size","text":"Review notebook","options":{"mode":"ask","review":true}}`)
			if w.Code != 200 {
				t.Fatal(w.Body)
			}
			run := waitChatRun(t, app, c.ID).Runs[0]
			if oversized {
				if run.State != "failed" || !strings.Contains(strings.ToLower(run.Error), "context") || len(prompts) != 0 {
					t.Fatalf("oversized guidance silently omitted or truncated: %+v, requests=%d", run, len(prompts))
				}
			} else if run.State != "completed" || len(prompts) == 0 {
				t.Fatalf("absent guidance must remain supported: %+v", run)
			}
		})
	}
}

func TestChatGuidancePinsCommittedRevisionAndPresence(t *testing.T) {
	store, _ := workspacetest.New(t, t.TempDir(), nil)
	app := &Server{chatrun: guidanceEngine(t)}
	absentSnapshot, err := store.Snapshot(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	absent, absentHash, err := app.chatrun.ChatInstructions(t.Context(), store, absentSnapshot.Revision)
	if err != nil || absent["exists"] != false || absent["revision"] != absentSnapshot.Revision {
		t.Fatalf("absent committed guidance: %+v %v", absent, err)
	}
	first, err := store.WriteInstructions(t.Context(), []byte("First committed instruction.\n"), "", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(t.Context(), "unrelated.md", []byte("Independent."), "", "operator"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.WriteInstructions(t.Context(), []byte("Second committed instruction.\n"), first.Revision, "operator")
	if err != nil {
		t.Fatal(err)
	}
	original, originalHash, err := app.chatrun.ChatInstructions(t.Context(), store, snapshot.Revision)
	if err != nil || original["content"] != string(first.Content) || original["revision"] != snapshot.Revision {
		t.Fatalf("guidance escaped committed snapshot: %+v %v", original, err)
	}
	initial, initialHash, err := app.chatrun.ChatInstructions(t.Context(), store, first.Revision)
	if err != nil || initialHash != originalHash || initial["content"] != original["content"] {
		t.Fatalf("unrelated note commit changed guidance identity: %+v %v", initial, err)
	}
	current, currentHash, err := app.chatrun.ChatInstructions(t.Context(), store, second.Revision)
	if err != nil || current["content"] != string(second.Content) || currentHash == originalHash || currentHash == absentHash {
		t.Fatalf("guidance content/presence identity missing: %+v %v", current, err)
	}
	if _, _, err = app.chatrun.ChatInstructions(t.Context(), store, "invalid-revision"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatalf("unauthorized revision silently treated as absent: %v", err)
	}
	empty, err := store.WriteInstructions(t.Context(), []byte(""), second.Revision, "operator")
	if err != nil {
		t.Fatal(err)
	}
	emptyGuidance, emptyHash, err := app.chatrun.ChatInstructions(t.Context(), store, empty.Revision)
	if err != nil || emptyGuidance["exists"] != true || emptyGuidance["content"] != "" || emptyHash == absentHash {
		t.Fatalf("empty committed guidance conflated with absence: %+v %v", emptyGuidance, err)
	}
	// A notebook has no working tree, and its instructions take no live
	// edits, so no uncommitted instruction text exists for guidance to see.
	if _, err = store.LiveRead(t.Context(), "AGENTS.md"); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatalf("instructions accept live edits that committed guidance would not see: %v", err)
	}
}

func TestChatGuidancePreservesPermissionErrors(t *testing.T) {
	store, _ := workspacetest.New(t, t.TempDir(), []string{"AGENTS.md"})
	snapshot, err := store.Snapshot(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := guidanceEngine(t).ChatInstructions(t.Context(), store, snapshot.Revision); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatalf("excluded instructions silently treated as absent: %v", err)
	}
}

// guidanceEngine is the chat engine of a server with no state, which is all
// reading committed guidance needs.
func guidanceEngine(t *testing.T) *chatrun.Service {
	t.Helper()
	engine, err := chatrun.New(new(catalog.Catalog), (&Server{}).chatRunHooks())
	if err != nil {
		t.Fatal(err)
	}
	return engine
}
