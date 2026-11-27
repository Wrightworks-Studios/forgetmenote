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
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/chatrun"
	"forgetmenote/internal/workspace"
)

func TestChatSnapshotIncludesPersistedPartialAnswer(t *testing.T) {
	app := chatFixture(t)
	c, err := app.chat.Store.Create("alice", "default", chat.Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Turns = append(c.Turns, chat.Turn{ID: "question", Role: "user", Content: "Question"})
		c.Runs = append(c.Runs, chat.Run{ID: "run", State: "running"})
		c.AddEvent("run", "text_delta", map[string]string{"turnId": "partial", "text": "First "})
		c.AddEvent("run", "text_delta", map[string]string{"turnId": "partial", "text": "second"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w := proxyCall(app, "alice", "GET", "/p/default/api/chat/"+c.ID, "")
	var got struct {
		Turns       []chat.Turn `json:"turns"`
		EventCursor int         `json:"eventCursor"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || got.EventCursor != 2 || len(got.Turns) != 2 || got.Turns[1].Content != "First second" {
		t.Fatalf("snapshot skips durable streamed text: %d %s", w.Code, w.Body)
	}
}

func TestChatFullReviewInventoriesAndResumesChangedNotes(t *testing.T) {
	var requests atomic.Int32
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var body any
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Examined supplied original passages. No unsupported claims."},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":10}}`)
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		w := proxyCall(app, "alice", "POST", "/p/default/api/note", fmt.Sprintf(`{"path":"notes/%s.md","content":"# %s\nDistinct original evidence %s.","revision":""}`, name, name, name))
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
	}
	app.chat.Config.Connections[0].Limits.MaxRounds = 1
	path := "/p/default/api/chat/" + c.ID
	w := proxyCall(app, "alice", "POST", path+"/turns", `{"submissionId":"review","text":"Review every note and compare original evidence","options":{"mode":"ask","review":true}}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	done := waitChatRun(t, app, c.ID)
	run := done.Runs[0]
	if run.Review == nil || len(run.Review.Notes) < 3 {
		t.Fatalf("full review did not inventory notebook: %+v", run)
	}
	if run.Review.Complete || run.State == "completed" {
		t.Fatalf("one bounded batch falsely completed inventory: %+v", run.Review)
	}
	examined := ""
	for _, n := range run.Review.Notes {
		if n.State == "examined" {
			examined = n.Path
			break
		}
	}
	if examined == "" {
		t.Fatal("successful original-source batch not recorded")
	}
	store, _ := app.catalog.Access("alice", "default", "read")
	old, e := store.Read(t.Context(), examined, "")
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(map[string]string{"path": examined, "content": "# Changed\nFresh evidence supersedes earlier analysis.", "revision": old.Revision})
	if w = proxyCall(app, "alice", "POST", "/p/default/api/note", string(raw)); w.Code != 200 {
		t.Fatal(w.Body)
	}
	w = proxyCall(app, "alice", "POST", path+"/runs/"+run.ID+"/resume", `{}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	done = waitChatRun(t, app, c.ID)
	for _, n := range done.Runs[0].Review.Notes {
		if n.Path == examined && n.Hash == workspace.ContentHash(old.Content) {
			t.Fatal("changed note retained obsolete coverage")
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("unbounded review requests: %d", requests.Load())
	}
}

func TestChatCustomToolRejectsUnknownFields(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	run := chat.Run{ID: chat.NewID(), State: "running", Options: chat.Options{Corpus: c.Options.Corpus, Mode: "ask"}}
	app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Turns = append(c.Turns, chat.Turn{ID: "origin-" + run.ID, RunID: run.ID, Role: "user", Content: "Explicit fixture task"})
		c.Runs = append(c.Runs, run)
		return nil
	})
	auth := chat.RunAuth{User: "alice", Project: "default", Epoch: app.catalog.Epoch("alice")}
	ctx, e := app.runContext(auth)
	if e != nil {
		t.Fatal(e)
	}
	e = app.chatrun.ExecuteChatTool(ctx, auth, c.ID, run.ID, chat.ToolCall{ID: "bad", Name: "search_passages", Arguments: json.RawMessage(`{"text":"alpha","endpoint":"https://untrusted.invalid"}`)}, app.chat.Config.Connections[0])
	if e == nil || !strings.Contains(e.Error(), "argument") {
		t.Fatalf("unknown custom tool field accepted: %v", e)
	}
}

func TestChatIncompleteReviewCannotAdvanceWorkflow(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	store, _ := app.catalog.Access("alice", "default", "read")
	status, err := store.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	run := chat.Run{ID: chat.NewID(), State: "running", Options: chat.Options{Corpus: c.Options.Corpus, Mode: "edit", Actions: []string{"review"}, Review: true}, Review: &chat.Coverage{CorpusID: c.Options.Corpus.ID(), Revision: status.Revision, Notes: []chat.ReviewNote{{Path: "notes/unexamined.md", State: "pending", Total: 1}}}}
	app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Turns = append(c.Turns, chat.Turn{ID: "origin-" + run.ID, RunID: run.ID, Role: "user", Content: "Explicit fixture task"})
		c.Runs = append(c.Runs, run)
		return nil
	})
	auth := chat.RunAuth{User: "alice", Project: "default", Epoch: app.catalog.Epoch("alice")}
	ctx, e := app.runContext(auth)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(map[string]any{"completion": map[string]any{"consumer": "chat-review", "previous": "", "revision": status.Revision, "summary": "Claimed full review", "reset": false}})
	if _, _, schemaErr := app.actionArguments("default", "complete_workflow", raw); schemaErr != nil {
		t.Fatal(schemaErr)
	}
	err = app.chatrun.ExecuteChatTool(ctx, auth, c.ID, run.ID, chat.ToolCall{ID: "checkpoint", Name: "complete_workflow", Arguments: raw}, app.chat.Config.Connections[0])
	if err == nil || !strings.Contains(err.Error(), "incomplete review") {
		t.Fatalf("incomplete notebook review did not reach the coverage guard: %v", err)
	}
}

