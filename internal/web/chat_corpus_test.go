package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"
)

func TestChatCorpusDefaultProviderBoundary(t *testing.T) {
	var mu sync.Mutex
	var captured []string
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, string(raw))
		mu.Unlock()
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Bounded answer."},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":5}}`)
	})
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	for _, note := range []struct{ path, body string }{{"active.md", "# corpusneedle\nACTIVE_CORPUS_MARKER"}, {"archive.md", "---\nfmn-container: archive\n---\n# corpusneedle\nARCHIVE_CORPUS_MARKER"}, {"draft.md", "---\nfmn-container: draft\n---\n# corpusneedle\nDRAFT_CORPUS_MARKER"}} {
		if _, err = store.Save(t.Context(), note.path, []byte(note.body), "", "synthetic"); err != nil {
			t.Fatal(err)
		}
	}
	response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"corpus-default","text":"corpusneedle","options":{"mode":"ask"}}`)
	if response.Code != 200 {
		t.Fatalf("admission %d %s", response.Code, response.Body)
	}
	done := waitChatRun(t, app, c.ID)
	if done.Runs[0].State != "completed" {
		t.Fatal(done.Runs[0].Error)
	}
	mu.Lock()
	requests := strings.Join(captured, "\n")
	mu.Unlock()
	if !strings.Contains(requests, "ACTIVE_CORPUS_MARKER") {
		t.Fatal("eligible evidence missing", requests)
	}
	for _, marker := range []string{"ARCHIVE_CORPUS_MARKER", "DRAFT_CORPUS_MARKER"} {
		if strings.Contains(requests, marker) {
			t.Errorf("automatic provider context disclosed excluded %s", marker)
		}
	}
	raw, _ := json.Marshal(done)
	var wire map[string]any
	json.Unmarshal(raw, &wire)
	if done.Version != 2 {
		t.Errorf("new chat version=%d; want 2", done.Version)
	}
	options := wire["options"].(map[string]any)
	scope, ok := options["corpus"].(map[string]any)
	if !ok || scope["version"] != float64(1) || scope["archives"] != "exclude" || scope["drafts"] != "exclude" || scope["history"] != false {
		t.Errorf("missing resolved Corpus: %v", options)
	}
	runs := wire["runs"].([]any)
	provenance := runs[0].(map[string]any)["provenance"].(map[string]any)
	if provenance["corpusId"] == nil || provenance["corpusId"] == "" {
		t.Error("run lacks corpus identity")
	}
}

