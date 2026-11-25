package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

type reviewDependencyFixture struct {
	app          *Server
	store        *workspace.Store
	conversation string
	run          string
	auth         chat.RunAuth
	connection   chat.Connection
}

func newReviewDependencyFixture(t *testing.T, handlers ...http.HandlerFunc) reviewDependencyFixture {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	if len(handlers) > 0 {
		handler = handlers[0]
	}
	app, c := runnerFixture(t, handler)
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, note := range []struct{ path, content string }{
		{"notes/a.md", "# Alphalaunch\nThe alphalaunch budget is 90."},
		{"notes/b.md", "# Alphalaunch comparison\nThe alphalaunch budget is COMPARISON_OLD_90. [Supporting page](source.md)"},
		{"notes/source.md", "Supporting official report."},
	} {
		if _, err := store.Save(t.Context(), note.path, []byte(note.content), "", "operator"); err != nil {
			t.Fatal(err)
		}
	}
	runID := chat.NewID()
	if err = app.chat.Store.Update("alice", "default", c.ID, func(c *chat.Conversation) error {
		c.Runs = append(c.Runs, chat.Run{ID: runID, State: "running", Epoch: app.catalog.Epoch("alice"), Options: chat.Options{Corpus: c.Options.Corpus, Mode: "ask", Review: true, Connection: "local", Model: "synthetic"}})
		c.Turns = append(c.Turns, chat.Turn{ID: chat.NewID(), RunID: runID, Role: "user", Content: "Compare alphalaunch budgets"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return reviewDependencyFixture{app, store, c.ID, runID, chat.RunAuth{User: "alice", Project: "default", Epoch: app.catalog.Epoch("alice")}, app.chat.Config.Connections[0]}
}

func (f reviewDependencyFixture) read(t *testing.T) chat.Run {
	t.Helper()
	c, err := f.app.chat.Store.Read("alice", "default", f.conversation)
	if err != nil {
		t.Fatal(err)
	}
	return *c.Run(f.run)
}
func (f reviewDependencyFixture) prepare(t *testing.T) chat.Run {
	t.Helper()
	if _, err := f.app.prepareChatReview(t.Context(), f.auth, f.conversation, f.run, f.connection); err != nil {
		t.Fatal(err)
	}
	return f.read(t)
}
func (f reviewDependencyFixture) update(t *testing.T, fn func(*chat.Run)) {
	t.Helper()
	if err := f.app.chat.Store.Update("alice", "default", f.conversation, func(c *chat.Conversation) error { fn(c.Run(f.run)); return nil }); err != nil {
		t.Fatal(err)
	}
}
func (f reviewDependencyFixture) save(t *testing.T, path, content string) {
	t.Helper()
	old, err := f.store.Read(t.Context(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Save(t.Context(), path, []byte(content), old.Revision, "operator"); err != nil {
		t.Fatal(err)
	}
}

func TestChatReviewComparisonDependencies(t *testing.T) {
	for _, change := range []string{"unchanged", "content", "references", "unavailable", "prepared"} {
		t.Run(change, func(t *testing.T) {
			f := newReviewDependencyFixture(t)
			initial := f.prepare(t)
			if initial.Review.BatchPath != "notes/a.md" {
				t.Fatalf("unexpected primary: %+v", initial.Review)
			}
			saw := false
			for _, citation := range initial.Citations {
				if citation.Path == "notes/b.md" {
					saw = true
				}
			}
			if !saw {
				t.Fatal("fixture did not include original comparison")
			}
			f.update(t, func(r *chat.Run) {
				if change == "prepared" {
					r.Messages = append(r.Messages, chat.Message{Role: "assistant", Content: "UNFINISHED_OLD_COMPARISON_CLAIM"})
				} else {
					finishChatReviewBatch(r, "STALE_AGREEMENT_SUMMARY")
				}
			})
			switch change {
			case "unchanged":
				if _, err := f.store.Save(t.Context(), "unrelated.md", []byte("Independent new entry."), "", "operator"); err != nil {
					t.Fatal(err)
				}
			case "content", "prepared":
				f.save(t, "notes/b.md", "# Alphalaunch comparison\nThe alphalaunch budget is COMPARISON_NEW_5. [Supporting page](source.md)")
			case "references":
				f.save(t, "notes/source.md", "Changed official report.")
			case "unavailable":
				status, err := f.store.Status(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.store.TrashNotes(t.Context(), []string{"notes/b.md"}, status.Revision, "operator"); err != nil {
					t.Fatal(err)
				}
			}
			next := f.prepare(t)
			primary := next.Review.Notes[0]
			if change == "unchanged" {
				if primary.State != "examined" || primary.Revision != initial.Review.Notes[0].Revision || primary.Summary != "STALE_AGREEMENT_SUMMARY" {
					t.Fatalf("unchanged comparison lost valid original analysis: %+v", primary)
				}
				return
			}
			if primary.State != "pending" || primary.Cursor != 0 || primary.Summary != "" || next.Review.BatchPath != "notes/a.md" {
				t.Fatalf("changed comparison retained old derived coverage: %+v", next.Review)
			}
			raw, _ := json.Marshal(next.Messages)
			if strings.Contains(string(raw), "STALE_AGREEMENT_SUMMARY") || strings.Contains(string(raw), "UNFINISHED_OLD_COMPARISON_CLAIM") {
				t.Fatalf("stale comparison analysis reached renewed context: %s", raw)
			}
			if change == "content" || change == "prepared" {
				if strings.Contains(string(raw), "COMPARISON_OLD_90") || !strings.Contains(string(raw), "COMPARISON_NEW_5") {
					t.Fatalf("renewed comparison omitted current original: %s", raw)
				}
			}
		})
	}
}

func TestChatReviewToolDependencySurvivesReload(t *testing.T) {
	for _, finished := range []bool{false, true} {
		t.Run(fmt.Sprintf("finished=%t", finished), func(t *testing.T) {
			f := newReviewDependencyFixture(t)
			f.prepare(t)
			if _, err := f.store.Save(t.Context(), "notes/z-tool.md", []byte("TOOL_ONLY_ORIGINAL_90"), "", "operator"); err != nil {
				t.Fatal(err)
			}
			call := chat.ToolCall{ID: "review-tool-source", Name: "read_note", Arguments: json.RawMessage(`{"path":"notes/z-tool.md"}`)}
			f.update(t, func(r *chat.Run) {
				r.Messages = append(r.Messages, chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{call}})
			})
			ctx, err := f.app.runContext(f.auth)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.app.executeChatTool(ctx, f.auth, f.conversation, f.run, call, f.connection); err != nil {
				t.Fatal(err)
			}
			read := f.read(t)
			saw := false
			for _, citation := range read.Citations {
				if citation.Path == "notes/z-tool.md" {
					saw = true
				}
			}
			if !saw {
				t.Fatal("actual read_note omitted pinned evidence")
			}
			if finished {
				f.update(t, func(r *chat.Run) { finishChatReviewBatch(r, "TOOL_DERIVED_OLD_90") })
			}
			f.app.chat.Store.Close()
			reopened, err := chat.OpenStore(f.app.db)
			if err != nil {
				t.Fatal(err)
			}
			f.app.chat.Store = reopened
			f.save(t, "notes/z-tool.md", "TOOL_ONLY_NEW_5")
			next := f.prepare(t)
			if next.Review.Notes[0].Cursor != 0 || next.Review.Notes[0].State != "pending" || next.Review.Notes[0].Summary != "" {
				t.Fatalf("changed tool evidence retained completed analysis after reload: %+v", next.Review)
			}
			raw, _ := json.Marshal(next.Messages)
			if strings.Contains(string(raw), "TOOL_ONLY_ORIGINAL_90") || strings.Contains(string(raw), "TOOL_DERIVED_OLD_90") {
				t.Fatalf("changed tool evidence survived prepared continuation after reload: %s", raw)
			}
		})
	}
}

func TestChatReviewDependencyProofIsBoundedAndLegacyRequeues(t *testing.T) {
	t.Run("legacy", func(t *testing.T) {
		f := newReviewDependencyFixture(t)
		f.prepare(t)
		f.update(t, func(r *chat.Run) {
			finishChatReviewBatch(r, "LEGACY_UNTRACKED_ANALYSIS")
			r.Review.Notes[0].Provenance = chat.Provenance{}
		})
		next := f.prepare(t)
		if next.Review.Notes[0].Cursor != 0 || next.Review.Notes[0].Summary != "" || next.Review.Complete {
			t.Fatalf("legacy missing proof certified completed coverage: %+v", next.Review)
		}
	})
	for _, kind := range []string{"unknown tool evidence", "source overflow", "dependency overflow"} {
		t.Run(kind, func(t *testing.T) {
			f := newReviewDependencyFixture(t)
			f.prepare(t)
			f.update(t, func(r *chat.Run) {
				switch kind {
				case "unknown tool evidence":
					r.Provenance.Complete = false
				case "source overflow":
					source := r.Citations[0]
					for i := 0; i < 300; i++ {
						source.Revision = fmt.Sprintf("%040x", i+1)
						r.Citations = append(r.Citations, source)
					}
				case "dependency overflow":
					source := r.Citations[0]
					for i := 0; i < 300; i++ {
						source.Path = fmt.Sprintf("notes/source-%d.md", i)
						r.Citations = append(r.Citations, source)
					}
				}
				finishChatReviewBatch(r, "UNVERIFIABLE_ANALYSIS")
			})
			stopped := f.read(t)
			note := stopped.Review.Notes[0]
			raw, _ := json.Marshal(note.Sources)
			if note.Provenance.Complete || note.State == "examined" || note.Cursor != 0 || stopped.Review.Complete || reviewComplete(stopped.Review) || len(note.Provenance.Dependencies) > 256 || len(note.Sources) > 256 || len(raw) > 64<<10 {
				t.Fatalf("unverifiable or excessive source proof certified coverage: %+v", note)
			}
			if _, err := f.app.prepareChatReview(t.Context(), f.auth, f.conversation, f.run, f.connection); err == nil || !strings.Contains(err.Error(), "evidence dependencies") {
				t.Fatalf("fresh unverifiable result silently retried: %v", err)
			}
			f.update(t, func(r *chat.Run) { r.State = "queued" })
			resumed := f.prepare(t)
			if resumed.Review.Notes[0].State != "pending" || resumed.Review.Notes[0].Cursor != 0 || strings.Contains(fmt.Sprint(resumed.Messages), "UNVERIFIABLE_ANALYSIS") {
				t.Fatalf("explicit resumed allowance failed to requeue unverifiable work: %+v", resumed.Review)
			}
		})
	}
}

func TestChatReviewAccumulatesOriginalSourcesAcrossParts(t *testing.T) {
	f := newReviewDependencyFixture(t)
	f.save(t, "notes/a.md", strings.Repeat("alphalaunch ", 180))
	first := f.prepare(t)
	if first.Review.Notes[0].Total != 2 {
		t.Fatalf("fixture needs two original passages: %+v", first.Review)
	}
	tool, err := f.store.Save(t.Context(), "notes/z-tool.md", []byte("Original tool input."), "", "operator")
	if err != nil {
		t.Fatal(err)
	}
	call := chat.ToolCall{ID: "first-part-source", Name: "read_note", Arguments: json.RawMessage(`{"path":"notes/z-tool.md"}`)}
	ctx, err := f.app.runContext(f.auth)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.app.executeChatTool(ctx, f.auth, f.conversation, f.run, call, f.connection); err != nil {
		t.Fatal(err)
	}
	f.update(t, func(r *chat.Run) { finishChatReviewBatch(r, "First original passage reviewed.") })
	next := f.prepare(t)
	if next.Review.Notes[0].Cursor != 1 || next.Review.BatchCursor != 1 {
		t.Fatalf("part progress not retained: %+v", next.Review)
	}
	originalProof := next.Review.Notes[0].Provenance
	originalSources := append([]chat.Citation(nil), next.Review.Notes[0].Sources...)
	// A later part may discover a new original. The earlier part's immutable
	// provenance must not alias the growing aggregate review dependency map.
	extra := next.Citations[0]
	extra.Path = "notes/later-original.md"
	next.Citations = append(next.Citations, extra)
	finishChatReviewBatch(&next, "Second original passage reviewed.")
	if _, exists := originalProof.Dependencies[extra.Path]; exists {
		t.Fatal("later part mutated previous part's immutable provenance map")
	}
	if next.Review.Notes[0].Provenance.Dependencies[tool.Path] == "" {
		t.Fatal("later part discarded earlier tool dependency")
	}
	saw := false
	for _, source := range originalSources {
		if source.Path == tool.Path && source.Revision == tool.Revision && source.Hash == workspace.ContentHash(tool.Content) && source.References != "" {
			saw = true
		}
		if source.Text != "" {
			t.Fatal("reusable source metadata retained full original text")
		}
	}
	if !saw {
		t.Fatalf("original tool revision/fingerprint not retained across parts: %+v", originalSources)
	}
	// The persisted first part must still invalidate if its earlier original
	// changes, even while the currently prepared second part has newer citations.
	f.save(t, tool.Path, "Changed tool input.")
	invalidated := f.prepare(t)
	if invalidated.Review.Notes[0].Cursor != 0 || invalidated.Review.Notes[0].Summary != "" {
		t.Fatalf("earlier part's changed source retained later progress: %+v", invalidated.Review)
	}
}

func TestChatReviewResumedProviderRebuildsChangedEvidence(t *testing.T) {
	for _, changed := range []string{"comparison", "tool"} {
		t.Run(changed, func(t *testing.T) {
			prompts := make(chan string, 2)
			var count atomic.Int32
			f := newReviewDependencyFixture(t, func(w http.ResponseWriter, r *http.Request) {
				var body json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				prompts <- string(body)
				if count.Add(1) == 1 {
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"PREPARED_OLD_TOOL_CLAIM","tool_calls":[{"id":"review-read","type":"function","function":{"name":"read_note","arguments":"{\"path\":\"notes/z-tool.md\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`)
				} else {
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Reviewed the current original evidence."},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`)
				}
			})
			if _, err := f.store.Save(t.Context(), "notes/z-tool.md", []byte("PREPARED_TOOL_ORIGINAL_90"), "", "operator"); err != nil {
				t.Fatal(err)
			}
			f.app.chat.Config.Connections[0].Limits.MaxRounds = 1
			f.update(t, func(r *chat.Run) { r.State = "interrupted" })
			resume := func() {
				t.Helper()
				w := proxyCall(f.app, "alice", "POST", "/p/default/api/chat/"+f.conversation+"/runs/"+f.run+"/resume", `{}`)
				if w.Code != 200 {
					t.Fatal(w.Body)
				}
				waitChatRun(t, f.app, f.conversation)
				f.app.chat.Wait()
			}
			resume()
			first := f.read(t)
			if first.State != "interrupted" || len(first.Operations) != 1 || first.Operations[0].State != "completed" {
				t.Fatalf("fixture did not pause after acknowledged read: %+v", first)
			}
			<-prompts
			if changed == "comparison" {
				f.save(t, "notes/b.md", "# Alphalaunch comparison\nThe alphalaunch budget is COMPARISON_NEW_5.")
			} else {
				f.save(t, "notes/z-tool.md", "PREPARED_TOOL_NEW_5")
			}
			resume()
			body := <-prompts
			if strings.Contains(body, "PREPARED_OLD_TOOL_CLAIM") || changed == "tool" && strings.Contains(body, "PREPARED_TOOL_ORIGINAL_90") {
				t.Fatalf("explicit continuation sent stale prepared dependency: %s", body)
			}
			if changed == "comparison" && (strings.Contains(body, "COMPARISON_OLD_90") || !strings.Contains(body, "COMPARISON_NEW_5")) {
				t.Fatalf("explicit continuation omitted renewed original comparison: %s", body)
			}
			if count.Load() != 2 {
				t.Fatalf("resume exceeded explicit one-round allowances: %d", count.Load())
			}
		})
	}
}
