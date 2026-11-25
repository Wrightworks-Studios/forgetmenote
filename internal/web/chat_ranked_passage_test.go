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

func TestReviewerRelevantPassageSurvivesReadDedup(t *testing.T) {
	const fact = "The exact approved schedule target is 2039-11-27, and the exception allowance is 41 days."
	full := "# Schedule\n" + strings.Repeat("Unrelated introductory context. ", 100) + "\n## Decision\n" + fact
	cite := chat.Citation{ID: "original", Path: "Schedule.md", URL: "/Schedule.md?fmn-rev=" + strings.Repeat("a", 40), Revision: strings.Repeat("a", 40), Hash: strings.Repeat("b", 64), Kind: "committed", Text: full}
	read, _ := json.Marshal(chatSourceContext{Kind: "note_read", Sources: []chat.Citation{cite}})
	cite.ID = "ranked"
	cite.Text = fact
	cite.URL += "#decision"
	search, _ := json.Marshal(chatSourceContext{Kind: "passage_search", Sources: []chat.Citation{cite}})
	req := chat.Request{MaxOutputTokens: 2048, Messages: []chat.Message{
		{Role: "user", Content: "What is the approved target and exception allowance?"},
		{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "read", Name: "read_note", Arguments: json.RawMessage(`{}`)}, {ID: "search", Name: "search_passages", Arguments: json.RawMessage(`{}`)}}},
		{Role: "tool", ToolCallID: "read", Content: string(read)},
		{Role: "tool", ToolCallID: "search", Content: string(search)},
	}}
	fitted, stats, err := fitChatRequest(chat.Run{Options: chat.Options{Mode: "ask"}}, req, 16384)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(fitted)
	t.Logf("before=%d after=%d hard=14336 sources_before=%d sources_after=%d fact_retained=%t", stats.BeforeTokens, stats.AfterTokens, stats.SourcesBefore, stats.SourcesAfter, strings.Contains(string(raw), fact))
	if !strings.Contains(string(raw), fact) {
		t.Fatal("Dedup discarded the ranked original fact, then prefix clipping discarded the same fact from the retained read, despite spare context")
	}
}

