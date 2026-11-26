package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
)

// A session revoked while the model answers keeps the answer out of the
// conversation: the run rechecks its access before it records the reply.
func TestChatAnswerRevokedDuringTheModelCallIsNotRecorded(t *testing.T) {
	var server atomic.Pointer[Server]
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if err := server.Load().catalog.Revoke(catalog.Bootstrap, "alice"); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Answer written after revocation."},"finish_reason":"stop"}]}`)
	})
	server.Store(app)
	if w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"revoked-answer","text":"Summarize the notebook.","options":{"mode":"ask"}}`); w.Code != 200 {
		t.Fatalf("turn: %d %s", w.Code, w.Body)
	}
	got := waitChatRun(t, app, c.ID)
	for _, turn := range got.Turns {
		if turn.Role == "assistant" {
			t.Fatalf("answer recorded after revocation: %q", turn.Content)
		}
	}
}

// A session revoked while research fetches a source keeps the source out of
// the run: the run rechecks its access before it records the fetched text.
func TestChatResearchSourceRevokedDuringTheFetchIsNotRecorded(t *testing.T) {
	app, c, _ := researchFetchFixture(t, `research_fetch {"url":"https://example.org/source.txt"}`)
	app.chat.Mu.Lock()
	prepare := app.chat.PrepareSource
	app.chat.PrepareSource = func(ctx context.Context, filename, originalURL string, data []byte) (workspace.SourceCapture, error) {
		if err := app.catalog.Revoke(catalog.Bootstrap, "alice"); err != nil {
			t.Error(err)
		}
		return prepare(ctx, filename, originalURL, data)
	}
	app.chat.Mu.Unlock()
	run := startResearchTurn(t, app, c, "Compare the notebook with https://example.org/source.txt please.")
	if run.State == "completed" {
		t.Fatalf("revoked research run completed")
	}
	for _, citation := range run.Citations {
		if citation.Kind == "external" {
			t.Fatalf("source recorded after revocation: %+v", citation)
		}
	}
	for _, message := range run.Messages {
		if strings.Contains(message.Content, "Synthetic public original") {
			t.Fatalf("source text recorded after revocation in a %s message", message.Role)
		}
	}
}

// A session revoked while Jev judges a citation keeps the judgment out of the
// check: each judgment rechecks the run's access before it is saved.
func TestJevCitationJudgmentRevokedDuringTheCallIsNotSaved(t *testing.T) {
	app := chatFixture(t)
	configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) {
		if err := app.catalog.Revoke(catalog.Bootstrap, "alice"); err != nil {
			t.Error(err)
		}
		syntheticCitationAnswer(w, r)
	})
	c, path := citationConversation(t, app, "Every batch requires cooling before packing. [Process](SOURCE)")
	if w := proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":true,"rerank":true,"citations":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := proxyCall(app, "alice", "POST", path, `{}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var record jevCitationRecord
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var pending bool
		var err error
		record, pending, err = app.citations.Snapshot("alice", "default", c.ID, "run")
		if err != nil {
			t.Fatal(err)
		}
		if !pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("citation check did not finish")
		}
	}
	for _, check := range record.Checks {
		for _, item := range check.Items {
			if item.Status == "supported" {
				t.Fatalf("judgment saved after revocation: %+v", check)
			}
		}
	}
}

