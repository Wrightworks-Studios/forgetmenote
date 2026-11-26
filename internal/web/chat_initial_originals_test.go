package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/chatrun"
)

// Exercise the ordinary admission fit followed immediately by the smaller,
// tool-free native request. Its evidence must come from the original retrieval,
// not a previously fitted excerpt or unrelated citations from a later round.
func TestChatInitialOriginalsSurviveFinalRequestRefit(t *testing.T) {
	for _, limit := range []int{4096, 8192, 16384} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			var actual []chat.Message
			var requests atomic.Int32
			app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				var body struct {
					Messages  []chat.Message    `json:"messages"`
					Tools     []json.RawMessage `json:"tools"`
					MaxTokens int               `json:"max_tokens"`
				}
				if err = json.Unmarshal(raw, &body); err != nil {
					t.Error(err)
				}
				actual = body.Messages
				if len(body.Tools) != 0 {
					t.Error("expected immediate final-answer request within one-round allowance")
				}
				if body.MaxTokens != 2048 || (len(raw)+2)/3+body.MaxTokens > limit {
					t.Errorf("native context/output budget exceeded: input bytes=%d output=%d limit=%d", len(raw), body.MaxTokens, limit)
				}
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Synthetic answer."},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`)
			})
			store, err := app.catalog.Access(catalog.Bootstrap, "default", "owner")
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 9; i++ {
				full := fmt.Sprintf("# Calibration %d\n", i) + strings.Repeat("Instrument background. ", 43) + fmt.Sprintf("The flux cutoff for specimen %d is VIOLET-%02d.\n", i, i)
				if _, err = store.Save(t.Context(), fmt.Sprintf("notes/calibration-%d.md", i), []byte(full), "", "operator"); err != nil {
					t.Fatal(err)
				}
			}
			connection := app.chat.Config.Connections[0]
			connection.Limits.ContextTokens = limit
			connection.Limits.MaxRounds = 1
			provider := setChatConnection(t, app, connection)
			const task = "Describe the calibration flux cutoff"
			body := map[string]any{"submissionId": "initial-final-original", "text": task, "options": map[string]any{"mode": "ask"}}
			attachment := ""
			if limit == 16384 {
				attachment = strings.Repeat("Explicit background. ", 1550)
				if len(attachment) != 32550 {
					t.Fatal("wrong attachment fixture length")
				}
				body["attachments"] = []chat.Attachment{{Kind: "private", Path: "notes/calibration-0.md", Content: attachment}}
			}
			raw, _ := json.Marshal(body)
			response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", string(raw))
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body)
			}
			done := waitEvidenceRun(t, app, c.ID)
			app.chat.Wait()
			run := done.Runs[0]
			if run.State != "completed" || requests.Load() != 1 {
				t.Fatalf("state=%s error=%s requests=%d", run.State, run.Error, requests.Load())
			}
			messages := append([]chat.Message(nil), run.Messages[:len(run.Messages)-1]...)
			index, prefix, payload, _ := chatrun.OptionalSources(messages, false)
			var durableSources []chat.Citation
			if err := json.Unmarshal(payload["sources"], &durableSources); err != nil {
				t.Fatal(err)
			}
			if index < 0 || !strings.HasPrefix(prefix, task+"\n\n<notebook_guidance>") {
				t.Fatal("original task/guidance envelope lost")
			}
			if !reflect.DeepEqual(durableSources, run.Citations) {
				t.Error("durable initial envelope lost exact original source content or identity during admission")
			}
			if len(run.Citations) == 0 {
				t.Fatal("fixture retrieved no originals")
			}
			// All citations here came from this run's initial retrieval: the provider made
			// no tool calls. Build the exact original-only control for the same fitter.
			for _, source := range run.Citations {
				note, err := store.Read(t.Context(), source.Path, source.Revision)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(note.Content), source.Text) || source.Hash != chat.Hash(note.Content) || source.URL == "" || source.ID == "" {
					t.Fatal("original citation lost its pinned note identity")
				}
			}
			payload["sources"], _ = json.Marshal(run.Citations)
			encoded, _ := json.Marshal(payload)
			messages[index].Content = prefix + "\n<untrusted_notebook_context>\n" + string(encoded) + "\n</untrusted_notebook_context>"
			req := chatrun.FinalRequest(chat.Request{System: chatrun.System, Model: run.Options.Model, Messages: messages, Tools: app.chatrun.RunTools(run, connection), MaxOutputTokens: connection.Limits.OutputTokens})
			restored, stats, err := chatrun.FitRequest(run, req, limit, provider.EstimateRequest)
			if err != nil {
				t.Fatal(err)
			}
			sources := func(messages []chat.Message) []chat.Citation {
				var out []chat.Citation
				for _, m := range messages {
					out = append(out, modelRequestEvidence(m)...)
				}
				return out
			}
			got, want := sources(actual), sources(restored.Messages)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("final native request did not recover the exact original excerpts/identities that fit its budget: got=%d sources want=%d", len(got), len(want))
			}
			facts := func(sources []chat.Citation) int {
				n := 0
				for _, s := range sources {
					if strings.Contains(s.Text, "VIOLET-") {
						n++
					}
				}
				return n
			}
			t.Logf("context=%d attachmentBytes=%d actualFacts=%d same-budget originalFacts=%d input=%d originals=%d", limit, len(attachment), facts(got), facts(want), stats.AfterTokens, len(run.Citations))
			if facts(want) == 0 || facts(got) != facts(want) {
				t.Error("recoverable complete facts missing from final request")
			}
			if stats.AfterTokens+connection.Limits.OutputTokens > limit {
				t.Fatal("control exceeded real native budget")
			}
			if attachment != "" {
				_, _, sent, _ := chatrun.OptionalSources(actual, false)
				var attachments []chat.Attachment
				if json.Unmarshal(sent["explicitAttachments"], &attachments) != nil || len(attachments) != 1 || attachments[0].Content != attachment || attachments[0].Hash != chat.Hash([]byte(attachment)) {
					t.Fatal("mandatory private attachment changed during fitting")
				}
			}
			public := proxyCall(app, "alice", "GET", "/p/default/api/chat/"+c.ID, "")
			var snapshot chat.Conversation
			if public.Code != 200 || json.Unmarshal(public.Body.Bytes(), &snapshot) != nil || len(snapshot.Runs[0].Messages) != 0 {
				t.Fatal("original native envelope escaped private storage")
			}
		})
	}
}

