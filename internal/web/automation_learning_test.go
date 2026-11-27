package web

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/automationrun"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/connectors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLearningCapturesInteractiveChatButNotBackgroundCalls(t *testing.T) {
	step := 0
	app, _ := learningFixture(t, func(w http.ResponseWriter, r *http.Request) {
		step++
		if step%2 == 1 {
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"query","type":"function","function":{"name":"call_data_operation","arguments":"{\"project\":\"default\",\"connection\":\"db\",\"operation\":\"query\",\"arguments\":{\"sql\":\"select count(*) from examples where id = 7\"}}"}}]},"finish_reason":"tool_calls"}]}`))
		} else {
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Fifteen records."},"finish_reason":"stop"}]}`))
		}
	})
	configureSlackFixture(t, app)
	b := app.slack.config.Channels["C123"]
	b.Connections = []string{"db"}
	app.slack.config.Channels["C123"] = b
	transport := &fixtureChatTransport{}
	app.messageClients["slack"] = transport
	for _, id := range []string{"one", "two"} {
		r := automation.Run{ID: automation.Hash(id), Spec: automation.Spec{ID: "chat-" + id, Kind: "assistant", Project: "default", Actor: "alice", AIConnection: "local", Model: "synthetic", Connections: []string{"db"}, ExternalProcessing: true, Instruction: "Count matching examples.", Delivery: &automation.Delivery{Client: "slack", Workspace: "T123", Channel: "C123", Thread: "thread", User: "U123"}}}
		if err := app.automationrun.RunAutomation(t.Context(), &r, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	// The same read-only data boundary is used by scheduled checks; those must not
	// feed their synthetic usage back into profile learning.
	query := automation.Call{Connection: "db", Operation: "query", Arguments: map[string]any{"sql": "select count(*) from examples where id = 7"}}
	background := automation.Spec{ID: "check", Kind: "checks", Actor: "alice", Project: "default", Connections: []string{"db"}, Checks: []automation.Check{{Name: "count", Call: query, Assertions: []automation.Assertion{{Pointer: "/rows/0/0", Equals: json.RawMessage(`15`)}}}}}
	r := automation.Run{ID: automation.Hash("background"), Spec: background}
	if err := app.automationrun.RunAutomation(t.Context(), &r, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(r.Receipts) != 1 || r.CheckReport == nil || r.CheckReport.State != "healthy" {
		t.Fatalf("background check did not call the data connection: %d receipts, %+v", len(r.Receipts), r.CheckReport)
	}
	runs, err := app.automations.Queue.List()
	if err != nil || len(runs) != 1 || runs[0].Learning == nil || len(runs[0].Learning.Events) != 2 {
		t.Fatalf("learning capture: %+v %v", runs, err)
	}
	for _, e := range runs[0].Learning.Events {
		if e.Intent != "Count matching examples." || e.Actor != "alice" || e.Conversation == "" {
			t.Fatal("lost chat attribution", e)
		}
	}
	if len(transport.replies) != 2 {
		t.Fatal("chat reply missing")
	}
}

func learningFixture(t *testing.T, model http.HandlerFunc) (*Server, automation.Spec) {
	t.Helper()
	app, _ := runnerFixture(t, model)
	app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var args map[string]any
		json.NewDecoder(r.Body).Decode(&args)
		if args["sql"] == "select 0 as count" {
			http.Error(w, "fixture unavailable", http.StatusServiceUnavailable)
			return
		}
		n := 7
		// The fixture interprets its tiny SQL subset independently of production
		// fingerprint/parameter extraction, so a bad binding changes the result.
		if number := regexp.MustCompile(`[0-9]+`).FindString(args["sql"].(string)); number != "" {
			n, _ = strconv.Atoi(number)
		}
		if params, ok := args["params"].([]any); ok && len(params) > 0 {
			n = int(params[0].(float64))
		}
		n = n*2 + 1
		if strings.Contains(args["sql"].(string), "wrong") {
			n = 8
		}
		json.NewEncoder(w).Encode(map[string]any{"rows": [][]int{{n}}})
	}))
	t.Cleanup(upstream.Close)
	m, err := connectors.New(connectors.Config{Version: 1, Connections: []connectors.Connection{{ID: "db", Kind: "http", Endpoint: upstream.URL, ExternalProcessing: true, Operations: []connectors.Operation{{Name: "query", ReadOnly: true, Method: "POST", Path: "/", Body: true, InputSchema: json.RawMessage(`{"type":"object","properties":{"sql":{"type":"string"},"params":{"type":"array","items":{"type":"number"}}},"required":["sql"],"additionalProperties":false}`)}}}}})
	if err != nil {
		t.Fatal(err)
	}
	app.dataConnections = m
	if _, err = app.catalog.SetConnectionAccess(catalog.Bootstrap, catalog.ConnectionGrant{Connection: "db", Users: map[string][]string{"alice": {"query"}}}); err != nil {
		t.Fatal(err)
	}
	if err = app.initAutomations(); err != nil {
		t.Fatal(err)
	}
	spec := automation.Spec{ID: "learn", Kind: "learning", Actor: "alice", Project: "default", Connections: []string{"db"}, AIConnection: "local", Model: "synthetic", ExternalProcessing: true, Learning: &automation.LearningPolicy{CaptureInputs: true, Folder: "learned", DebounceSeconds: 120, MaxWaitSeconds: 900, MinExamples: 2, RetentionHours: 24}}
	setTestAutomationSpec(t, app, spec)
	return app, spec
}