// rerankingRunner is a chat run fixture whose initial retrieval and passage
// search are ranked by a synthetic Jev. jev runs inside each Jev call before
// it answers; model answers each model request.
func rerankingRunner(t *testing.T, jev func(), model http.HandlerFunc) (*Server, chat.Conversation, *atomic.Int32) {
	t.Helper()
	app, c := runnerFixture(t, model)
	var jevCalls atomic.Int32
	score := scoreMaintenanceFirst(&jevCalls)
	configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) {
		jev()
		score(w, r)
	})
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		"a.md": "# Record\nBicycle workshop bicycle workshop bicycle workshop attendance.",
		"z.md": "# Maintenance\nBicycle workshop chains require CERULEAN lubricant.",
	} {
		if _, err := store.Save(context.Background(), path, []byte(content), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if w := proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":true,"rerank":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	return app, c, &jevCalls
}

// A session revoked while Jev ranks a run's initial evidence stops the run
// before the model sees that evidence.
func TestChatInitialEvidenceRevokedDuringTheJevRankingIsNotSent(t *testing.T) {
	var server atomic.Pointer[Server]
	var models atomic.Int32
	app, c, jevCalls := rerankingRunner(t, func() {
		if err := server.Load().catalog.Revoke(catalog.Bootstrap, "alice"); err != nil {
			t.Error(err)
		}
	}, func(w http.ResponseWriter, r *http.Request) {
		models.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Answer from ranked evidence."},"finish_reason":"stop"}]}`)
	})
	server.Store(app)
	if w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"revoked-ranking","text":"bicycle workshop chains","options":{"mode":"ask"}}`); w.Code != 200 {
		t.Fatalf("turn: %d %s", w.Code, w.Body)
	}
	got := waitChatRun(t, app, c.ID)
	if jevCalls.Load() == 0 {
		t.Fatal("the initial evidence was not ranked by Jev")
	}
	if models.Load() != 0 {
		t.Fatalf("model received evidence ranked after revocation (run %s)", got.Runs[len(got.Runs)-1].State)
	}
}

// toolCallAnswer answers a model request with one tool call.
func toolCallAnswer(w http.ResponseWriter, id, name, arguments string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]string{"name": name, "arguments": arguments}}}}, "finish_reason": "tool_calls"}}})
}

// recordedText reports whether any message, turn or event of the
// conversation's runs contains text.
func recordedText(c chat.Conversation, text string) bool {
	for _, run := range c.Runs {
		for _, m := range run.Messages {
			if strings.Contains(m.Content, text) {
				return true
			}
		}
	}
	for _, turn := range c.Turns {
		if strings.Contains(turn.Content, text) {
			return true
		}
	}
	for _, event := range c.Events {
		if strings.Contains(string(event.Data), text) {
			return true
		}
	}
	return false
}

// A session revoked while the model chooses a tool keeps the tool from running
// and its result out of the run.
func TestChatToolCallRevokedDuringTheModelCallDoesNotRun(t *testing.T) {
	saveMarker := func(t *testing.T, app *Server) {
		t.Helper()
		store, err := app.catalog.Access(catalog.Bootstrap, "default", "edit")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Save(context.Background(), "z.md", []byte("# Maintenance\nBicycle workshop chains require CERULEAN lubricant."), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	revoke := func(t *testing.T, app *Server) {
		if err := app.catalog.Revoke(catalog.Bootstrap, "alice"); err != nil {
			t.Error(err)
		}
	}
	turn := func(t *testing.T, app *Server, c chat.Conversation, body string) chat.Conversation {
		t.Helper()
		if w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", body); w.Code != 200 {
			t.Fatalf("turn: %d %s", w.Code, w.Body)
		}
		return waitChatRun(t, app, c.ID)
	}
	t.Run("research fetch", func(t *testing.T) {
		var server atomic.Pointer[Server]
		app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
			revoke(t, server.Load())
			toolCallAnswer(w, "call-1", "research_fetch", `{"url":"https://example.org/source.txt"}`)
		})
		server.Store(app)
		fetched := enableSyntheticResearch(t, app)
		got := turn(t, app, c, `{"submissionId":"revoked-fetch","text":"Compare the notebook with https://example.org/source.txt please.","options":{"mode":"ask","research":true}}`)
		if urls := fetched(); len(urls) != 0 || recordedText(got, "Synthetic public original") {
			t.Fatalf("source fetched after revocation: %v", urls)
		}
	})
	t.Run("passage search", func(t *testing.T) {
		var server atomic.Pointer[Server]
		app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
			revoke(t, server.Load())
			toolCallAnswer(w, "call-1", "search_passages", `{"text":"chains lubricant"}`)
		})
		server.Store(app)
		saveMarker(t, app)
		got := turn(t, app, c, `{"submissionId":"revoked-search","text":"What do the notes say about weather?","options":{"mode":"ask"}}`)
		if recordedText(got, "CERULEAN") {
			t.Fatal("passages searched after revocation were recorded")
		}
	})
	t.Run("sibling read over the tool budget", func(t *testing.T) {
		var server atomic.Pointer[Server]
		var requests atomic.Int32
		app, c := runnerFixtureLimits(t, func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Tools []json.RawMessage `json:"tools"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			n := requests.Add(1)
			if len(body.Tools) == 0 {
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Final answer."},"finish_reason":"stop"}]}`)
				return
			}
			calls := []any{map[string]any{"id": fmt.Sprint("call-", n, "-0"), "type": "function", "function": map[string]string{"name": "search_passages", "arguments": `{"text":"weather"}`}}}
			if n > 1 {
				// The second sibling exceeds the remaining budget and is omitted.
				revoke(t, server.Load())
				calls = append(calls, map[string]any{"id": fmt.Sprint("call-", n, "-1"), "type": "function", "function": map[string]string{"name": "search_passages", "arguments": `{"text":"weather"}`}})
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": calls}, "finish_reason": "tool_calls"}}})
		}, map[string]int{"runSeconds": 10, "maxTools": 2})
		server.Store(app)
		got := turn(t, app, c, `{"submissionId":"revoked-budget","text":"What do the notes say about weather?","options":{"mode":"ask"}}`)
		if requests.Load() < 2 {
			t.Fatalf("model requests: %d", requests.Load())
		}
		for _, op := range got.Runs[len(got.Runs)-1].Operations {
			if op.State == "omitted" {
				t.Fatalf("an omitted read was recorded after revocation: %+v", op)
			}
		}
	})
	editor := func(t *testing.T, app *Server) {
		t.Helper()
		if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
			t.Fatal(err)
		}
	}
	const editTurn = `{"submissionId":"revoked-edit","text":"Update the notebook.","options":{"mode":"edit","actions":["notes"]}}`
	t.Run("note write", func(t *testing.T) {
		var server atomic.Pointer[Server]
		app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
			revoke(t, server.Load())
			toolCallAnswer(w, "call-1", "submit_change_group", `{"mode":"apply","group":{"id":"x","summary":"Summary","edits":[{"path":"saved.md","content":"# Saved\nWritten after revocation.\n","revision":"","hash":""}]}}`)
		})
		server.Store(app)
		editor(t, app)
		turn(t, app, c, editTurn)
		store, err := app.catalog.Access(catalog.Bootstrap, "default", "read")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Read(context.Background(), "saved.md", ""); err == nil {
			t.Fatal("a note was written after revocation")
		}
	})
	t.Run("approval offer", func(t *testing.T) {
		var server atomic.Pointer[Server]
		var requests atomic.Int32
		var archive atomic.Value
		app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
			if requests.Add(1) == 1 {
				toolCallAnswer(w, "call-1", "discover_actions", `{"names":["set_note_container"]}`)
				return
			}
			revoke(t, server.Load())
			toolCallAnswer(w, "call-2", "set_note_container", archive.Load().(string))
		})
		server.Store(app)
		editor(t, app)
		store, err := app.catalog.Access("alice", "default", "edit")
		if err != nil {
			t.Fatal(err)
		}
		note, err := store.Save(t.Context(), "notes/archive.md", []byte("# Archive me"), "", "alice")
		if err != nil {
			t.Fatal(err)
		}
		arguments, _ := json.Marshal(map[string]string{"path": note.Path, "revision": note.Revision, "container": "archive"})
		archive.Store(string(arguments))
		if w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", editTurn); w.Code != 200 {
			t.Fatalf("turn: %d %s", w.Code, w.Body)
		}
		got := waitChatRunOrApproval(t, app, c.ID)
		if requests.Load() < 2 {
			t.Fatalf("model requests: %d", requests.Load())
		}
		run := got.Runs[len(got.Runs)-1]
		for _, op := range run.Operations {
			if op.State == "awaiting_confirmation" {
				t.Fatalf("an action was offered for approval after revocation: %+v", op)
			}
		}
	})
}

