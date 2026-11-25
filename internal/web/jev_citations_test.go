package web

import (
	"context"
	"encoding/json"
	"errors"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/pg/pgtest"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func configureSyntheticJev(t *testing.T, app *Server, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	previous := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() {
		app.Close()
		http.DefaultTransport = previous
	})
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte("synthetic-key"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := *app.chat.Config
	cfg.Jev = &decision.Config{Enabled: true, Model: "jev-1.13.0", Endpoint: server.URL + "/v1/systemone", SecretFile: key}
	raw, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "chat.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureChat(path); err != nil {
		t.Fatal(err)
	}
}

func syntheticCitationAnswer(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Questions map[string]decision.Question `json:"questions"`
	}
	if json.NewDecoder(r.Body).Decode(&request) != nil {
		http.Error(w, "fixture", 500)
		return
	}
	answers := map[string]any{}
	for id, q := range request.Questions {
		probabilities := map[string]float64{}
		for option := range q.Criteria.(map[string]any) {
			probabilities[option] = 0
		}
		probabilities["supported"] = 1
		answers[id] = map[string]any{"type": "choice", "choice": "supported", "confidence": 1, "probabilities": probabilities}
	}
	json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 60, "output_tokens": 20}})
}

func citationConversation(t *testing.T, app *Server, answer string) (chat.Conversation, string) {
	t.Helper()
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := store.Save(context.Background(), "notes/evidence.md", []byte("# Process\nEvery batch requires cooling before packing."), "", "operator")
	if err != nil {
		t.Fatal(err)
	}
	c, err := app.chat.Store.Create("alice", "default", chat.Options{Connection: "local", Model: "synthetic", Mode: "ask", Corpus: &chat.Corpus{Version: 1, Archives: "lower", Drafts: "include"}})
	if err != nil {
		t.Fatal(err)
	}
	url := "/notes/evidence.md?fmn-rev=" + doc.Revision
	citation := chat.Citation{ID: "evidence", Path: doc.Path, Revision: doc.Revision, Hash: chat.Hash(doc.Content), Text: string(doc.Content), URL: url, Kind: "committed"}
	answer = strings.ReplaceAll(answer, "SOURCE", url)
	if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Runs = append(c.Runs, chat.Run{ID: "run", State: "completed", Options: c.Options, Epoch: app.catalog.Epoch("alice"), Citations: []chat.Citation{citation}})
		c.Turns = append(c.Turns, chat.Turn{ID: "user-turn", RunID: "run", Role: "user", Content: "What is the process?"}, chat.Turn{ID: "answer-turn", RunID: "run", Role: "assistant", Content: answer, Citations: []chat.Citation{citation}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c, err = app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c, "/p/default/api/chat/" + c.ID + "/runs/run/citations"
}

func TestCitationDeletionDoesNotRecreateLateSidecar(t *testing.T) {
	app := chatFixture(t)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	configureSyntheticJev(t, app, syntheticCitationAnswer)
	c, path := citationConversation(t, app, "Answer without a provider call.")
	if w := proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":true,"rerank":true,"citations":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w := proxyCall(app, "alice", "POST", path, "{}"); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	check := waitCitation(t, app, path)
	deadline := time.Now().Add(time.Second)
	for app.citations.Active() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("citation teardown did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	w := proxyCall(app, "alice", "DELETE", "/p/default/api/chat/"+c.ID, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	check.State = "completed"
	if err := app.saveCitationCheck("alice", "default", c.ID, "run", check); err == nil {
		t.Fatal("retired citation work recreated a deleted private sidecar")
	}
}

func waitCitation(t *testing.T, app *Server, path string) jevCitationCheck {
	t.Helper()
	parts := strings.Split(path, "/")
	if len(parts) != 9 || parts[1] != "p" || parts[4] != "chat" || parts[6] != "runs" {
		t.Fatal("invalid citation fixture path", path)
	}
	deadline := time.Now().Add(3 * time.Second)
	var status int
	var body string
	for time.Now().Before(deadline) {
		// Repeated HTTP reads revalidate Git-backed evidence. Let the bounded
		// worker finish before checking its result through that public boundary.
		_, pending, err := app.citations.Snapshot("alice", parts[2], parts[5], parts[7])
		if err != nil {
			t.Fatal(err)
		}
		if pending {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		w := proxyCall(app, "alice", "GET", path, "")
		status, body = w.Code, w.Body.String()
		if w.Code != 200 {
			t.Fatalf("read check: %d %s", w.Code, w.Body.String())
		}
		var check jevCitationCheck
		if err := json.Unmarshal(w.Body.Bytes(), &check); err != nil {
			t.Fatal(err)
		}
		if check.State != "pending" && check.State != "running" {
			return check
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("GET %s: citation check did not finish within 3s; last HTTP %d: %s", path, status, body)
	return jevCitationCheck{}
}

func TestJevCitationCheckUsesExplicitConsentAndPrivateSidecar(t *testing.T) {
	app := chatFixture(t)
	var calls atomic.Int32
	configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		syntheticCitationAnswer(w, r)
	})
	c, path := citationConversation(t, app, "Every batch requires cooling before packing. [Process](SOURCE)")
	if w := proxyCall(app, "alice", "POST", path, `{}`); w.Code == 200 {
		t.Fatal("allowed remote check without personal consent")
	}
	if calls.Load() != 0 {
		t.Fatal("sent private evidence without consent")
	}
	if w := proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":true,"rerank":true,"citations":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := proxyCall(app, "alice", "POST", path, `{}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	check := waitCitation(t, app, path)
	if check.State != "completed" || check.Checked != 1 || check.Items[0].Status != "supported" || calls.Load() != 1 {
		t.Fatalf("bad citation support: %+v calls %d", check, calls.Load())
	}
	if w := proxyCall(app, "bob", "GET", path, ""); w.Code != 404 {
		t.Fatal("another user accessed private claims", w.Code)
	}
	w := proxyCall(app, "alice", "DELETE", "/p/default/api/chat/"+c.ID, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := app.jevState.records.Read(citationRecordName("alice", c.ID), new(any)); !os.IsNotExist(err) {
		t.Fatal("deleted chat retained private claim sidecar", err)
	}
	records, err := app.chat.JevLedger.Records()
	if err != nil || len(records) != 1 || records[0].Usage == nil {
		t.Fatal("chat deletion removed paid usage")
	}
}

func TestJevCitationMappingAndQuoteFailuresStayUnchecked(t *testing.T) {
	app := chatFixture(t)
	var calls atomic.Int32
	configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		syntheticCitationAnswer(w, r)
	})
	_, path := citationConversation(t, app, "The process says \"skip cooling\". [Process](SOURCE)\n\nThe batch is cooled. [one](SOURCE) [two](SOURCE)")
	proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":true,"rerank":true,"citations":true}`)
	if w := proxyCall(app, "alice", "POST", path, `{}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	check := waitCitation(t, app, path)
	if check.State != "incomplete" || check.Total != 2 || check.Checked != 1 || check.Items[0].Status != "invalid_quote" || check.Items[1].Status != "unchecked" || calls.Load() != 0 {
		t.Fatalf("guessed ambiguous/invalid citation: %+v calls %d", check, calls.Load())
	}
}

func TestJevAutomaticCitationCompletionRequiresAllConsentGates(t *testing.T) {
	for _, test := range []struct {
		name                                    string
		operator, consent, citations, localOnly bool
		wantCalls                               int32
	}{
		{name: "enabled", operator: true, consent: true, citations: true, wantCalls: 1},
		{name: "operator disabled", consent: true, citations: true},
		{name: "no personal consent", operator: true, citations: true},
		{name: "citations disabled", operator: true, consent: true},
		{name: "local only", operator: true, consent: true, citations: true, localOnly: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := chatFixture(t)
			var calls atomic.Int32
			if test.operator {
				configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					syntheticCitationAnswer(w, r)
				})
			}
			c, path := citationConversation(t, app, "Every batch requires cooling before packing. [Process](SOURCE)")
			body, _ := json.Marshal(jevPreferences{Consent: test.consent, Rerank: true, Citations: test.citations})
			if w := proxyCall(app, "alice", "POST", "/api/jev/settings", string(body)); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			setChatFixtureLocalOnly(t, app, test.localOnly)
			auth := chat.RunAuth{User: "alice", Project: "default", Epoch: app.catalog.Epoch("alice")}
			app.chatTerminal(auth, c.ID, "run", "completed", nil)
			check := waitCitation(t, app, path)
			if calls.Load() != test.wantCalls {
				t.Fatalf("completion made %d calls, want %d; check %+v", calls.Load(), test.wantCalls, check)
			}
			if test.wantCalls > 0 && check.State != "completed" {
				t.Fatalf("automatic check missing: %+v", check)
			}
		})
	}
}

func TestJevCitationDiscardsJudgmentWhenSourceChangesDuringCall(t *testing.T) {
	app := chatFixture(t)
	configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) {
		store, err := app.catalog.Access(catalog.Bootstrap, "default", "edit")
		if err != nil {
			t.Error(err)
			return
		}
		doc, err := store.Read(context.Background(), "notes/evidence.md", "")
		if err != nil {
			t.Error(err)
			return
		}
		if _, err = store.Save(context.Background(), doc.Path, []byte("# Process\nThe cooling requirement has changed."), doc.Revision, "operator"); err != nil {
			t.Error(err)
		}
		syntheticCitationAnswer(w, r)
	})
	_, path := citationConversation(t, app, "Every batch requires cooling before packing. [Process](SOURCE)")
	proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":true,"rerank":true,"citations":true}`)
	if w := proxyCall(app, "alice", "POST", path, `{}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	check := waitCitation(t, app, path)
	if check.State == "completed" || check.Checked != 0 {
		t.Fatalf("published stale source judgment: %+v", check)
	}
}

// A conversation's deletion and its citation checks exclude each other on
// every gateway.
func TestConversationDeletionAndCitationChecksExcludeEachOtherAcrossGateways(t *testing.T) {
	app := chatFixture(t)
	app.ConfigureProxy(proxyTestSecret, []string{"127.0.0.1/32"})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	configureSyntheticJev(t, app, syntheticCitationAnswer)
	c, _ := citationConversation(t, app, "Answer without a provider call.")
	if w := proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":true,"rerank":true,"citations":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	// Another gateway checking the conversation's citations holds it.
	gateway, _ := pgtest.Gateway(t, app.db)
	held, err := gateway.Shared(t.Context(), conversationLock("alice", c.ID))
	if err != nil {
		t.Fatal(err)
	}
	if w := proxyCall(app, "alice", "DELETE", "/p/default/api/chat/"+c.ID, ""); w.Code != 409 {
		t.Fatal("a conversation was deleted during another gateway's citation check", w.Code, w.Body)
	}
	held()
	// Another gateway deletes it; a check that starts here afterwards finds
	// nothing to check and records nothing.
	if _, err = app.chat.Store.Delete("alice", "default", c.ID, nil); err != nil {
		t.Fatal(err)
	}
	auth := chat.RunAuth{User: "alice", Project: "default", Epoch: app.catalog.Epoch("alice")}
	if err = app.startCitationCheck(auth, c.ID, "run", true); !errors.Is(err, chat.ErrNotFound) {
		t.Fatal("a citation check started for a conversation deleted elsewhere", err)
	}
	if record, pending, err := app.citations.Snapshot("alice", "default", c.ID, "run"); err != nil || pending || len(record.Checks) != 0 {
		t.Fatal("a check was recorded for a deleted conversation", pending, len(record.Checks), err)
	}
}
