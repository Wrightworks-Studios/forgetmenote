package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"net/http"
	"strings"
	"testing"
)

func TestShipBCommittedAttachmentInvalidatesAnswer(t *testing.T) {
	prompts := make(chan string, 2)
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		prompts <- string(body)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ATTACHED_CLAIM_OLD_LIMIT_90"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`)
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	store, _ := app.catalog.Access("alice", "default", "edit")
	if _, err := store.Save(t.Context(), "notes/alphalaunch.md", []byte("# Alphalaunch\nIndependent launch navigation."), "", ""); err != nil {
		t.Fatal(err)
	}
	attachment, err := store.Save(t.Context(), "notes/policy.md", []byte("# Policy\nThe limit is 90."), "", "")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "/p/default/api/chat/" + c.ID + "/turns"
	in := map[string]any{"submissionId": "first", "text": "alphalaunch", "options": chat.Options{Mode: "ask"}, "attachments": []chat.Attachment{{Kind: "committed", Path: attachment.Path, Revision: attachment.Revision}}}
	raw, _ := json.Marshal(in)
	if w := proxyCall(app, "alice", "POST", endpoint, string(raw)); w.Code != 200 {
		t.Fatal(w.Body)
	}
	done := waitChatRun(t, app, c.ID)
	app.chat.Wait()
	firstPrompt := <-prompts
	if !strings.Contains(firstPrompt, "The limit is 90") {
		t.Fatal("fixture attachment missing")
	}
	if len(done.Runs[0].Citations) != 1 || done.Runs[0].Citations[0].Path != "notes/alphalaunch.md" {
		t.Fatalf("fixture needs independent retrieval: %+v", done.Runs[0].Citations)
	}
	if _, err = store.Save(t.Context(), attachment.Path, []byte("# Policy\nThe limit is now 5."), attachment.Revision, ""); err != nil {
		t.Fatal(err)
	}
	if w := proxyCall(app, "alice", "POST", endpoint, `{"submissionId":"second","text":"alphalaunch","options":{"mode":"ask"}}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	done = waitChatRun(t, app, c.ID)
	app.chat.Wait()
	nextPrompt := <-prompts
	if strings.Contains(nextPrompt, "ATTACHED_CLAIM_OLD_LIMIT_90") {
		t.Fatalf("changed attachment's old answer is reused in provider context; first answer provenance=%+v citations=%+v", done.Turns[1].Provenance, done.Turns[1].Citations)
	}
}

func TestShipBReviewComparisonChangeInvalidatesCachedAnalysis(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	store, _ := app.catalog.Access("alice", "default", "edit")
	first, err := store.Save(t.Context(), "notes/a.md", []byte("# Alphalaunch\nThe alphalaunch budget is 90."), "", "")
	if err != nil {
		t.Fatal(err)
	}
	comparison, err := store.Save(t.Context(), "notes/b.md", []byte("# Alphalaunch comparison\nFor the other team the alphalaunch budget is also 90."), "", "")
	if err != nil {
		t.Fatal(err)
	}
	runID := chat.NewID()
	if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Runs = append(c.Runs, chat.Run{ID: runID, State: "running", Options: chat.Options{Corpus: c.Options.Corpus, Mode: "ask", Review: true, Connection: "local", Model: "synthetic"}})
		c.Turns = append(c.Turns, chat.Turn{ID: chat.NewID(), RunID: runID, Role: "user", Content: "Compare alphalaunch budgets for contradictions"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	auth := chat.RunAuth{User: "alice", Project: "default", Epoch: app.catalog.Epoch("alice")}
	conn := app.chat.Config.Connections[0]
	if _, err = app.prepareChatReview(t.Context(), auth, c.ID, runID, conn); err != nil {
		t.Fatal(err)
	}
	saved, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	run := saved.Run(runID)
	if run.Review.BatchPath != first.Path {
		t.Fatalf("unexpected primary %s", run.Review.BatchPath)
	}
	sawComparison := false
	for _, cit := range run.Citations {
		if cit.Path == comparison.Path {
			sawComparison = true
		}
	}
	if !sawComparison {
		t.Fatal("fixture did not supply comparison original")
	}
	if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		finishChatReviewBatch(c.Run(runID), "A and B agree on budget 90.")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(t.Context(), comparison.Path, []byte("# Alphalaunch\nThe alphalaunch budget is now 5, contradicting 90."), comparison.Revision, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = app.prepareChatReview(t.Context(), auth, c.ID, runID, conn); err != nil {
		t.Fatal(err)
	}
	saved, err = app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range saved.Run(runID).Review.Notes {
		if n.Path == first.Path && n.State == "examined" {
			t.Fatalf("analysis using changed comparison remains examined: %+v", n)
		}
	}
}