// A session revoked while Jev ranks a passage search keeps the ranked passages
// out of the run.
func TestChatPassageSearchRevokedDuringTheJevRankingIsNotRecorded(t *testing.T) {
	var server atomic.Pointer[Server]
	var armed atomic.Bool
	app, c, jevCalls := rerankingRunner(t, func() {
		if armed.Load() {
			if err := server.Load().catalog.Revoke(catalog.Bootstrap, "alice"); err != nil {
				t.Error(err)
			}
		}
	}, func(w http.ResponseWriter, r *http.Request) {
		armed.Store(true)
		toolCallAnswer(w, "call-1", "search_passages", `{"text":"bicycle workshop chains"}`)
	})
	server.Store(app)
	if w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"revoked-search-ranking","text":"weather report","options":{"mode":"ask"}}`); w.Code != 200 {
		t.Fatalf("turn: %d %s", w.Code, w.Body)
	}
	got := waitChatRun(t, app, c.ID)
	if jevCalls.Load() == 0 {
		t.Fatal("the passage search was not ranked by Jev")
	}
	if recordedText(got, "CERULEAN") {
		t.Fatal("passages ranked after revocation were recorded")
	}
}

// crossNotebookEdit runs an Edit-mode turn whose corpus also reaches another
// notebook the person may read. The model makes each call, "name arguments",
// in turn, and the conversation is returned once the run settles or pauses.
func crossNotebookEdit(t *testing.T, calls ...string) (*Server, chat.Conversation) {
	t.Helper()
	var requests atomic.Int32
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if n := int(requests.Add(1)); n <= len(calls) {
			name, arguments, _ := strings.Cut(calls[n-1], " ")
			toolCallAnswer(w, fmt.Sprint("call-", n), name, arguments)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}]}`)
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Create(catalog.Bootstrap, "other", "Other"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "other", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	if w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"cross-notebook-edit","text":"Use the other notebook to update this one.","options":{"mode":"edit","actions":["notes"],"corpus":{"version":1,"archives":"exclude","drafts":"exclude","history":false,"noteProjects":["other"]}}}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	return app, waitChatRunOrApproval(t, app, c.ID)
}