func TestChatRankedMiddleAndEndPassagesReachFinalRequest(t *testing.T) {
	for _, limit := range []int{16384, 6144} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			const middle = "The exact approved schedule target is 2039-11-27, and the exception allowance is 41 days."
			const ending = "The final validation code is CERULEAN-73."
			full := "# Schedule\n" + strings.Repeat("Unrelated introductory context. ", 100) + "\n## Decision\n" + middle + "\n\n" + strings.Repeat("Unrelated middle calibration background. ", 100) + "\n## Final validation\n" + ending + "\n"
			var requests atomic.Int32
			revision := ""
			app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []struct {
						Role    string `json:"role"`
						Content string `json:"content"`
						CallID  string `json:"tool_call_id"`
						Calls   []struct {
							ID string `json:"id"`
						} `json:"tool_calls"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/json")
				if requests.Add(1) == 1 {
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"read-original","type":"function","function":{"name":"read_note","arguments":"{\"path\":\"notes/schedule.md\"}"}},{"id":"ranked-originals","type":"function","function":{"name":"search_passages","arguments":"{\"text\":\"approved schedule target exception allowance final validation\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":500,"completion_tokens":50}}`)
					return
				}
				all := ""
				pins := map[string]bool{}
				pending := map[string]bool{}
				for _, message := range body.Messages {
					all += message.Content
					// Shared evidence may retain the fuller original in the
					// initial envelope while its duplicate tool result is an ack.
					if message.Role == "user" {
						const opening = "\n<untrusted_notebook_context>\n"
						const closing = "\n</untrusted_notebook_context>"
						start := strings.LastIndex(message.Content, opening)
						if start >= 0 && strings.HasSuffix(message.Content, closing) {
							var payload struct {
								Sources []chat.Citation `json:"sources"`
							}
							if json.Unmarshal([]byte(message.Content[start+len(opening):len(message.Content)-len(closing)]), &payload) == nil {
								for _, source := range payload.Sources {
									if strings.Contains(source.Text, middle) || strings.Contains(source.Text, ending) {
										if source.Path != "notes/schedule.md" || source.Revision != revision || source.Hash != workspace.ContentHash([]byte(full)) || !strings.Contains(source.URL, "fmn-rev="+revision) {
											t.Error("initial ranked original lost its exact pinned revision/hash")
										}
										if strings.Contains(source.Text, middle) {
											pins["middle"] = pins["middle"] || strings.Contains(source.URL, "#decision")
										}
										if strings.Contains(source.Text, ending) {
											pins["ending"] = pins["ending"] || strings.Contains(source.URL, "#final-validation")
										}
									}
								}
							}
						}
					}
					for _, call := range message.Calls {
						pending[call.ID] = true
					}
					if message.Role == "tool" {
						if !pending[message.CallID] {
							t.Error("fitting orphaned a native result")
						}
						delete(pending, message.CallID)
						var evidence chatSourceContext
						if json.Unmarshal([]byte(message.Content), &evidence) == nil {
							for _, source := range evidence.Sources {
								if strings.Contains(source.Text, middle) || strings.Contains(source.Text, ending) {
									if source.Path != "notes/schedule.md" || source.Revision != revision || source.Hash != workspace.ContentHash([]byte(full)) || !strings.Contains(source.URL, "fmn-rev="+revision) {
										t.Error("ranked original lost its exact pinned revision/hash")
									}
									if strings.Contains(source.Text, middle) {
										pins["middle"] = pins["middle"] || strings.Contains(source.URL, "#decision")
									}
									if strings.Contains(source.Text, ending) {
										pins["ending"] = pins["ending"] || strings.Contains(source.URL, "#final-validation")
									}
								}
							}
						}
					}
				}
				if len(pending) != 0 {
					t.Error("fitting dropped a native result")
				}
				if !strings.Contains(all, middle) || !strings.Contains(all, ending) || !pins["middle"] || !pins["ending"] {
					t.Errorf("ranked originals disappeared after read clipping: context=%d middle=%v ending=%v pins=%v", limit, strings.Contains(all, middle), strings.Contains(all, ending), pins)
				}
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The original decision records 2039-11-27 and 41 days; the final validation is CERULEAN-73."},"finish_reason":"stop"}],"usage":{"prompt_tokens":700,"completion_tokens":40}}`)
			})
			store, err := app.catalog.Access(catalog.Bootstrap, "default", "owner")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.Save(t.Context(), "notes/schedule.md", []byte(full), "", "operator"); err != nil {
				t.Fatal(err)
			}
			original, err := store.Read(t.Context(), "notes/schedule.md", "")
			if err != nil {
				t.Fatal(err)
			}
			revision = original.Revision
			connection := app.chat.Config.Connections[0]
			connection.Limits.ContextTokens = limit
			setChatConnection(t, app, connection)
			response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"ranked-originals","text":"What approved schedule target, exception allowance and final validation do the original passages establish?","options":{"mode":"ask"}}`)
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body)
			}
			done := waitChatRun(t, app, c.ID)
			app.chat.Wait()
			run := done.Runs[0]
			if run.State != "completed" || requests.Load() != 2 || len(run.Operations) != 2 {
				t.Fatalf("original read/search round did not finish: state=%s error=%q requests=%d operations=%d", run.State, run.Error, requests.Load(), len(run.Operations))
			}
			var retained noteOutput
			if json.Unmarshal(run.Operations[0].Result, &retained) != nil || retained.Content != full || retained.HTML == "" || !strings.Contains(string(run.Operations[1].Result), middle) || !strings.Contains(string(run.Operations[1].Result), ending) {
				t.Fatal("fixture did not retrieve or preserve the full ranked originals")
			}
		})
	}
}

func TestRankedFactSurvivesLaterReadPrefixBudgetRetries(t *testing.T) {
	const fact = "The retained original decision is 2039-11-27 with a 41-day exception allowance."
	full := strings.Repeat("Introductory context. ", 60) + fact + strings.Repeat(" Additional original tail.", 50)
	cite := chat.Citation{ID: "broad-read", Kind: "committed", Path: "notes/schedule.md", URL: "/notes/schedule.md?fmn-rev=" + strings.Repeat("a", 40), Revision: strings.Repeat("a", 40), Hash: strings.Repeat("b", 64), Text: full}
	read, _ := json.Marshal(chatSourceContext{Kind: "note_read", Sources: []chat.Citation{cite}})
	precise := cite
	precise.ID = "ranked-match"
	precise.URL += "#decision"
	precise.Text = fact
	search, _ := json.Marshal(chatSourceContext{Kind: "passage_search", Sources: []chat.Citation{precise}})
	request := chat.Request{System: strings.Repeat("Mandatory stable instruction. ", 120), MaxOutputTokens: 2048, Messages: []chat.Message{{Role: "user", Content: "What original decision and exception apply?"}, {Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "read", Name: "read_note", Arguments: json.RawMessage(`{}`)}, {ID: "search", Name: "search_passages", Arguments: json.RawMessage(`{}`)}}}, {Role: "tool", ToolCallID: "read", Content: string(read)}, {Role: "tool", ToolCallID: "search", Content: string(search)}}}
	fitted, fit, err := fitChatRequest(chat.Run{Options: chat.Options{Mode: "ask"}}, request, 4096)
	if err != nil {
		t.Fatal(err)
	}
	var broad, ranked chatSourceContext
	json.Unmarshal([]byte(fitted.Messages[2].Content), &broad)
	json.Unmarshal([]byte(fitted.Messages[3].Content), &ranked)
	if len(broad.Sources) != 1 || len(broad.Sources[0].Text) >= 1200 || strings.Contains(broad.Sources[0].Text, fact) {
		t.Fatalf("fixture did not exercise a tighter subsequent read-prefix budget: fit=%+v sources=%d", fit, len(broad.Sources))
	}
	if len(ranked.Sources) != 1 || ranked.Sources[0].Text != fact || ranked.Sources[0].URL != precise.URL || ranked.Sources[0].Hash != precise.Hash || fitted.MaxOutputTokens != 2048 || fit.AfterTokens+2048 > 4096 {
		t.Fatal("later clipping discarded the only retained ranked fact or its pinned guard")
	}
	if request.Messages[2].Content != string(read) || request.Messages[3].Content != string(search) {
		t.Fatal("fitting changed original native result messages")
	}
}
