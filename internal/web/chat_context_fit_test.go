package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestChatContextReadAndSearchStillReachAnswerAtDefaultLimit(t *testing.T) {
	testChatContextReadAndSearch(t, false, false)
}
func TestChatContextLegacyContinueKeepsCompletedReceipts(t *testing.T) {
	testChatContextReadAndSearch(t, true, false)
}
func TestChatContextRepeatedReadSearchRoundsStillReachAnswer(t *testing.T) {
	testChatContextReadAndSearch(t, false, true)
}
func testChatContextReadAndSearch(t *testing.T, legacy, repeated bool) {
	var requests atomic.Int32
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		n := requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 || repeated && n == 2 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"read-overview","type":"function","function":{"name":"read_note","arguments":"{\"path\":\"notes/overview.md\"}"}},{"id":"search-details","type":"function","function":{"name":"search_passages","arguments":"{\"text\":\"Zephyr relay retrywindow\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7000,"completion_tokens":90}}`)
			return
		}
		tools := 0
		useful := ""
		toolText := ""
		for _, m := range body.Messages {
			useful += m.Content
			if m.Role == "tool" {
				tools++
				toolText += m.Content
			}
		}
		expectedTools := 2
		if repeated {
			expectedTools = 4
		}
		if tools != expectedTools || !strings.Contains(useful, "2031-05-17") || !strings.Contains(useful, "14 days") {
			t.Errorf("continuation lost requested original evidence: tools=%d date=%v retry=%v", tools, strings.Contains(useful, "2031-05-17"), strings.Contains(useful, "14 days"))
		}
		if strings.Contains(toolText, `"html"`) {
			t.Error("read result duplicated rendered HTML in model context")
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The synthetic Zephyr relay target is 2031-05-17 and the retry window is 14 days, according to the requested committed sources."},"finish_reason":"stop"}],"usage":{"prompt_tokens":8000,"completion_tokens":50}}`)
	})
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "owner")
	if err != nil {
		t.Fatal(err)
	}
	paragraph := "Zephyr relay schedule target analysis records reproducible observations, calibration details, resource allocation, package compatibility, verification criteria and repeatable follow-up procedures. "
	overview := "# Zephyr relay overview\nThe target date is 2031-05-17.\n" + strings.Repeat(paragraph, 110)
	notes := map[string]string{"notes/overview.md": overview, "notes/retrywindow.md": "# Zephyr relay retrywindow\nThe retry window is 14 days.\n" + strings.Repeat(paragraph, 75)}
	for i := 0; i < 12; i++ {
		notes[fmt.Sprintf("notes/study-%02d.md", i)] = fmt.Sprintf("# Zephyr relay schedule study %d\n", i) + strings.Repeat(paragraph, 35)
	}
	for path, content := range notes {
		if _, err = store.Save(t.Context(), path, []byte(content), "", "operator"); err != nil {
			t.Fatal(err)
		}
	}
	if app.chat.Config.Connections[0].Limits.ContextTokens != 16384 || app.chat.Config.Connections[0].Limits.OutputTokens != 2048 {
		t.Fatal("fixture changed default context/output limits")
	}
	if legacy {
		app.chat.Config.Connections[0].Limits.MaxRounds = 1
		if err = app.catalog.Member(catalog.Bootstrap, "default", "alice", "comment"); err != nil {
			t.Fatal(err)
		}
	}
	submission := `{"submissionId":"context-read-search","text":"Explain the Zephyr relay schedule target and retry window using committed notes.","options":{"mode":"ask"}}`
	// Recreate a deployed one-round receipt pause without the new Ask answer
	// reservation during fixture setup. Resume then restores read-only Ask.
	if legacy {
		submission = strings.Replace(submission, `"mode":"ask"`, `"mode":"suggest"`, 1)
	}
	w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", submission)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	// Join the admitted worker before reading its result in this multi-round fixture.
	app.chat.Wait()
	done := waitChatRun(t, app, c.ID)
	run := done.Runs[0]
	if legacy {
		if run.State != "interrupted" || requests.Load() != 1 || len(run.Operations) != 2 {
			t.Fatalf("legacy setup did not pause after acknowledged tools: state=%s requests=%d tools=%d", run.State, requests.Load(), len(run.Operations))
		}
		receipts := append([]chat.Operation(nil), run.Operations...)
		if err = app.chat.Store.Update("alice", "default", c.ID, func(saved *chat.Conversation) error {
			current := saved.Run(run.ID)
			current.Options.Mode = "ask"
			for i, message := range current.Messages {
				if message.Role != "tool" {
					continue
				}
				for _, op := range current.Operations {
					if op.ToolCallID != message.ToolCallID {
						continue
					}
					current.Messages[i].Content = oldChatSourceRepresentation(op)
				}
			}
			// Restore the old server's full automatic-evidence JSON. It retained all
			// pinned metadata in both the original retrieval and raw tool responses.
			index, prefix, payload, _ := optionalChatSources(current.Messages, false)
			initial := []chat.Citation{}
			for _, source := range current.Citations {
				if strings.HasPrefix(source.ID, "source-") {
					initial = append(initial, source)
				}
			}
			payload["sources"], _ = json.Marshal(initial)
			raw, _ := json.Marshal(payload)
			current.Messages[index].Content = prefix + "\n<untrusted_notebook_context>\n" + string(raw) + "\n</untrusted_notebook_context>"
			req := chat.Request{Model: current.Options.Model, System: chatSystem, Messages: current.Messages, Tools: app.runTools(*current, app.chat.Config.Connections[0]), MaxOutputTokens: 2048}
			size, estimateErr := app.chat.Snapshot().Providers["local"].EstimateRequest(req)
			if estimateErr != nil {
				return estimateErr
			}
			if size+2048 <= 16384 {
				t.Fatalf("legacy fixture does not reproduce old overflow: %d", size)
			}
			current.State = "interrupted"
			current.Error = "Chat context or token budget reached; start a new conversation or continue with a fresh budget"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		w = proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/runs/"+run.ID+"/resume", `{}`)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
		app.chat.Wait()
		done = waitChatRun(t, app, c.ID)
		run = done.Runs[0]
		if !reflect.DeepEqual(run.Operations, receipts) {
			t.Fatal("Continue reran or changed completed tool receipts")
		}
	}
	expectedRequests, expectedOperations := int32(2), 2
	if repeated {
		expectedRequests, expectedOperations = 3, 4
	}
	if run.State != "completed" || requests.Load() != expectedRequests {
		t.Fatalf("read/search tools left no room for final answer: state=%s error=%q requests=%d operations=%d", run.State, run.Error, requests.Load(), len(run.Operations))
	}
	if len(run.Operations) != expectedOperations || run.Usage.Requests != int(expectedRequests) || done.Turns[len(done.Turns)-1].Role != "assistant" {
		t.Fatal("completion lost durable tools, accounting or answer")
	}
	var stored noteOutput
	if json.Unmarshal(run.Operations[0].Result, &stored) != nil || stored.Content != overview || stored.HTML == "" {
		t.Fatal("model excerpting damaged the full durable read result")
	}
	if len(run.Citations) == 0 || len(run.Operations[1].Result) < 12000 {
		t.Fatal("large original search result or pinned citations were not retained")
	}
}

// Exact deployed representation: read acknowledgements appended pinned metadata;
// the custom search tool returned clipped raw JSON without a second footer.
func oldChatSourceRepresentation(op chat.Operation) string {
	text := textLimit(string(op.Result), 12000)
	if op.Name == "read_note" && len(op.Citations) > 0 {
		metadata := append([]chat.Citation(nil), op.Citations...)
		for i := range metadata {
			metadata[i].Text = ""
		}
		raw, _ := json.Marshal(metadata)
		text += "\nPinned notebook evidence: " + string(raw)
	}
	return text
}

func TestChatContextLegacyRepeatedIDsKeepOriginalRevision(t *testing.T) {
	makeReceipt := func(revision, operation, suffix string) chat.Operation {
		raw, _ := json.Marshal(noteOutput{Path: "notes/synthetic.md", Content: strings.Repeat("Same synthetic prefix. ", 800) + suffix, HTML: "<p>Synthetic</p>", Revision: revision})
		return chat.Operation{ID: operation, Name: "read_note", ToolCallID: "repeated-read", State: "completed", Result: raw, Citations: []chat.Citation{{ID: "read-" + operation, Path: "notes/synthetic.md", URL: "/notes/synthetic.md?fmn-rev=" + revision, Revision: revision, Kind: "committed"}}}
	}
	old, current := makeReceipt("original-revision", "first-operation", "OLD_END"), makeReceipt("later-revision", "second-operation", "NEW_END")
	if textLimit(string(old.Result), 12000) != textLimit(string(current.Result), 12000) {
		t.Fatal("fixture prefixes do not collide")
	}
	call := chat.Message{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "repeated-read", Name: "read_note", Arguments: json.RawMessage(`{"path":"notes/synthetic.md"}`)}}}
	req := chat.Request{MaxOutputTokens: 2048, Messages: []chat.Message{{Role: "user", Content: "Compare the two original revisions."}, call, {Role: "tool", ToolCallID: "repeated-read", Content: oldChatSourceRepresentation(old)}, call, {Role: "tool", ToolCallID: "repeated-read", Content: oldChatSourceRepresentation(current)}}}
	fitted, _, err := fitChatRequest(chat.Run{Operations: []chat.Operation{old, current}}, req, 16384)
	if err != nil {
		t.Fatal(err)
	}
	for i, revision := range map[int]string{2: "original-revision", 4: "later-revision"} {
		var got chatSourceContext
		if json.Unmarshal([]byte(fitted.Messages[i].Content), &got) != nil || len(got.Sources) != 1 || got.Sources[0].Revision != revision {
			t.Fatalf("tool cycle %d was substituted from another receipt: %s", i, fitted.Messages[i].Content)
		}
	}
	if req.Messages[2].Content != oldChatSourceRepresentation(old) {
		t.Fatal("legacy transcript mutated")
	}
}