// waitChatRunOrApproval returns the conversation once its last run settles or
// pauses for an approval.
func waitChatRunOrApproval(t *testing.T, app *Server, id string) chat.Conversation {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		got, err := app.chat.Store.Read("alice", "default", id)
		if err != nil {
			t.Fatal(err)
		}
		if run := got.Runs[len(got.Runs)-1]; !chat.Active(run.State) || run.State == "awaiting_confirmation" {
			app.chat.Wait()
			return got
		}
	}
	t.Fatal("chat run did not settle or pause")
	return chat.Conversation{}
}

// A note change the model chooses in a run whose corpus reaches another
// notebook is refused by the chat guard; nothing is written.
func TestChatCrossNotebookRunCannotWriteNotes(t *testing.T) {
	app, got := crossNotebookEdit(t, `submit_change_group {"mode":"apply","group":{"id":"x","summary":"Summary","edits":[{"path":"saved.md","content":"# Saved\nFrom another notebook.\n","revision":"","hash":""}]}}`)
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "read")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(context.Background(), "saved.md", ""); err == nil {
		t.Fatal("a cross-notebook run wrote a note")
	}
	run := got.Runs[len(got.Runs)-1]
	if run.State == "awaiting_confirmation" {
		t.Fatal("a cross-notebook change was offered for approval")
	}
	if !recordedText(got, "cross-notebook source context is read-only") {
		t.Fatalf("the refusal was not recorded: state=%s error=%q", run.State, run.Error)
	}
}