func TestChatChangedEvidenceInvalidatesCompactHistory(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit")
	w := proxyCall(app, "alice", "POST", "/p/default/api/note", `{"path":"notes/source.md","content":"Original source","revision":""}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	store, _ := app.catalog.Access("alice", "default", "read")
	original, err := store.Read(t.Context(), "notes/source.md", "")
	if err != nil {
		t.Fatal(err)
	}
	err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Turns = append(c.Turns, chat.Turn{ID: "old", Role: "assistant", Content: "OUTDATED_CLAIM_SHOULD_NOT_REENTER_CONTEXT", Citations: []chat.Citation{{Path: "notes/source.md", Revision: original.Revision}}})
		for i := 0; i < 10; i++ {
			c.Turns = append(c.Turns, chat.Turn{ID: fmt.Sprint(i), Role: "user", Content: "Small previous message"})
		}
		c.Turns = append(c.Turns, chat.Turn{ID: "current", Role: "user", Content: "What does the source say now?"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"path": "notes/source.md", "content": "Changed source evidence", "revision": original.Revision})
	if w = proxyCall(app, "alice", "POST", "/p/default/api/note", string(raw)); w.Code != 200 {
		t.Fatal(w.Body)
	}
	c, _ = app.chat.Store.Read("alice", "default", c.ID)
	messages, _, memory, err := initialChatTestMessages(t, app, c, app.chat.Config.Connections[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		if strings.Contains(m.Content, "OUTDATED_CLAIM_SHOULD_NOT_REENTER_CONTEXT") {
			t.Fatal("changed evidence retained obsolete compact analysis")
		}
	}
	if memory.Revisions["notes/source.md"] == "" {
		t.Fatal("compact memory did not bind selected evidence revisions")
	}
}

func TestChatResearchRequiresBothOptInsAndKeepsDurableLimits(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	path := "/p/default/api/chat/" + c.ID + "/turns"
	w := proxyCall(app, "alice", "POST", path, `{"submissionId":"disabled","text":"Look up this topic","options":{"mode":"ask","research":true}}`)
	if w.Code != 400 {
		t.Fatal("unconfigured research run accepted")
	}
	secret := filepath.Join(t.TempDir(), "search-secret")
	os.WriteFile(secret, []byte("synthetic"), 0600)
	configured := &chat.ResearchConfig{Enabled: true, Format: "brave", BaseURL: "https://search.invalid", SecretFile: secret, MaxSearches: 1, MaxFetches: 1}
	research, err := chat.NewResearch(*configured)
	if err != nil {
		t.Fatal(err)
	}
	app.chat.Mu.Lock()
	config := *app.chat.Config
	config.Research = configured
	app.chat.Config, app.chat.Research = &config, research
	app.chat.Mu.Unlock()
	run := chat.Run{ID: chat.NewID(), State: "running", Options: chat.Options{Corpus: c.Options.Corpus, Mode: "ask"}, ResearchSearches: 1}
	app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Turns = append(c.Turns, chat.Turn{ID: "origin-" + run.ID, RunID: run.ID, Role: "user", Content: "Explicit fixture task"})
		c.Runs = append(c.Runs, run)
		return nil
	})
	auth := chat.RunAuth{User: "alice", Project: "default", Epoch: app.catalog.Epoch("alice")}
	ctx, err := app.runContext(auth)
	if err != nil {
		t.Fatal(err)
	}
	connection := app.chat.Config.Connections[0]
	call := chat.ToolCall{ID: "search", Name: "research_search", Arguments: json.RawMessage(`{"query":"synthetic"}`)}
	if err = app.chatrun.ExecuteChatTool(ctx, auth, c.ID, run.ID, call, connection); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatal("operator-only opt-in authorized research")
	}
	app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error { c.Run(run.ID).Options.Research = true; return nil })
	if err = app.chatrun.ExecuteChatTool(ctx, auth, c.ID, run.ID, call, connection); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("durable research limit not enforced: %v", err)
	}
	setChatFixtureLocalOnly(t, app, true)
	if err = app.chatrun.ExecuteChatTool(ctx, auth, c.ID, run.ID, call, connection); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatal("local-only mode authorized research")
	}
	setChatFixtureLocalOnly(t, app, false)
	call.Name = "research_fetch"
	call.Arguments = json.RawMessage(`{"url":"http://127.0.0.1/private"}`)
	if err = app.chatrun.ExecuteChatTool(ctx, auth, c.ID, run.ID, call, connection); err == nil {
		t.Fatal("research accepted private network source")
	}
	got, _ := app.chat.Store.Read("alice", "default", c.ID)
	if got.Runs[0].ResearchSearches != 1 || got.Runs[0].ResearchFetches != 0 {
		t.Fatal("rejected request changed durable allowance")
	}
}

func TestChatPersonalUsageIncludesOnlyCurrentOwner(t *testing.T) {
	app := chatFixture(t)
	for _, user := range []string{"alice", "bob"} {
		c, err := app.chat.Store.Create(user, "default", chat.Options{})
		if err != nil {
			t.Fatal(err)
		}
		tokens := int64(10)
		if user == "bob" {
			tokens = 500
		}
		app.chat.Store.Update(user, "default", c.ID, func(c *chat.Conversation) error {
			c.Runs = append(c.Runs, chat.Run{ID: "done", State: "completed", Usage: chat.Accounting{InputTokens: tokens, OutputTokens: 5, Requests: 1}})
			return nil
		})
	}
	w := proxyCall(app, "alice", "GET", "/api/chat/connections", "")
	var body struct {
		Usage struct {
			Total chat.Accounting `json:"total"`
		} `json:"usage"`
	}
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Usage.Total.InputTokens != 10 || body.Usage.Total.OutputTokens != 5 {
		t.Fatalf("personal usage missing or cross-user: %s", w.Body)
	}
}

func TestChatEmptyTrashApprovalBindsExactPreview(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner")
	store, _ := app.catalog.Access("alice", "default", "owner")
	trashOne := func(name string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"path": "notes/" + name + ".md", "content": "Synthetic trash fixture", "revision": ""})
		w := proxyCall(app, "alice", "POST", "/p/default/api/note", string(raw))
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
		status, err := store.Status(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = store.TrashNotes(t.Context(), []string{"notes/" + name + ".md"}, status.Revision, "alice"); err != nil {
			t.Fatal(err)
		}
	}
	trashOne("first")
	run := chat.Run{ID: chat.NewID(), State: "running", Rounds: 1, Options: chat.Options{Corpus: c.Options.Corpus, Mode: "edit"}}
	app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Turns = append(c.Turns, chat.Turn{ID: "origin-" + run.ID, RunID: run.ID, Role: "user", Content: "Explicit fixture task"})
		c.Runs = append(c.Runs, run)
		return nil
	})
	auth := chat.RunAuth{User: "alice", Project: "default", Epoch: app.catalog.Epoch("alice")}
	ctx, err := app.runContext(auth)
	if err != nil {
		t.Fatal(err)
	}
	call := chat.ToolCall{ID: "empty", Name: "empty_trash", Arguments: json.RawMessage(`{"revision":"model-cannot-authorize","confirmation":"model-cannot-approve"}`)}
	err = app.chatrun.ExecuteChatTool(ctx, auth, c.ID, run.ID, call, app.chat.Config.Connections[0])
	if err != chatrun.ErrConfirmation {
		t.Fatalf("exact approval was not prepared: %v", err)
	}
	prepared, _ := app.chat.Store.Read("alice", "default", c.ID)
	operation := prepared.Runs[0].Operations[0]
	var preview workspace.TrashListing
	if json.Unmarshal(operation.Result, &preview) != nil || len(preview.Entries) != 1 {
		t.Fatal("approval omitted actual trash preview")
	}
	trashOne("second")
	raw, _ := json.Marshal(map[string]string{"decision": "approve", "confirmation": operation.Confirmation})
	path := "/p/default/api/chat/" + c.ID + "/actions/" + operation.ID
	response := proxyCall(app, "alice", "POST", path, string(raw))
	if response.Code != 200 || !strings.Contains(response.Body.String(), "error") {
		t.Fatalf("stale approval did not report failure: %d %s", response.Code, response.Body)
	}
	listing, err := store.ListTrash(t.Context())
	if err != nil || len(listing.Entries) != 2 {
		t.Fatal("stale preview emptied changed Trash")
	}
	if response = proxyCall(app, "alice", "POST", path, string(raw)); response.Code != 409 {
		t.Fatal("consumed stale approval remained reusable")
	}
}

func TestChatExplicitDecisionDeclineAndCancellation(t *testing.T) {
	for _, decision := range []string{"decline", "approve"} {
		t.Run(decision, func(t *testing.T) {
			app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
			app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner")
			var calls atomic.Int32
			started := make(chan struct{})
			action := app.actions["empty_trash"]
			action.Call = func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
				calls.Add(1)
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			app.actions["empty_trash"] = action
			run := chat.Run{ID: chat.NewID(), State: "awaiting_confirmation", Options: chat.Options{Corpus: c.Options.Corpus, Connection: "local", Model: "synthetic", Mode: "edit"}, Operations: []chat.Operation{{ID: "decision", ToolCallID: "call", Name: "empty_trash", State: "awaiting_confirmation", Confirmation: "exact", Arguments: json.RawMessage(`{"project":"default","revision":"synthetic","confirmation":"manifest"}`)}}}
			app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
				c.Turns = append(c.Turns, chat.Turn{ID: "origin-" + run.ID, RunID: run.ID, Role: "user", Content: "Explicit fixture task"})
				c.Runs = append(c.Runs, run)
				return nil
			})
			response := make(chan int, 1)
			go func() {
				w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/actions/decision", `{"decision":"`+decision+`","confirmation":"exact"}`)
				response <- w.Code
			}()
			if decision == "approve" {
				select {
				case <-started:
				case <-time.After(time.Second):
					t.Fatal("approved action did not start")
				}
				w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/runs/"+run.ID+"/cancel", `{}`)
				if w.Code != 200 {
					t.Fatal(w.Body)
				}
			}
			select {
			case status := <-response:
				if decision == "decline" && status != 200 || decision == "approve" && status != 409 {
					t.Fatalf("decision status %d", status)
				}
			case <-time.After(time.Second):
				t.Fatal("explicit decision ignored cancellation")
			}
			got, _ := app.chat.Store.Read("alice", "default", c.ID)
			finished := got.Runs[0]
			if chat.Active(finished.State) || finished.Operations[0].Confirmation != "" {
				t.Fatal("consumed decision remains unrecoverably active")
			}
			if decision == "decline" && (calls.Load() != 0 || finished.Operations[0].State != "completed") {
				t.Fatal("decline dispatched or remained unresolved")
			}
		})
	}
}

func TestChatInterruptedTranscriptKeepsOrderAndCanSavePartial(t *testing.T) {
	app := chatFixture(t)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	c, err := app.chat.Store.Create("alice", "default", chat.Options{Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		first := chat.Turn{ID: "question", RunID: "first", Role: "user", Content: "First question"}
		c.Turns = append(c.Turns, first)
		c.AddEvent("first", "user", first)
		c.Runs = append(c.Runs, chat.Run{ID: "first", State: "interrupted", Citations: []chat.Citation{{ID: "source-1", URL: "/p/default/note?path=notes/example.md&revision=pinned", Kind: "committed"}}})
		c.AddEvent("first", "text_delta", map[string]string{"turnId": "partial", "text": "Recovered answer fragment"})
		second := chat.Turn{ID: "second-question", RunID: "second", Role: "user", Content: "Second question"}
		c.Turns = append(c.Turns, second)
		c.AddEvent("second", "user", second)
		c.Runs = append(c.Runs, chat.Run{ID: "second", State: "completed"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/p/default/api/chat/" + c.ID
	t.Run("order", func(t *testing.T) {
		w := proxyCall(app, "alice", "GET", path, "")
		var got struct {
			Turns      []chat.Turn `json:"turns"`
			TotalTurns int         `json:"totalTurns"`
		}
		if json.Unmarshal(w.Body.Bytes(), &got) != nil || w.Code != 200 {
			t.Fatal(w.Body)
		}
		if len(got.Turns) != 3 || got.Turns[1].ID != "partial" || got.TotalTurns != 3 || len(got.Turns[1].Citations) != 1 {
			t.Fatalf("interrupted transcript lost order/count/citations: %s", w.Body)
		}
	})
	t.Run("save", func(t *testing.T) {
		w := proxyCall(app, "alice", "POST", path+"/save-note", `{"submissionId":"save-partial","path":"notes/recovered-chat.md","revision":"","turnIds":["partial"]}`)
		if w.Code != 200 {
			t.Fatalf("recovered answer cannot be explicitly saved: %d %s", w.Code, w.Body)
		}
		store, _ := app.catalog.Access("alice", "default", "read")
		note, err := store.Read(t.Context(), "notes/recovered-chat.md", "")
		if err != nil || !strings.Contains(string(note.Content), "Recovered answer fragment") || !strings.Contains(string(note.Content), "revision=pinned") {
			t.Fatalf("saved partial lost text or provenance: %s %v", note.Content, err)
		}
	})
}