func TestChatContextAmbiguousLegacySearchDoesNotGuessReceipt(t *testing.T) {
	prefix := strings.Repeat("same synthetic passage prefix ", 500)
	makeReceipt := func(revision string) chat.Operation {
		raw := json.RawMessage(fmt.Sprintf(`{"passages":[{"text":%q,"path":"notes/synthetic.md","revision":%q,"citation":%q}]}`, prefix, revision, "/notes/synthetic.md?fmn-rev="+revision))
		return chat.Operation{Name: "search_passages", ToolCallID: "repeated-search", State: "completed", Result: raw}
	}
	old, current := makeReceipt("old"), makeReceipt("new")
	msg := chat.Message{Role: "tool", ToolCallID: "repeated-search", Content: oldChatSourceRepresentation(old)}
	if msg.Content != oldChatSourceRepresentation(current) {
		t.Fatal("search fixture does not have an ambiguous clipped prefix")
	}
	req := chat.Request{MaxOutputTokens: 2048, Messages: []chat.Message{{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: msg.ToolCallID, Name: "search_passages", Arguments: json.RawMessage(`{"text":"synthetic"}`)}}}, msg}}
	fitted, _, err := fitChatRequest(chat.Run{Operations: []chat.Operation{old, current}}, req, 16384)
	if err != nil {
		t.Fatal(err)
	}
	if fitted.Messages[1].Content != msg.Content {
		t.Fatal("ambiguous legacy evidence was replaced with a guessed receipt")
	}
}