// A control the model chooses in a run whose corpus reaches another notebook
// is not offered for approval.
func TestChatCrossNotebookRunDoesNotOfferControls(t *testing.T) {
	_, got := crossNotebookEdit(t, `discover_actions {"names":["duplicate_notes"]}`, `duplicate_notes {"project":"default","from":"a.md","to":"copy.md","revision":"synthetic"}`)
	run := got.Runs[len(got.Runs)-1]
	if run.State == "awaiting_confirmation" {
		t.Fatal("a cross-notebook control was offered for approval")
	}
	for _, op := range run.Operations {
		if op.State == "awaiting_confirmation" {
			t.Fatalf("a cross-notebook control awaits approval: %+v", op)
		}
	}
	if !recordedText(got, "cross-notebook source context is read-only") {
		t.Fatalf("the refusal was not recorded: state=%s error=%q", run.State, run.Error)
	}
}

// The dispatcher's chat guard refuses a change carrying personal context unless
// the person approved it. In a chat run the executor already turns such a change
// into an approval before dispatch; this calls the dispatcher directly, so the
// guard is the only check that can refuse.
func TestChatDispatchRefusesAnUnapprovedPersonalChange(t *testing.T) {
	app := personalWriteFixture(t)
	options := chat.Options{Connection: "local", Model: "synthetic", Mode: "edit", Actions: []string{"notes"}, Corpus: &chat.Corpus{Version: 1, Archives: "exclude", Drafts: "exclude"}}
	c, err := app.chat.Store.Create("alice", "default", options)
	if err != nil {
		t.Fatal(err)
	}
	auth := chat.RunAuth{User: "alice", Project: "default", Epoch: app.catalog.Epoch("alice")}
	if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Runs = append(c.Runs, chat.Run{ID: "run", State: "running", Options: options, Epoch: auth.Epoch, PersonalConsent: "synthetic-consent"})
		c.Turns = append(c.Turns, chat.Turn{ID: "user-turn", RunID: "run", Role: "user", Content: "Save my email to a note."})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if c, err = app.chat.Store.Read("alice", "default", c.ID); err != nil {
		t.Fatal(err)
	}
	ctx, err := app.runContext(auth)
	if err != nil {
		t.Fatal(err)
	}
	if ctx, err = chat.WithRunScope(ctx, c, *c.Run("run")); err != nil {
		t.Fatal(err)
	}
	change := func(path string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"project": "default", "mode": "apply", "group": map[string]any{"id": path, "summary": "Summary", "edits": []any{map[string]string{"path": path, "content": "# Leak\nPRIVATE_EMAIL_BODY\n", "revision": "", "hash": ""}}}})
		return raw
	}
	store, _ := app.catalog.Access(catalog.Bootstrap, "default", "read")
	if _, err := app.dispatchActionOutcome(ctx, "default", "submit_change_group", change("leak.md")); !errors.Is(err, errChatPersonalApproval) {
		t.Fatalf("unapproved personal change: %v", err)
	}
	if _, err := store.Read(ctx, "leak.md", ""); err == nil {
		t.Fatal("unapproved personal change was committed")
	}
	if _, err := app.dispatchActionOutcome(chat.WithApproval(ctx), "default", "submit_change_group", change("approved.md")); err != nil {
		t.Fatalf("approved personal change: %v", err)
	}
}