func TestChatCorpusAlternateToolsProviderBoundary(t *testing.T) {
	for _, tool := range []struct{ name, args string }{
		{"read_note", `{"path":"archive.md"}`},
		{"search_notes", `{"query":"ARCHIVE_CORPUS_MARKER"}`},
		{"browse_folder", `{}`},
		{"hybrid_search", `{"query":{"text":"ARCHIVE_CORPUS_MARKER","mode":"text","archives":"include"}}`},
		{"get_note_sources", `{"path":"active.md"}`},
		{"read_project_changes", `{"snapshot":true}`},
		{"list_comments", `{"path":"archive.md"}`},
		{"read_live_note", `{"path":"archive.md"}`},
		{"get_indexing_passages", `{}`},
		{"list_trash", `{}`},
		{"export_project_backup", `{}`},
	} {
		t.Run(tool.name, func(t *testing.T) {
			var mu sync.Mutex
			var requests []string
			app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				requests = append(requests, string(raw))
				n := len(requests)
				mu.Unlock()
				if n <= 2 {
					name, args := tool.name, tool.args
					if n == 1 {
						name = "discover_actions"
						discovery, _ := json.Marshal(map[string]any{"names": []string{tool.name}})
						args = string(discovery)
					}
					json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "scope-probe", "type": "function", "function": map[string]string{"name": name, "arguments": args}}}}, "finish_reason": "tool_calls"}}})
				} else {
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Scoped answer."},"finish_reason":"stop"}]}`)
				}
			})
			store, _ := app.catalog.Access(catalog.Bootstrap, "default", "edit")
			for _, note := range []struct{ path, body string }{{"active.md", "# corpusneedle\nACTIVE_CORPUS_MARKER"}, {"archive.md", "---\nfmn-container: archive\ntitle: ARCHIVE_CORPUS_MARKER\n---\n# Excluded\n[ARCHIVE_CORPUS_MARKER](active.md)"}} {
				if _, err := store.Save(t.Context(), note.path, []byte(note.body), "", "synthetic"); err != nil {
					t.Fatal(err)
				}
			}
			response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"alternate-tool","text":"corpusneedle","options":{"mode":"ask"}}`)
			if response.Code != 200 {
				t.Fatal(response.Body)
			}
			done := waitChatRun(t, app, c.ID)
			mu.Lock()
			captured := append([]string(nil), requests...)
			mu.Unlock()
			// The model's own query may contain the marker; only tool-result disclosure
			// counts. Parse messages instead of treating generated arguments as evidence.
			for _, raw := range captured {
				var wire struct {
					Messages []struct{ Role, Content string }
				}
				json.Unmarshal([]byte(raw), &wire)
				for _, message := range wire.Messages {
					if message.Role == "tool" && strings.Contains(message.Content, "ARCHIVE_CORPUS_MARKER") {
						t.Errorf("excluded source through %s: %s", tool.name, message.Content)
					}
				}
			}
			if len(captured) == 0 {
				t.Fatal("initial provider request absent", done.Runs)
			}
		})
	}
}

func TestChatCorpusScopeChangeInvalidatesUncitedAnswer(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, string(raw))
		n := len(requests)
		mu.Unlock()
		answer := "Fresh answer"
		if n == 1 {
			answer = "OLD_SCOPE_SUMMARY_MARKER"
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": answer}, "finish_reason": "stop"}}})
	})
	path := "/p/default/api/chat/" + c.ID + "/turns"
	for i, options := range []string{`{"mode":"ask"}`, `{"mode":"ask","corpus":{"version":1,"archives":"include","drafts":"exclude","history":false}}`} {
		body := fmt.Sprintf(`{"submissionId":"scope-change-%d","text":"ordinary greeting","options":%s}`, i, options)
		response := proxyCall(app, "alice", "POST", path, body)
		if response.Code != 200 {
			t.Fatal(response.Body)
		}
		done := waitChatRun(t, app, c.ID)
		if done.Runs[len(done.Runs)-1].State != "completed" {
			t.Fatal(done.Runs)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatal("provider request count", len(requests))
	}
	if strings.Contains(requests[1], "OLD_SCOPE_SUMMARY_MARKER") {
		t.Fatal("uncited prior answer crossed a changed Corpus at unchanged HEAD")
	}
	saved, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil || saved.Turns[1].Content != "OLD_SCOPE_SUMMARY_MARKER" {
		t.Fatal("original transcript changed", err)
	}
}

func TestChatCorpusClassificationChangeBeforeNextProvider(t *testing.T) {
	var app *Server
	var mu sync.Mutex
	var requests []string
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, string(raw))
		n := len(requests)
		mu.Unlock()
		if n == 1 {
			store, err := app.catalog.Access(catalog.Bootstrap, "default", "edit")
			if err != nil {
				t.Error(err)
				return
			}
			doc, err := store.Read(r.Context(), "changing.md", "")
			if err != nil {
				t.Error(err)
				return
			}
			if _, err = store.SetContainer(r.Context(), doc.Path, "archive", doc.Revision, "synthetic concurrent user"); err != nil {
				t.Error(err)
				return
			}
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"status-after-change","type":"function","function":{"name":"project_status","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
		} else {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Current scoped answer"},"finish_reason":"stop"}]}`)
		}
	})
	store, _ := app.catalog.Access(catalog.Bootstrap, "default", "edit")
	if _, err := store.Save(t.Context(), "changing.md", []byte("# corpusneedle\nCHANGED_TO_EXCLUDED_MARKER"), "", "synthetic"); err != nil {
		t.Fatal(err)
	}
	response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"changed-between-calls","text":"corpusneedle","options":{"mode":"ask"}}`)
	if response.Code != 200 {
		t.Fatal(response.Body)
	}
	done := waitChatRun(t, app, c.ID)
	mu.Lock()
	defer mu.Unlock()
	if len(requests) == 0 || !strings.Contains(requests[0], "CHANGED_TO_EXCLUDED_MARKER") {
		t.Fatal("initial fixture lacked eligible evidence")
	}
	for _, raw := range requests[1:] {
		if strings.Contains(raw, "CHANGED_TO_EXCLUDED_MARKER") {
			t.Fatal("newly excluded context reached another provider request")
		}
	}
	if done.Runs[0].State == "failed" {
		t.Fatal("context change must safely refresh or interrupt", done.Runs[0].Error)
	}
}

func TestChatCorpusMutationAndReviewBoundaries(t *testing.T) {
	for _, mode := range []string{"save", "proposal", "review"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			var requests []string
			var revision string
			app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				requests = append(requests, string(raw))
				n := len(requests)
				mu.Unlock()
				if n == 1 && mode != "review" {
					name := "save_note"
					args := map[string]any{"path": "archive.md", "revision": revision, "content": "# Promoted by automatic action"}
					if mode == "proposal" {
						name = "submit_change_group"
						args = map[string]any{"mode": "proposal", "group": map[string]any{"id": "excluded-proposal", "summary": "Excluded change", "edits": []any{map[string]any{"path": "archive.md", "revision": revision, "hash": workspace.ContentHash([]byte("---\nfmn-container: archive\n---\n# EXCLUDED_REVIEW_MARKER")), "content": "# Promoted"}}}}
					}
					arg, _ := json.Marshal(args)
					json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "excluded-mutation", "type": "function", "function": map[string]string{"name": name, "arguments": string(arg)}}}}, "finish_reason": "tool_calls"}}})
					return
				}
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Finished"},"finish_reason":"stop"}]}`)
			})
			if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
				t.Fatal(err)
			}
			store, _ := app.catalog.Access(catalog.Bootstrap, "default", "edit")
			doc, err := store.Save(t.Context(), "archive.md", []byte("---\nfmn-container: archive\n---\n# EXCLUDED_REVIEW_MARKER"), "", "synthetic")
			if err != nil {
				t.Fatal(err)
			}
			revision = doc.Revision
			review := "false"
			if mode == "review" {
				review = "true"
			}
			response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"guard-boundary","text":"Review or update notebook","options":{"mode":"edit","actions":["notes"],"review":`+review+`}}`)
			if response.Code != 200 {
				t.Fatal(response.Body)
			}
			done := waitChatRun(t, app, c.ID)
			after, _ := store.Read(t.Context(), "archive.md", "")
			if string(after.Content) != string(doc.Content) {
				t.Error("excluded target promoted by automatic write")
			}
			e, releaseEngine, _ := app.acquireEngine(t.Context(), store)
			if releaseEngine != nil {
				t.Cleanup(releaseEngine)
			}

			proposals, err := e.Proposals(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(proposals) != 0 {
				t.Error("excluded target entered shared proposal")
			}
			mu.Lock()
			defer mu.Unlock()
			for _, raw := range requests {
				if strings.Contains(raw, "EXCLUDED_REVIEW_MARKER") {
					t.Error("excluded content reached review/provider")
				}
			}
			if mode == "review" && done.Runs[0].Review != nil && len(done.Runs[0].Review.Notes) != 0 {
				t.Error("excluded note counted in full-review denominator")
			}
		})
	}
}

func TestChatCorpusHistoricalAnswerKeepsVersionLocalProof(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	var revision string
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, string(raw))
		n := len(requests)
		mu.Unlock()
		if n == 1 {
			args, _ := json.Marshal(map[string]string{"path": "past.md", "revision": revision})
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "past-read", "type": "function", "function": map[string]string{"name": "read_note", "arguments": string(args)}}}}, "finish_reason": "tool_calls"}}})
			return
		}
		content := "Current answer"
		if n == 2 {
			content = "HISTORICAL_DERIVED_CLAIM"
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": content}, "finish_reason": "stop"}}})
	})
	store, _ := app.catalog.Access(catalog.Bootstrap, "default", "edit")
	doc, err := store.Save(t.Context(), "past.md", []byte("# Historical eligible original\n"), "", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	revision = doc.Revision
	if _, err = store.SetContainer(t.Context(), doc.Path, "archive", doc.Revision, "synthetic"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		body := fmt.Sprintf(`{"submissionId":"historical-%d","text":"Discuss the selected past version","options":{"mode":"ask","corpus":{"version":1,"archives":"exclude","drafts":"exclude","history":true}}}`, i)
		response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", body)
		if response.Code != 200 {
			t.Fatal(response.Body)
		}
		done := waitChatRun(t, app, c.ID)
		if done.Runs[len(done.Runs)-1].State != "completed" {
			t.Fatal(done.Runs[len(done.Runs)-1].Error)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 3 {
		t.Fatal("provider requests", len(requests))
	}
	if !strings.Contains(requests[2], "HISTORICAL_DERIVED_CLAIM") {
		t.Fatal("eligible immutable historical proof was coerced into today's excluded path")
	}
}

func TestChatCorpusGuidanceRemainsSeparateFromRetrieval(t *testing.T) {
	var mu sync.Mutex
	var request string
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		request = string(raw)
		mu.Unlock()
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Understood"},"finish_reason":"stop"}]}`)
	})
	store, _ := app.catalog.Access(catalog.Bootstrap, "default", "owner")
	if _, err := store.WriteInstructions(t.Context(), []byte("# guidanceneedle\nOWNER_GUIDANCE_MARKER"), "", "owner"); err != nil {
		t.Fatal(err)
	}
	response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"guidance-separate","text":"guidanceneedle","options":{"mode":"ask"}}`)
	if response.Code != 200 {
		t.Fatal(response.Body)
	}
	done := waitChatRun(t, app, c.ID)
	for _, citation := range done.Runs[0].Citations {
		if citation.Path == "AGENTS.md" {
			t.Error("Owner guidance entered note-corpus retrieval")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(request, "OWNER_GUIDANCE_MARKER") {
		t.Error("separate Owner guidance missing")
	}
}
