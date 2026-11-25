package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/workspace"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestChatGlobalEvidenceBudgetFourRoundsTwelveTools(t *testing.T) {
	testChatGlobalEvidenceBudget(t, "suggest", false)
}
func TestChatAskReservesGroundedFinalAnswer(t *testing.T) {
	testChatGlobalEvidenceBudget(t, "ask", false)
}
func TestChatLegacyTwelveToolContinueKeepsOriginalReceipts(t *testing.T) {
	testChatGlobalEvidenceBudget(t, "suggest", true)
}

func testChatGlobalEvidenceBudget(t *testing.T, mode string, legacy bool) {
	t.Helper()
	var requests atomic.Int32
	originals := map[string]string{}
	path := func(group string, n int) string {
		return fmt.Sprintf("notes/%s-%02d-%s.md", group, n, strings.Repeat("synthetic-observation-", 6))
	}
	makeCall := func(id, name string, args any) map[string]any {
		raw, _ := json.Marshal(args)
		return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(raw)}}
	}
	read := func(id, path string) map[string]any {
		return makeCall(id, "read_note", map[string]string{"path": path})
	}
	search := func(id, query, scope string) map[string]any {
		return makeCall(id, "search_passages", map[string]string{"text": query, "scope": scope})
	}
	rounds := [][]any{
		{read("read-target", "notes/target.md"), makeCall("source-origin", "get_note_sources", map[string]string{"path": "notes/target.md"})},
		{read("read-exception", "notes/exception.md"), search("amber", "amber observations", "current"), search("cobalt", "cobalt observations", "current"), search("delta", "delta observations", "current")},
		{read("read-large", "notes/large.md"), read("read-other-1", path("amber", 0)), read("read-other-2", path("cobalt", 0)), read("read-other-3", path("delta", 0))},
		{search("epsilon", "epsilon observations", "current"), search("zeta-history", "zeta observations", "history")},
	}
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []chat.Message    `json:"messages"`
			Tools    []json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		n := int(requests.Add(1))
		w.Header().Set("Content-Type", "application/json")
		if n <= 4 && len(body.Tools) > 0 {
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "Checking synthetic originals.", "tool_calls": rounds[n-1]}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 9918, "completion_tokens": 169}})
			return
		}
		text := ""
		for _, message := range body.Messages {
			text += message.Content
		}
		if !strings.Contains(text, "2037-08-19") || !strings.Contains(text, "17 days") {
			t.Error("final request lost substantive original target/exception evidence")
		}
		citation := ""
		groupCitations := map[string]string{}
		for _, message := range body.Messages {
			evidence := chatSourceContext{Sources: modelRequestEvidence(message)}
			if len(evidence.Sources) > 0 {
				for _, source := range evidence.Sources {
					if source.Hash == "" || source.Revision == "" || source.Hash != workspace.ContentHash([]byte(originals[source.Path])) {
						t.Error("retained original source lost the guarded-edit preconditions")
					}
					if strings.Contains(source.Text, "2037-08-19") {
						citation = source.URL
					}
					for _, group := range []string{"amber", "cobalt", "delta", "epsilon", "zeta"} {
						if strings.Contains(source.Text, group+" coefficient is 23") {
							groupCitations[group] = source.URL
						}
					}
				}
			}
		}
		if mode == "suggest" {
			for _, group := range []string{"amber", "cobalt", "delta", "epsilon", "zeta"} {
				if !strings.Contains(text, group+" coefficient is 23") {
					t.Errorf("final comparison lost substantive %s evidence", group)
				}
			}
		}
		if legacy && len(body.Tools) > 0 {
			t.Error("legacy Ask continuation offered more exploratory tools")
		}
		if citation == "" {
			t.Error("final request lost the pinned target citation")
		}
		answer := "The synthetic relay target is 2037-08-19 and its exception window is 17 days. Original source: " + citation + ". Other unexamined evidence remains uncertain."
		if mode == "suggest" {
			for _, group := range []string{"amber", "cobalt", "delta", "epsilon", "zeta"} {
				if groupCitations[group] == "" {
					t.Errorf("missing original citation for %s comparison", group)
				}
				answer += " The " + group + " coefficient is 23 (" + groupCitations[group] + ")."
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 8000, "completion_tokens": 70}})
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	store, err := app.catalog.Access("alice", "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	var edits []workspace.NoteEdit
	save := func(name, content string) {
		originals[name] = content
		edits = append(edits, workspace.NoteEdit{Path: name, Content: content})
	}
	save("notes/target.md", "# Synthetic relay target\nThe original synthetic relay target date is 2037-08-19. This committed planning observation is the authoritative target recorded for this synthetic exercise.\n")
	save("notes/exception.md", "# Synthetic relay exception\nThe original synthetic relay exception window is 17 days. This separate committed policy passage qualifies the target date and must be compared with it.\n")
	save("notes/large.md", "# Synthetic large observation\nIndependent extended synthetic details.\n"+strings.Repeat("Large synthetic observation, original archive detail and independently recorded calibration evidence. ", 900))
	for _, group := range []string{"amber", "cobalt", "delta", "epsilon", "zeta"} {
		for n := 0; n < 12; n++ {
			save(path(group, n), fmt.Sprintf("# Synthetic %s observations %02d\nThe %s coefficient is 23 in this synthetic original measurement.\n", group, n, group)+strings.Repeat(group+" observations provide synthetic measurements and independently recorded context for comparison. ", 12))
		}
	}
	// The evidence budget depends on these committed notes, not on one commit
	// per note. Separate commits made the run index 64 history revisions.
	for start := 0; start < len(edits); start += 50 {
		group := workspace.EditGroup{ID: fmt.Sprintf("synthetic-evidence-%d", start), Summary: "Synthetic evidence fixture", Edits: edits[start:min(start+50, len(edits))]}
		if _, err := store.ApplyGroup(t.Context(), group); err != nil {
			t.Fatal(err)
		}
	}
	if app.chat.Config.Connections[0].Limits.ContextTokens != 16384 || app.chat.Config.Connections[0].Limits.OutputTokens != 2048 {
		t.Fatal("default budgets changed")
	}
	// Four rounds of real Git reads under -race need the same wall-clock
	// allowance as waitEvidenceRun; this test measures evidence, not latency.
	app.chat.Config.Connections[0].Limits.RunSeconds = 30
	if legacy {
		app.chat.Config.Connections[0].Limits.MaxRounds = 4
	}
	body, _ := json.Marshal(map[string]any{"submissionId": "many-original-results", "text": "Compare the original synthetic relay target date and exception window, considering amber, cobalt, delta, epsilon and zeta observations. State missing evidence honestly.", "options": map[string]any{"mode": mode, "corpus": &chat.Corpus{Version: 1, Archives: "exclude", Drafts: "exclude", History: true}}})
	endpoint := "/p/default/api/chat/" + c.ID
	response := proxyCall(app, "alice", "POST", endpoint+"/turns", string(body))
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	done := waitEvidenceRun(t, app, c.ID)
	run := done.Runs[0]
	if legacy {
		if requests.Load() != 4 || len(run.Operations) != 12 || run.State != "interrupted" {
			t.Fatalf("legacy setup did not retain four acknowledged rounds/twelve tools: state=%s error=%q requests=%d operations=%d", run.State, run.Error, requests.Load(), len(run.Operations))
		}
		receipts := append([]chat.Operation(nil), run.Operations...)
		// Construct the previously deployed Ask state through real read-only tools,
		// with the new Ask cutoff disabled only during fixture setup via Suggest.
		// Changing it back to Ask reduces authority; it never permits a mutation.
		if err = app.chat.Store.Update("alice", "default", c.ID, func(saved *chat.Conversation) error {
			current := saved.Run(run.ID)
			current.Options.Mode = "ask"
			current.State = "failed"
			current.Error = "Model context limit reached after reducing optional evidence"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		response = proxyCall(app, "alice", "POST", endpoint+"/runs/"+run.ID+"/resume", `{}`)
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body)
		}
		done = waitEvidenceRun(t, app, c.ID)
		run = done.Runs[0]
		if !reflect.DeepEqual(run.Operations, receipts) {
			t.Fatal("legacy Continue repeated or changed completed receipts")
		}
	}
	if run.State != "completed" {
		t.Fatalf("global evidence/metadata budget prevented final answer: state=%s error=%q requests=%d operations=%d", run.State, run.Error, requests.Load(), len(run.Operations))
	}
	if mode == "ask" && (requests.Load() > 3 || len(run.Operations) > 6) {
		t.Fatalf("ordinary Ask exhausted exploration instead of reserving its answer: requests=%d tools=%d", requests.Load(), len(run.Operations))
	}
	if mode == "suggest" && (requests.Load() != 5 || len(run.Operations) != 12) {
		t.Fatalf("non-Ask workflow was curtailed: requests=%d tools=%d", requests.Load(), len(run.Operations))
	}
	if run.Usage.Requests != int(requests.Load()) || run.Usage.Tokens() > 64000 {
		t.Fatal("finalization lost accounting or exceeded its explicit allowance")
	}
	large := false
	for _, op := range run.Operations {
		if len(op.Result) > 151000 {
			large = true
		}
	}
	if mode == "suggest" && !large {
		t.Fatal("full large read receipt was not retained")
	}
	if !strings.Contains(done.Turns[len(done.Turns)-1].Content, "2037-08-19") {
		t.Fatal("no useful final answer persisted")
	}
	if mode == "suggest" {
		for _, group := range []string{"amber", "cobalt", "delta", "epsilon", "zeta"} {
			if !strings.Contains(done.Turns[len(done.Turns)-1].Content, group+" coefficient is 23 (/notes/") {
				t.Errorf("final answer omitted grounded %s comparison", group)
			}
		}
	}
}