// A session revoked while the model examines a review batch keeps that batch
// unrecorded and stops the review before the next batch is sent.
func TestChatReviewRevokedDuringABatchStops(t *testing.T) {
	var server atomic.Pointer[Server]
	var requests atomic.Int32
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			if err := server.Load().catalog.Revoke(catalog.Bootstrap, "alice"); err != nil {
				t.Error(err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Examined supplied original passages. No unsupported claims."},"finish_reason":"stop"}]}`)
	})
	server.Store(app)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if _, err := store.Save(context.Background(), "notes/"+name+".md", []byte("# "+name+"\nDistinct original evidence "+name+"."), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"revoked-review","text":"Review every note and compare original evidence","options":{"mode":"ask","review":true}}`); w.Code != 200 {
		t.Fatalf("turn: %d %s", w.Code, w.Body)
	}
	got := waitChatRun(t, app, c.ID)
	if n := requests.Load(); n != 1 {
		t.Fatalf("review sent %d batches after revocation", n)
	}
	if review := got.Runs[len(got.Runs)-1].Review; review != nil {
		for _, note := range review.Notes {
			if note.State == "examined" {
				t.Fatalf("batch examined after revocation was recorded: %s", note.Path)
			}
		}
	}
}

// revokingBody is a request body that revokes alice's sessions after its first
// half is read, while the handler is still reading the request.
func revokingBody(t *testing.T, app *Server, body string) io.Reader {
	rest := strings.NewReader(body[len(body)/2:])
	var once sync.Once
	return io.MultiReader(strings.NewReader(body[:len(body)/2]), readerFunc(func(p []byte) (int, error) {
		once.Do(func() {
			if err := app.catalog.Revoke(catalog.Bootstrap, "alice"); err != nil {
				t.Error(err)
			}
		})
		return rest.Read(p)
	}))
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// sessionCall sends one browser request with a fresh session cookie for alice.
func sessionCall(t *testing.T, app *Server, method, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	login := httptest.NewRecorder()
	if err := app.createSession(login, "alice"); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, app.origin+path, body)
	r.Header.Set("Origin", app.origin)
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(login.Result().Cookies()[0])
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	return w
}

// countingModel answers every request and counts them.
func countingModel(calls *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Answer for a revoked session."},"finish_reason":"stop"}]}`)
	}
}

