package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

func TestReviewR2RankedMultibyteOriginalReachesFinalAnswer(t *testing.T) {
	const fact = "The approved schedule target is 2039-11-27 and the exception allowance is 41 days."
	originals := map[string]string{"notes/calibration.md": "# Calibration\n" + strings.Repeat("測定 ", 300) + fact + "\n"}
	testRankedCompleteOriginals(t, 16384, originals, []string{fact}, "approved schedule target exception allowance", "What approved schedule target and exception allowance do the original passages establish?", true)
}

func TestReviewR2RankedMultiResultOriginalsReachFinalAnswer(t *testing.T) {
	originals := map[string]string{}
	facts := []string{}
	for i := 0; i < 9; i++ {
		facts = append(facts, fmt.Sprintf("The flux cutoff for specimen %d is VIOLET-%02d.", i, i))
		originals[fmt.Sprintf("notes/specimen-%d.md", i)] = fmt.Sprintf("# Calibration %d\n", i) + strings.Repeat("Instrument background. ", 43) + facts[i] + "\n"
	}
	for _, limit := range []int{16384, 6144} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			testRankedCompleteOriginals(t, limit, originals, facts, "calibration flux cutoff", "Identify my coded notebook facts.", false)
		})
	}
}

func testRankedCompleteOriginals(t *testing.T, limit int, originals map[string]string, facts []string, query, task string, automatic bool) {
	t.Helper()
	var requests atomic.Int32
	var finalSources []chat.Citation
	var finalAnswer string
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
				CallID  string `json:"tool_call_id"`
				Calls   []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"messages"`
			Tools     []json.RawMessage `json:"tools"`
			MaxTokens int               `json:"max_tokens"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
		}
		if body.MaxTokens != 2048 || (len(raw)+2)/3+body.MaxTokens > limit {
			t.Errorf("actual native request exceeded its context/output allowance: bytes=%d output=%d limit=%d", len(raw), body.MaxTokens, limit)
		}
		n := requests.Add(1)
		pending := map[string]bool{}
		resultCount := 0
		sources := []chat.Citation{}
		partial := false
		for _, m := range body.Messages {
			message := chat.Message{Role: m.Role, Content: m.Content}
			sources = append(sources, modelRequestEvidence(message)...)
			for _, call := range m.Calls {
				if pending[call.ID] {
					t.Error("duplicate native call before result")
				}
				pending[call.ID] = true
			}
			if m.Role == "tool" {
				if !pending[m.CallID] {
					t.Error("native result has no preceding call")
				}
				delete(pending, m.CallID)
				resultCount++
				var envelope chatSourceContext
				if json.Unmarshal([]byte(m.Content), &envelope) == nil && envelope.Partial {
					partial = true
				}
			}
		}
		if len(pending) != 0 || resultCount != int(n)-1 {
			t.Error("native search call/result pairs changed")
		}
		w.Header().Set("Content-Type", "application/json")
		if n <= 2 {
			if n == 1 {
				for _, fact := range facts {
					retained := false
					for _, source := range sources {
						retained = retained || strings.Contains(source.Text, fact)
					}
					if retained != automatic {
						t.Errorf("automatic ranked evidence presence=%t, want %t", retained, automatic)
					}
				}
			}
			args, _ := json.Marshal(map[string]string{"text": query})
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": fmt.Sprintf("search-%d", n), "type": "function", "function": map[string]any{"name": "search_passages", "arguments": string(args)}}}}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 500, "completion_tokens": 20}})
			return
		}
		if len(body.Tools) != 0 {
			t.Error("final answer still offered exploratory tools")
		}
		found := 0
		answer := ""
		for _, fact := range facts {
			citation := ""
			for _, source := range sources {
				if strings.Contains(source.Text, fact) {
					citation = source.URL
					break
				}
			}
			if citation != "" {
				found++
				answer += fact + " (" + citation + ")\n"
			} else if limit == 16384 {
				t.Errorf("final request discarded ranked original fact with spare context: %s", fact)
			}
		}
		if limit < 16384 {
			if found == 0 || found == len(facts) || !partial {
				t.Errorf("constrained fixture did not retain useful originals with honest partial evidence: found=%d partial=%t", found, partial)
			}
			answer += "Other originals are omitted from this context; their values remain unverified."
		}
		finalSources, finalAnswer = sources, answer
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 500, "completion_tokens": 20}})
	})
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "owner")
	if err != nil {
		t.Fatal(err)
	}
	for path, full := range originals {
		if _, err = store.Save(t.Context(), path, []byte(full), "", "operator"); err != nil {
			t.Fatal(err)
		}
	}
	connection := app.chat.Config.Connections[0]
	connection.Limits.ContextTokens = limit
	provider := setChatConnection(t, app, connection)
	raw, _ := json.Marshal(map[string]any{"submissionId": "complete-ranked-originals", "text": task, "options": map[string]string{"mode": "ask"}})
	response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", string(raw))
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	done := waitEvidenceRun(t, app, c.ID)
	app.chat.Wait()
	run := done.Runs[0]
	if requests.Load() != 3 || len(run.Operations) != 2 || run.State != "completed" {
		t.Fatalf("search/final answer did not complete: requests=%d operations=%d state=%s error=%s", requests.Load(), len(run.Operations), run.State, run.Error)
	}
	if finalAnswer == "" || done.Turns[len(done.Turns)-1].Content != finalAnswer {
		t.Fatal("grounded final answer not delivered and preserved")
	}
	originalsByID := map[string]chat.Citation{}
	for _, message := range run.Messages {
		for _, source := range modelRequestEvidence(message) {
			originalsByID[source.ID] = source
		}
	}
	passagesByURL := map[string]chat.Citation{}
	for _, op := range run.Operations {
		if op.State != "completed" {
			t.Fatal("search receipt is not completed")
		}
		var result knowledge.RetrievalResult
		if json.Unmarshal(op.Result, &result) != nil || len(result.Passages) != len(facts) {
			t.Fatalf("fixture retrieved %d passages, want %d", len(result.Passages), len(facts))
		}
		for _, fact := range facts {
			found := false
			for _, passage := range result.Passages {
				found = found || strings.Contains(passage.Text, fact)
			}
			if !found {
				t.Fatalf("durable original retrieval lost requested fact: %s", fact)
			}
		}
		for _, passage := range result.Passages {
			if utf8.RuneCountInString(passage.Text) > 1400 {
				t.Fatal("fixture exceeded default bounded retrieval length")
			}
			passagesByURL[passage.Citation] = chat.Citation{ID: passage.ID, Path: passage.Path, URL: passage.Citation, Text: passage.Text, Revision: passage.Revision, Hash: passage.NoteHash, Kind: "committed"}
		}
	}
	for _, source := range finalSources {
		original, ok := originalsByID[source.ID]
		if !ok || source.Path != original.Path || source.URL != original.URL || source.Hash != original.Hash || source.Revision != original.Revision || source.Kind != original.Kind || source.Hash != workspace.ContentHash([]byte(originals[source.Path])) {
			t.Fatal("retained ranked original lost its exact receipt, pinned identity or whole-note hash")
		}
		passage, ok := passagesByURL[source.URL]
		if !ok || original.Text != passage.Text || original.Revision != passage.Revision || original.Hash != passage.Hash {
			t.Fatal("retained citation does not bind the complete original retrieval result")
		}
		if limit == 16384 && source.Text != original.Text {
			t.Fatal("ranked original was shortened despite sufficient context")
		}
		if source.Text != original.Text && (!strings.HasPrefix(original.Text, chatExcerptOriginal(source.Text)) || !strings.Contains(source.Text, "[Omitted from model context;")) {
			t.Fatal("reduced passage lacks an accurate original prefix and omission notice")
		}
	}
	// Restore all 18 occurrences, including duplicates, to prove the ample case
	// never needed prefix caps. The smaller context must genuinely need reduction.
	originalRequest := chat.Request{System: chatSystem, Model: run.Options.Model, MaxOutputTokens: 2048, Messages: append([]chat.Message(nil), run.Messages[:len(run.Messages)-1]...)}
	for i, m := range originalRequest.Messages {
		if m.Role != "tool" {
			continue
		}
		for _, op := range run.Operations {
			if op.ToolCallID == m.ToolCallID {
				var envelope chatSourceContext
				json.Unmarshal([]byte(m.Content), &envelope)
				if len(envelope.Sources) != len(facts) {
					t.Fatal("durable native search result lost selected original sources")
				}
				for j := range envelope.Sources {
					envelope.Sources[j].Text = passagesByURL[envelope.Sources[j].URL].Text
				}
				restored, _ := json.Marshal(envelope)
				originalRequest.Messages[i].Content = string(restored)
			}
		}
	}
	originalRequest = finalChatRequest(originalRequest)
	estimate, err := provider.EstimateRequest(originalRequest)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("full ranked original request input=%d output=2048 context=%d", estimate, limit)
	if (estimate+2048 <= limit) != (limit == 16384) {
		t.Fatal("fixture did not establish its actual full-original budget condition")
	}
}
