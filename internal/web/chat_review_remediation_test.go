package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
)

func TestChatOriginalEvidenceSurvivesReloadAndReferenceInvalidation(t *testing.T) {
	app := chatFixture(t)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	store, _ := app.catalog.Access("alice", "default", "edit")
	source, err := store.Save(t.Context(), "notes/source.md", []byte("# Original source\nBudget evidence A."), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(t.Context(), "notes/claim.md", []byte("# Claim\nClaim budget statement cites [original](source.md)."), "", ""); err != nil {
		t.Fatal(err)
	}
	c, err := app.chat.Store.Create("alice", "default", chat.Options{Corpus: &chat.Corpus{Version: 1, Archives: "exclude", Drafts: "exclude", History: false}, Mode: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	c.Turns = []chat.Turn{{ID: "question", Role: "user", Content: "Claim budget statement"}}
	conn := app.chat.Config.Connections[0]
	_, sources, memory, err := initialChatTestMessages(t, app, c, conn)
	if err != nil {
		t.Fatal(err)
	}
	var claim chat.Citation
	for _, citation := range sources {
		if citation.Path == "notes/claim.md" {
			claim = citation
		}
	}
	if claim.References == "" {
		t.Fatal("original reference dependencies not pinned")
	}
	c.Memory = memory
	c.Turns = append(c.Turns, chat.Turn{ID: "answer", Role: "assistant", Content: "ORIGINAL_CLAIM_MARKER", Provenance: memory.Provenance, Citations: []chat.Citation{claim}}, chat.Turn{ID: "followup", Role: "user", Content: "Check claim budget"})
	if err = app.chat.Store.Update("alice", "default", c.ID, func(saved *chat.Conversation) error { saved.Turns = c.Turns; saved.Memory = c.Memory; return nil }); err != nil {
		t.Fatal(err)
	}
	c, err = app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages, _, _, err := initialChatTestMessages(t, app, c, conn)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(messages)
	if !strings.Contains(string(b), "ORIGINAL_CLAIM_MARKER") {
		t.Fatal("unchanged original evidence was not reusable after reload")
	}
	if _, err = store.Save(t.Context(), "notes/source.md", []byte("# Changed source\nBudget evidence B supersedes A."), source.Revision, ""); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 3; round++ {
		c, err = app.chat.Store.Read("alice", "default", c.ID)
		if err != nil {
			t.Fatal(err)
		}
		messages, _, memory, err = initialChatTestMessages(t, app, c, conn)
		if err != nil {
			t.Fatal(err)
		}
		b, _ = json.Marshal(messages)
		if strings.Contains(string(b), "ORIGINAL_CLAIM_MARKER") {
			t.Fatalf("obsolete reference claim re-entered history on followup %d", round)
		}
		if err = app.chat.Store.Update("alice", "default", c.ID, func(saved *chat.Conversation) error {
			saved.Memory = memory
			for i := 0; i < 6; i++ {
				saved.Turns = append(saved.Turns, chat.Turn{ID: fmt.Sprintf("turn-%d-%d", round, i), Role: "user", Content: "Check current claim budget"})
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestChatRevokedPausedApprovalReconcilesAfterRestart(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"New permitted notebook"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":2}}`)
	})
	if err := app.catalog.Create(catalog.Bootstrap, "second", "Second"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "second", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	if err := app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Runs = append(c.Runs, chat.Run{ID: "paused", State: "awaiting_confirmation", Epoch: app.catalog.Epoch("alice"), Options: c.Options, Usage: chat.Accounting{InputTokens: 7, OutputTokens: 3, Requests: 1, Reserved: 500}, ReservationDay: chat.Day(), Operations: []chat.Operation{{ID: "approval", Name: "empty_trash", State: "awaiting_confirmation", Confirmation: "old-confirmation"}}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", ""); err != nil {
		t.Fatal(err)
	}
	app.chat.Store.Close()
	reopened, err := chat.OpenStore(app.db)
	if err != nil {
		t.Fatal(err)
	}
	app.chat.Store = reopened
	w := proxyCall(app, "alice", "POST", "/p/second/api/chat", `{"connection":"local","model":"synthetic","mode":"ask"}`)
	var second chat.Conversation
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &second) != nil {
		t.Fatal(w.Body)
	}
	w = proxyCall(app, "alice", "POST", "/p/second/api/chat/"+second.ID+"/turns", `{"submissionId":"second-after-restart","text":"Hello","options":{"mode":"ask"}}`)
	if w.Code != 200 {
		t.Fatalf("restored revoked approval blocks new permitted chat: %d %s", w.Code, w.Body)
	}
	old, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	run := old.Runs[0]
	if run.State != "cancelled" || run.Usage.Reserved != 0 || run.Usage.Tokens() != 10 || run.Usage.Requests != 1 || run.Operations[0].Confirmation != "" || run.Operations[0].State != "declined" {
		t.Fatalf("paused reconciliation lost usage or retained approval: %+v", run)
	}
	for _, suffix := range []string{"", "/events?after=0"} {
		if w := proxyCall(app, "alice", "GET", "/p/default/api/chat/"+c.ID+suffix, ""); w.Code != 403 {
			t.Fatalf("revoked chat disclosure: %d", w.Code)
		}
	}
	w = proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/actions/approval", `{"decision":"approve","confirmation":"old-confirmation"}`)
	if w.Code != 403 {
		t.Fatalf("revoked approval accessible after reconciliation: %d", w.Code)
	}
}

func TestChatReviewIncludesOnlyExplicitPrivateAttachment(t *testing.T) {
	captured := make(chan string, 1)
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		select {
		case captured <- string(body):
		default:
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Compared the supplied review evidence."},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`)
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	store, _ := app.catalog.Access("alice", "default", "edit")
	if _, err := store.Save(t.Context(), "notes/review.md", []byte("# Review source\nCommitted original evidence."), "", ""); err != nil {
		t.Fatal(err)
	}
	app.chat.Config.Connections[0].Limits.MaxRounds = 1
	w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"review-attached","text":"Review the notebook against my explicitly attached draft","options":{"mode":"ask","review":true},"attachments":[{"kind":"private","path":"notes/private.md","content":"EXPLICIT_PRIVATE_REVIEW_MARKER"}]}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	done := waitChatRun(t, app, c.ID)
	select {
	case body := <-captured:
		if !strings.Contains(body, "EXPLICIT_PRIVATE_REVIEW_MARKER") || !strings.Contains(body, "explicitAttachments") {
			t.Fatal("review silently ignored deliberately attached private evidence")
		}
	default:
		t.Fatal("no review request")
	}
	for _, note := range done.Runs[0].Review.Notes {
		if note.Path == "notes/private.md" {
			t.Fatal("private attachment entered committed inventory")
		}
	}
	if _, err := store.Read(t.Context(), "notes/private.md", ""); err == nil {
		t.Fatal("private attachment was written to notebook")
	}
}

func TestChatReviewCacheDistinguishesExplicitAttachments(t *testing.T) {
	var sawSecond atomic.Bool
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if strings.Contains(string(body), "SECOND_REVIEW_ATTACHMENT") {
			sawSecond.Store(true)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Reviewed this original passage with the explicit attachment."},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`)
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	store, _ := app.catalog.Access("alice", "default", "edit")
	if _, err := store.Save(t.Context(), "notes/review.md", []byte("# Original\nCompare this note against the draft."), "", ""); err != nil {
		t.Fatal(err)
	}
	for i, content := range []string{"FIRST_REVIEW_ATTACHMENT", "SECOND_REVIEW_ATTACHMENT"} {
		body, _ := json.Marshal(map[string]any{"submissionId": fmt.Sprintf("review-cache-%d", i), "text": "Review notes against my attached draft", "options": chat.Options{Mode: "ask", Review: true}, "attachments": []chat.Attachment{{Kind: "private", Path: "notes/private.md", Content: content}}})
		w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", string(body))
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
		done := waitChatRun(t, app, c.ID)
		if run := done.Runs[len(done.Runs)-1]; run.State != "completed" || run.Review == nil || !run.Review.Complete {
			t.Fatalf("review did not complete: %+v", run)
		}
	}
	if !sawSecond.Load() {
		t.Fatal("changed private attachment reused prior completed coverage without examining new review input")
	}
}