// A session revoked while a turn's request is still being read never starts
// the run: the turn checks the run's access once it has the whole request,
// before the model or Jev sees anything.
func TestChatTurnRevokedWhileItsRequestIsReadDoesNotStart(t *testing.T) {
	start := func(t *testing.T, app *Server, c chat.Conversation) chat.Conversation {
		t.Helper()
		body := `{"submissionId":"revoked-request","text":"bicycle workshop chains","options":{"mode":"ask"}}`
		if w := sessionCall(t, app, "POST", "/p/default/api/chat/"+c.ID+"/turns", revokingBody(t, app, body)); w.Code != http.StatusForbidden {
			t.Errorf("turn: %d %s", w.Code, w.Body)
		}
		app.chat.Wait()
		got, err := app.chat.Store.Read("alice", "default", c.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	t.Run("model", func(t *testing.T) {
		var calls atomic.Int32
		app, c := runnerFixture(t, countingModel(&calls))
		if got := start(t, app, c); len(got.Runs) != 0 || calls.Load() != 0 {
			t.Fatalf("run started after revocation: %d runs, %d model calls", len(got.Runs), calls.Load())
		}
	})
	t.Run("Jev ranking", func(t *testing.T) {
		var calls atomic.Int32
		app, c, jevCalls := rerankingRunner(t, func() {}, countingModel(&calls))
		if got := start(t, app, c); len(got.Runs) != 0 || jevCalls.Load() != 0 || calls.Load() != 0 {
			t.Fatalf("run started after revocation: %d runs, %d Jev calls, %d model calls", len(got.Runs), jevCalls.Load(), calls.Load())
		}
	})
}

// A session revoked while a resume request is still being read never resumes
// the run.
func TestChatResumeRevokedWhileItsRequestIsReadDoesNotRun(t *testing.T) {
	var calls atomic.Int32
	app, c := runnerFixture(t, countingModel(&calls))
	runID := chat.NewID()
	if err := app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Runs = append(c.Runs, chat.Run{ID: runID, State: "failed", Options: c.Options, Epoch: app.catalog.Epoch("alice")})
		c.Turns = append(c.Turns, chat.Turn{ID: "resumed-task", RunID: runID, Role: "user", Content: "Summarize the notebook."})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w := sessionCall(t, app, "POST", "/p/default/api/chat/"+c.ID+"/runs/"+runID+"/resume", revokingBody(t, app, `{"budgetTokens":0}`)); w.Code != http.StatusForbidden {
		t.Errorf("resume: %d %s", w.Code, w.Body)
	}
	app.chat.Wait()
	got, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state := got.Run(runID).State; state != "failed" || calls.Load() != 0 {
		t.Fatalf("run resumed after revocation: state %s, %d model calls", state, calls.Load())
	}
}

// A session revoked while an approved action runs, when the action's outcome
// is unknown, gets a refusal instead of the uncertain-outcome reply.
func TestChatApprovedActionWithAnUnknownOutcomeAfterRevocationIsRefused(t *testing.T) {
	var calls atomic.Int32
	app, c := runnerFixture(t, countingModel(&calls))
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	store, err := app.catalog.Access("alice", "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	note, err := store.Save(t.Context(), "notes/approved.md", []byte("# Approved"), "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := app.chat.Config.Resolve("local", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	options := c.Options
	options.Mode = "edit"
	options.Actions = []string{"notes"}
	auth := chat.RunAuth{User: "alice", Project: "default", Epoch: app.catalog.Epoch("alice")}
	arguments, _ := json.Marshal(map[string]string{"path": note.Path, "revision": note.Revision, "container": "archive"})
	call := chat.ToolCall{ID: "archive", Name: "set_note_container", Arguments: arguments}
	run := chat.Run{ID: chat.NewID(), State: "running", Epoch: auth.Epoch, Options: options, Rounds: 1, RoundLimit: connection.Limits.MaxRounds, ToolLimit: connection.Limits.MaxTools, Messages: []chat.Message{{Role: "user", Content: "Archive the note."}, {Role: "assistant", ToolCalls: []chat.ToolCall{call}}}}
	if err = app.chat.Store.Update("alice", "default", c.ID, func(saved *chat.Conversation) error {
		saved.Turns = append(saved.Turns, chat.Turn{ID: chat.NewID(), RunID: run.ID, Role: "user", Content: "Archive the note."})
		saved.Runs = append(saved.Runs, run)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, err := app.runContext(auth)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.executeNotebookTool(ctx, auth, c.ID, run.ID, call, connection); !errors.Is(err, errChatConfirmation) {
		t.Fatalf("approval not prepared: %v", err)
	}
	prepared, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	operation := prepared.Run(run.ID).Operations[0]
	action := app.actions[call.Name]
	action.Call = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		if err := app.catalog.Revoke(catalog.Bootstrap, "alice"); err != nil {
			return nil, err
		}
		return nil, context.Canceled
	}
	app.actions[call.Name] = action
	approval, _ := json.Marshal(map[string]string{"decision": "approve", "confirmation": operation.Confirmation})
	w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/actions/"+operation.ID, string(approval))
	app.chat.Wait()
	if w.Code != http.StatusForbidden {
		t.Fatalf("approval after revocation: %d %s", w.Code, w.Body)
	}
}

// A session revoked while a save-to-note request is still being read writes
// nothing: Edit access is unchanged, so only the run's session check refuses.
func TestChatSaveNoteRevokedWhileItsRequestIsReadIsNotWritten(t *testing.T) {
	app := chatFixture(t)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	c, err := app.chat.Store.Create("alice", "default", chat.Options{Connection: "local", Model: "synthetic", Mode: "ask", Corpus: &chat.Corpus{Version: 1, Archives: "exclude", Drafts: "exclude"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Turns = append(c.Turns, chat.Turn{ID: "answer", Role: "assistant", Content: "Evidence worth saving."})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	body := `{"submissionId":"save-revoked","path":"notes/saved.md","revision":"","turnIds":["answer"]}`
	w := sessionCall(t, app, "POST", "/p/default/api/chat/"+c.ID+"/save-note", revokingBody(t, app, body))
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "read")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(t.Context(), "notes/saved.md", ""); err == nil || w.Code != http.StatusForbidden {
		t.Fatalf("revoked session saved a note: %d %s", w.Code, w.Body)
	}
}