// Large real-Git evidence fixtures need a bounded race-instrumented wait. This
// changes neither application deadlines nor the configured-deadline test helper.
// It joins the admitted run rather than polling: rereading the growing
// conversation every few milliseconds took CPU from the run it was waiting for.
func waitEvidenceRun(t *testing.T, app *Server, id string) chat.Conversation {
	t.Helper()
	joined := make(chan struct{})
	go func() {
		app.chat.Wait()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(30 * time.Second):
		t.Fatal("large evidence fixture did not finish within 30 seconds")
	}
	c, err := app.chat.Store.Read("alice", "default", id)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Runs) == 0 || chat.Active(c.Runs[len(c.Runs)-1].State) {
		t.Fatalf("evidence run stopped without settling: %+v", c.Runs)
	}
	return c
}

// Read the complete native request: pooled originals may remain in the initial
// context when duplicate tool envelopes have become acknowledgements.
func modelRequestEvidence(message chat.Message) []chat.Citation {
	var result struct {
		Sources []chat.Citation `json:"sources"`
	}
	if message.Role == "tool" {
		var tool chatSourceContext
		if json.Unmarshal([]byte(message.Content), &tool) == nil && (tool.Kind == "note_read" || tool.Kind == "passage_search") {
			return tool.Sources
		}
	}
	if message.Role == "user" {
		const opening = "\n<untrusted_notebook_context>\n"
		const closing = "\n</untrusted_notebook_context>"
		start := strings.LastIndex(message.Content, opening)
		if start >= 0 && strings.HasSuffix(message.Content, closing) && json.Unmarshal([]byte(message.Content[start+len(opening):len(message.Content)-len(closing)]), &result) == nil {
			return result.Sources
		}
	}
	return nil
}