// Each estimate encodes the whole request, which may hold entire notes. A
// request that fits is estimated once, as the model receives it.
func TestChatFitEstimatesFittingRequestOnce(t *testing.T) {
	request := chat.Request{Model: "synthetic", System: "Synthetic system", MaxOutputTokens: 1000, Messages: []chat.Message{{Role: "user", Content: nativeBudgetUser(t, nativeBudgetSources("fits", 3, 4), "SYNTHETIC_ATTACHMENT")}}}
	estimates := 0
	fitted, fit, err := fitChatRequest(chat.Run{}, request, 100000, func(r chat.Request) (int, error) { estimates++; return estimateTokens(r), nil })
	if err != nil {
		t.Fatal(err)
	}
	if estimates != 1 {
		t.Fatalf("fitting request estimated %d times", estimates)
	}
	content := fitted.Messages[0].Content
	if strings.Contains(content, "synthetic-passage-hash") || !strings.Contains(content, "synthetic-note-hash") || !strings.Contains(content, "SYNTHETIC_ATTACHMENT") {
		t.Fatal("fitting request lost its model projection: passage checksums stay in the durable record, whole-note guards and attachments reach the model")
	}
	if fit.Changed || fit.AfterTokens != estimateTokens(fitted) || fit.BeforeTokens != fit.AfterTokens || fit.SourcesBefore != 3 || fit.SourcesAfter != 3 {
		t.Fatalf("fitting request reported %+v", fit)
	}
}

// Automation sends the fitted request at the size the fit last estimated.
func TestChatFitReportsEstimateOfReducedRequest(t *testing.T) {
	request := chat.Request{Model: "synthetic", System: "Synthetic system", MaxOutputTokens: 1000, Messages: []chat.Message{{Role: "user", Content: nativeBudgetUser(t, nativeBudgetSources("reduce", 12, 300), "SYNTHETIC_ATTACHMENT")}}}
	fitted, fit, err := fitChatRequest(chat.Run{}, request, 8000, func(r chat.Request) (int, error) { return estimateTokens(r), nil })
	if err != nil {
		t.Fatal(err)
	}
	if !fit.Changed || fit.SourcesAfter == 0 || fit.SourcesAfter >= fit.SourcesBefore || fit.BeforeTokens != estimateTokens(request) || fit.AfterTokens != estimateTokens(fitted) || fit.AfterTokens+fitted.MaxOutputTokens > 8000 {
		t.Fatalf("reduced request reported %+v; request %d, fitted %d", fit, estimateTokens(request), estimateTokens(fitted))
	}
}