func TestLearningPublishesOnlyReplayValidatedProfiles(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		t.Run(map[bool]string{false: "correct", true: "wrong-results"}[wrong], func(t *testing.T) {
			calls := 0
			app, spec := learningFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				var req struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				json.NewDecoder(r.Body).Decode(&req)
				var input struct {
					Groups map[string]any `json:"groups"`
				}
				json.Unmarshal([]byte(req.Messages[len(req.Messages)-1].Content), &input)
				templates := map[string]any{}
				for key := range input.Groups {
					sql := "select count(*) from examples where id = $1"
					if wrong {
						sql = "select wrong_count()"
					}
					templates[key] = map[string]any{"sql": sql, "params": []any{"{{/literals/0}}"}}
				}
				answer, _ := json.Marshal(map[string]any{"content": "# Count matching examples\n\nUse the observed query pattern and check the identifier filter.", "templates": templates})
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": string(answer)}, "finish_reason": "stop"}}})
			})
			for _, n := range []string{"7", "8"} {
				w := apiCall(app, "POST", "/api/v1/actions/call_data_operation", `{"project":"default","connection":"db","operation":"query","arguments":{"sql":"select count(*) from examples where id=`+n+`"}}`, testToken)
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body)
				}
			}
			runs, err := app.automations.Queue.List()
			if err != nil || len(runs) != 1 {
				t.Fatal(runs, err)
			}
			run := runs[0]
			err = app.automationrun.RunAutomation(context.Background(), &run, func() error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			store, _ := app.catalog.Access("alice", "default", "read")
			found, err := store.Find(context.Background(), "", "recipe", "", spec.Learning.Folder+"/")
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if wrong {
				want = 0
			}
			if len(found) != want || calls != 1 {
				t.Fatalf("profiles=%d want=%d model calls=%d", len(found), want, calls)
			}
			if !wrong {
				first, _ := store.Status(context.Background())
				if err = app.automationrun.RunAutomation(context.Background(), &run, func() error { return nil }); err != nil {
					t.Fatal(err)
				}
				after, _ := store.Status(context.Background())
				if first.Revision != after.Revision || calls != 1 {
					t.Fatal("replay regenerated or recommitted")
				}
			}
		})
	}
}

func TestDataUsageLearnsWithoutAnExistingProfile(t *testing.T) {
	app, _ := learningFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("capture must not call the model") })
	for _, n := range []string{"7", "8"} {
		body := `{"project":"default","connection":"db","operation":"query","arguments":{"sql":"select count(*) from examples where id = ` + n + `"}}`
		w := apiCall(app, "POST", "/api/v1/actions/call_data_operation", body, testToken)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
	}
	runs, err := app.automations.Queue.List()
	if err != nil || len(runs) != 1 || runs[0].Learning == nil || len(runs[0].Learning.Events) != 2 {
		t.Fatalf("unknown query shapes should produce one pending learning batch: %+v %v", runs, err)
	}
	if runs[0].Learning.Events[0].Shape != runs[0].Learning.Events[1].Shape {
		t.Fatal("literal values split equivalent query shapes")
	}
}

func TestLearningReplayUsesNewestSuccessfulSchema(t *testing.T) {
	now := time.Now()
	events := []automation.LearningEvent{
		{ID: "new1", At: now, Complete: true, Outcome: "succeeded", Call: automation.Call{SchemaHash: "v2"}},
		{ID: "old", At: now.Add(-time.Hour), Complete: true, Outcome: "succeeded", Call: automation.Call{SchemaHash: "v1"}},
		{ID: "new2", At: now.Add(time.Second), Complete: true, Outcome: "succeeded", Call: automation.Call{SchemaHash: "v2"}},
	}
	got := automationrun.LearningExamples(events)
	if len(got) != 2 || got[0].ID != "new1" || got[1].ID != "new2" {
		t.Fatal("stale schema entered the reference replay", got)
	}
}

func TestLearningPreservesOriginalFrontmatterBytes(t *testing.T) {
	for _, header := range []string{"---\r\ntags: [recipe]\r\ncustom: retained\r\n---\r\n", "---\ntags: [recipe]\ncustom: retained\n---"} {
		content, err := automationrun.LearningContent([]byte(header), automationrun.LearningCandidate{Content: "# Updated guidance"}, automationrun.LearnedProfile{})
		if err != nil || !strings.HasPrefix(content, header+"\n") {
			t.Fatalf("frontmatter was replaced: %q (%v)", content, err)
		}
	}
}