func TestChatIntermediateIsPublicProjectionOnly(t *testing.T) {
	var requests atomic.Int32
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role   string `json:"role"`
				CallID string `json:"tool_call_id"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if requests.Add(1) == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Checking the original.","tool_calls":[{"id":"original-read","type":"function","function":{"name":"read_note","arguments":"{\"path\":\"notes/calibration.md\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`)
			return
		}
		matched := 0
		for _, m := range body.Messages {
			if m.Role == "tool" && m.CallID == "original-read" {
				matched++
			}
		}
		if matched != 1 {
			t.Error("native result pair not retained")
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The calibration is stable."},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`)
	})
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(t.Context(), "notes/calibration.md", []byte("# Calibration\nThe calibration is stable.\n"), "", "operator"); err != nil {
		t.Fatal(err)
	}
	response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"projection-only","text":"Read the calibration original","options":{"mode":"ask"}}`)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	done := waitChatRun(t, app, c.ID)
	app.chat.Wait()
	if done.Runs[0].State != "completed" || requests.Load() != 2 || len(done.Turns) != 3 {
		t.Fatal("tool run did not complete")
	}
	var raw []byte
	if err := app.db.Pool.QueryRow(t.Context(), `SELECT record FROM chat_conversations WHERE org = $1 AND account = $2 AND id = $3`, app.db.Org, chat.Hash([]byte("alice")), c.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var record struct {
		Turns []map[string]json.RawMessage `json:"turns"`
	}
	if err = json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	for _, turn := range record.Turns {
		if _, exists := turn["intermediate"]; exists {
			t.Error("API-only intermediate flag persisted into the strict legacy record")
		}
	}
	_, _, initialPayload, _ := chatrun.OptionalSources(done.Runs[0].Messages, false)
	var initialSources []chat.Citation
	if json.Unmarshal(initialPayload["sources"], &initialSources) != nil || len(initialSources) != 1 || initialSources[0].ID != "source-1" || len(done.Runs[0].Citations) <= len(initialSources) {
		t.Fatal("later tool citations replaced or contaminated the original automatic envelope")
	}
	if done.Turns[1].Provider == nil || len(done.Turns[1].Provider.ToolCalls) != 1 || done.Turns[1].Provider.ToolCalls[0].ID != "original-read" {
		t.Fatal("durable native planning state lost")
	}
	public := proxyCall(app, "alice", "GET", "/p/default/api/chat/"+c.ID, "")
	var snapshot chat.Conversation
	if public.Code != 200 || json.Unmarshal(public.Body.Bytes(), &snapshot) != nil || len(snapshot.Turns) != 3 || !snapshot.Turns[1].Intermediate || snapshot.Turns[2].Intermediate || snapshot.Turns[1].Provider != nil {
		t.Fatal("public intermediate projection missing or exposed native state")
	}
	assistantEvents := 0
	for _, event := range done.Events {
		if event.Type == "assistant" {
			var projection chat.Turn
			if err = json.Unmarshal(event.Data, &projection); err != nil {
				t.Fatal(err)
			}
			if assistantEvents == 0 && !projection.Intermediate {
				t.Error("stream lost intermediate flag")
			}
			assistantEvents++
		}
	}
	if assistantEvents != 2 {
		t.Fatal("missing durable assistant events")
	}
}
