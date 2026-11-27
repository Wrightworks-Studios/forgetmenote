package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/decision"
)

func learningAnswer(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	json.NewDecoder(r.Body).Decode(&request)
	var input struct {
		Groups map[string]any `json:"groups"`
	}
	json.Unmarshal([]byte(request.Messages[len(request.Messages)-1].Content), &input)
	templates := map[string]any{}
	for key := range input.Groups {
		templates[key] = map[string]any{"sql": "{{/arguments/sql}}"}
	}
	answer, _ := json.Marshal(map[string]any{"content": "# Observed query workflow\n\nUse the validated data operation and preserve the identifier filter.", "templates": templates})
	json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": string(answer)}, "finish_reason": "stop"}}})
}

func TestLearningSuppliesExactOperationArgumentSchema(t *testing.T) {
	app, _ := learningFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct{ Content string }
			Tools    []json.RawMessage
		}
		json.NewDecoder(r.Body).Decode(&request)
		if len(request.Tools) != 0 {
			t.Error("learning should synthesize from captured evidence without rediscovering it through tools")
		}
		var input struct {
			Groups           map[string]any
			OperationSchemas map[string]map[string]json.RawMessage
			ObservedActivity []struct {
				Arguments map[string]any
				Outcome   string
			}
		}
		json.Unmarshal([]byte(request.Messages[len(request.Messages)-1].Content), &input)
		if len(input.ObservedActivity) != 1 || input.ObservedActivity[0].Arguments["sql"] != "select 0 as count" || input.ObservedActivity[0].Outcome == "succeeded" {
			t.Error("activity must preserve the failed attempt without repeating successful group examples", input.ObservedActivity)
		}
		var schema struct {
			Properties map[string]json.RawMessage
			Required   []string
		}
		json.Unmarshal(input.OperationSchemas["db"]["query"], &schema)
		argument := "query.sql"
		if len(schema.Properties["sql"]) > 0 && len(schema.Required) == 1 && schema.Required[0] == "sql" {
			argument = "sql"
		}
		templates := map[string]any{}
		for key := range input.Groups {
			templates[key] = map[string]any{argument: "{{/arguments/sql}}"}
		}
		answer, _ := json.Marshal(map[string]any{"content": "# Count examples\n\nPreserve the requested identifier filter.", "templates": templates})
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": string(answer)}, "finish_reason": "stop"}}})
	})
	client := actionClient(t, app, "alice", "notes:read data:read")
	for _, n := range []int{0, 7, 8} {
		actionCall(t, client, "call_data_operation", map[string]any{"project": "default", "connection": "db", "operation": "query", "arguments": map[string]any{"sql": fmt.Sprintf("select %d as count", n)}, "learningContext": map[string]string{"conversation": "schema-test", "intent": "Count matching examples"}}, n == 0)
	}
	runs, _ := app.automations.Queue.List()
	run := runs[0]
	if err := app.automationrun.RunAutomation(t.Context(), &run, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(run.Learning.Published) != 1 {
		t.Fatal("profile generation did not receive the operation's exact argument contract", run.Learning.Summary)
	}
}

func TestLearningUsesConfiguredPromptBudget(t *testing.T) {
	for _, budget := range []int{100000, 1} {
		t.Run(fmt.Sprintf("tokens-%d", budget), func(t *testing.T) {
			calls := 0
			app, spec := learningFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				learningAnswer(w, r)
			})
			spec.MaxTokens = budget
			setTestAutomationSpec(t, app, spec)
			config := *app.chat.Config
			config.Connections = append([]chat.Connection(nil), config.Connections...)
			config.Connections[0].Limits.ContextTokens = 100000
			raw, _ := json.Marshal(config)
			file := filepath.Join(t.TempDir(), "chat.json")
			if err := os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := app.ConfigureChat(file); err != nil {
				t.Fatal(err)
			}
			store, _ := app.catalog.Access("alice", "default", "edit")
			content := "---\ntags: [recipe]\n---\n# Count examples\n" + strings.Repeat("Preserve the requested identifier filter.\n", 1600)
			doc, err := store.Save(t.Context(), "learned/count.md", []byte(content), "", "alice")
			if err != nil {
				t.Fatal(err)
			}
			captureLearningExamples(t, app, "count")
			runs, _ := app.automations.Queue.List()
			run := runs[0]
			run.Learning.Decisions = map[string]string{}
			for _, event := range run.Learning.Events {
				run.Learning.Decisions[learningGroup(event)] = doc.Path
			}
			err = app.automationrun.RunAutomation(t.Context(), &run, func() error { return nil })
			after, readErr := store.Read(t.Context(), doc.Path, "")
			if readErr != nil {
				t.Fatal(readErr)
			}
			if budget == 1 {
				if err == nil || err.Error() != "automation token budget reached" || calls != 0 || after.Revision != doc.Revision {
					t.Fatal("exhausted budget generated or published a profile", calls, err, run.Learning.Summary)
				}
				return
			}
			if err != nil || calls != 1 || len(run.Learning.Published) != 1 || len(profileLearningRecord(after.Content).Shapes) != 1 {
				t.Fatal("larger evidence fitting the configured budget was not replay-checked and published", calls, err, run.Learning.Summary)
			}
		})
	}
}

func TestLearningJevConsolidatesMCPActivityBeforeOneModelCall(t *testing.T) {
	for _, repeats := range []int{2, 1} {
		t.Run(fmt.Sprintf("examples-per-shape-%d", repeats), func(t *testing.T) {
			testLearningConsolidation(t, repeats)
		})
	}
}

func testLearningConsolidation(t *testing.T, repeats int) {
	modelCalls := 0
	app, spec := learningFixture(t, func(w http.ResponseWriter, r *http.Request) { modelCalls++; learningAnswer(w, r) })
	spec.Learning.Jev = true
	setTestAutomationSpec(t, app, spec)
	jevcalls := 0
	configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) {
		jevcalls++
		var req struct {
			Questions map[string]decision.Question `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Questions) != repeats {
			t.Errorf("expected %d task judgments, got %d", repeats, len(req.Questions))
		}
		answers := map[string]any{}
		for id, q := range req.Questions {
			choice := "new"
			if id == "q1" {
				choice = "g0"
			}
			probabilities := map[string]float64{}
			for k := range q.Criteria.(map[string]any) {
				probabilities[k] = 0
			}
			probabilities[choice] = 1
			answers[id] = map[string]any{"type": "choice", "choice": choice, "confidence": 1, "probabilities": probabilities}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 60, "output_tokens": 20}})
	})
	client := actionClient(t, app, "alice", "notes:read data:read")
	for _, alias := range []string{"count", "total"} {
		intent := "Count matching examples"
		if repeats == 2 && alias == "total" {
			intent = "Total matching examples"
		}
		for n := 7; n < 7+repeats; n++ {
			sql := fmt.Sprintf("select %d as %s", n, alias)
			actionCall(t, client, "call_data_operation", map[string]any{"project": "default", "connection": "db", "operation": "query", "arguments": map[string]any{"sql": sql}, "learningContext": map[string]string{"conversation": "task-123", "intent": intent}}, false)
		}
	}
	runs, err := app.automations.Queue.List()
	if err != nil || len(runs) != 1 {
		t.Fatal(runs, err)
	}
	run := runs[0]
	if err = app.automationrun.RunAutomation(context.Background(), &run, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	store, _ := app.catalog.Access("alice", "default", "read")
	notes, err := store.Find(context.Background(), "", "recipe", "", "learned/")
	if err != nil || len(notes) != 1 || modelCalls != 1 || jevcalls != 1 {
		t.Fatalf("notes %d model %d Jev %d: %v", len(notes), modelCalls, jevcalls, err)
	}
	if len(run.Learning.Published) != 1 {
		t.Fatal("missing publication receipt")
	}
	if !strings.Contains(runStatus(run).Learning.Summary, "Created: "+notes[0].Path) || strings.Contains(runStatus(run).Learning.Summary, "{{/arguments/sql}}") {
		t.Fatal("missing concise publication result", runStatus(run).Learning)
	}
	doc, err := store.Read(t.Context(), notes[0].Path, "")
	if err != nil || len(profileLearningRecord(doc.Content).Shapes) != 2 {
		t.Fatal("each query variant must retain its own validated template", err)
	}
	// The next client can discover the committed recipe through the same MCP
	// tool used by interactive assistants, without exposing captured activity.
	found := actionCall(t, client, "find_recipes", map[string]any{"project": "default", "query": "Observed query workflow"}, false)
	encoded, _ := json.Marshal(found)
	if !strings.Contains(string(encoded), notes[0].Path) {
		t.Fatal("published profile is not discoverable", string(encoded))
	}
}

func TestLearningRevocationAndSensitiveInputs(t *testing.T) {
	app, _ := learningFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("revoked evidence reached model") })
	credential, err := issueCredential(app.catalog, app.oauth, "alice", CredentialSpec{Name: "learner", Projects: []string{"default"}, Connections: []string{"db"}, Scopes: []string{"notes:read", "data:read"}, Audiences: []string{"api"}, Days: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"select 7", "select 8", "select 'password=private'"} {
		body, _ := json.Marshal(map[string]any{"project": "default", "connection": "db", "operation": "query", "arguments": map[string]any{"sql": sql}})
		if w := apiCall(app, "POST", "/api/v1/actions/call_data_operation", string(body), credential.Token); w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
	}
	runs, err := app.automations.Queue.List()
	if err != nil || len(runs) != 1 || len(runs[0].Learning.Events) != 2 {
		t.Fatal("sensitive call captured", runs, err)
	}
	run := runs[0]
	run.Learning.Prepared = true
	for _, e := range run.Learning.Events {
		run.Learning.Fresh = append(run.Learning.Fresh, learningGroup(e))
	}
	if err = app.revokeCredential("alice", "alice", credential.ID); err != nil {
		t.Fatal(err)
	}
	if err = app.automationrun.RunAutomation(context.Background(), &run, func() error { return nil }); err == nil {
		t.Fatal("revoked credential still authorized learning")
	}
	status, _ := json.Marshal(runStatus(run))
	if strings.Contains(string(status), "select 7") || strings.Contains(string(status), credential.Token) {
		t.Fatal("private evidence leaked through status")
	}
}

func TestLearningExpiryPurgesIdleEvidence(t *testing.T) {
	app, _ := learningFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected model") })
	w := apiCall(app, "POST", "/api/v1/actions/call_data_operation", `{"project":"default","connection":"db","operation":"query","arguments":{"sql":"select 7"}}`, testToken)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	if err := app.automations.Queue.PruneLearning(time.Now().Add(25 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	runs, err := app.automations.Queue.List()
	if err != nil || len(runs) != 0 {
		t.Fatal("expired evidence retained", runs, err)
	}
}

func TestLearningRechecksProfileVisibilityAfterJev(t *testing.T) {
	app, spec := learningFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("profile was sent to the model after being archived")
	})
	spec.Learning.Jev = true
	setTestAutomationSpec(t, app, spec)
	store, _ := app.catalog.Access("alice", "default", "edit")
	doc, err := store.Save(context.Background(), "learned/guide.md", []byte("---\ntags: [recipe]\n---\n# Counts\nCount examples.\n"), "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) {
		if _, err := store.Save(context.Background(), doc.Path, []byte("---\ntags: [recipe]\nfmn-container: archive\n---\n# Counts\n"), doc.Revision, "alice"); err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": map[string]any{"q0": map[string]any{"type": "choice", "choice": "p0", "confidence": 1, "probabilities": map[string]int{"p0": 1, "new": 0, "defer": 0}}}, "usage": map[string]int{"input_tokens": 60, "output_tokens": 20}})
	})
	captureLearningExamples(t, app, "count")
	runs, _ := app.automations.Queue.List()
	run := runs[0]
	if err = app.automationrun.RunAutomation(context.Background(), &run, func() error { return nil }); err == nil {
		t.Fatal("lost source visibility did not stop learning")
	}
}

func TestLearningPreservesConfiguredRecipeContract(t *testing.T) {
	app, _ := learningFixture(t, learningAnswer)
	store, _ := app.catalog.Access("alice", "default", "edit")
	doc, err := store.Save(context.Background(), "learned/guide.md", []byte("---\ntags: [recipe]\n---\n# Counts\n```sql count\nselect 7 as count\n```\n"), "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	setTestAutomationSpec(t, app, automation.Spec{ID: "contract", Kind: "recipe", Actor: "alice", Project: "default", Recipe: doc.Path, Connections: []string{"db"}, Checks: []automation.Check{{Name: "count", Block: "count", Call: automation.Call{Connection: "db", Operation: "query"}, Assertions: []automation.Assertion{{Pointer: "/rows/0/0", Equals: json.RawMessage(`15`)}}}}})
	captureLearningExamples(t, app, "count")
	runs, _ := app.automations.Queue.List()
	run := runs[0]
	run.Learning.Decisions = map[string]string{}
	for _, e := range run.Learning.Events {
		run.Learning.Decisions[learningGroup(e)] = doc.Path
	}
	if err = app.automationrun.RunAutomation(context.Background(), &run, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	after, _ := store.Read(context.Background(), doc.Path, "")
	if after.Revision != doc.Revision || len(run.Learning.Published) != 0 || run.Learning.Deferred != 1 {
		t.Fatal("learning replaced a recipe with a draft that dropped its configured SQL check")
	}
}

func TestLearningRechecksOAuthAPICredential(t *testing.T) {
	app, _ := learningFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("revoked OAuth API input reached the model") })
	token := RandomToken()
	app.oauth.Access[digest(token)] = withOAuthFamily(accessGrant{Client: "api-client", Scope: "notes:read data:read", Resource: app.origin + apiPrefix, Subject: "alice", Epoch: app.catalog.Epoch("alice"), Expires: time.Now().Add(time.Hour).Unix()})
	app.oauth.Refresh[digest(token+"-refresh")] = app.oauth.Access[digest(token)]
	for _, n := range []string{"7", "8"} {
		w := apiCall(app, "POST", "/api/v1/actions/call_data_operation", `{"project":"default","connection":"db","operation":"query","arguments":{"sql":"select `+n+` as count"}}`, token)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
	}
	app.oauth.Mu.Lock()
	app.oauth.RevokeFamily(app.oauth.Access[digest(token)].Family)
	app.oauth.Mu.Unlock()
	runs, err := app.automations.Queue.List()
	if err != nil || len(runs) != 1 {
		t.Fatal(runs, err)
	}
	if err = app.automationrun.RunAutomation(context.Background(), &runs[0], func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestLearningOAuthAuthorizationSurvivesTokenRefresh(t *testing.T) {
	app, _ := learningFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("eligibility test must not call a model") })
	now := time.Now().Unix()
	app.oauth.Refresh[digest("learning-refresh")] = accessGrant{Subject: "alice", Client: "learning-client", Scope: "notes:read data:read", Resource: app.origin + apiPrefix, Family: "learning-family", FamilyExpires: now + 90*86400, Expires: now + 30*86400, Epoch: app.catalog.Epoch("alice")}
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {"learning-client"}, "refresh_token": {"learning-refresh"}}
	pair := decodePair(t, tokenForm(app, "/oauth/token", form))
	for _, n := range []string{"7", "8"} {
		w := apiCall(app, "POST", "/api/v1/actions/call_data_operation", `{"project":"default","connection":"db","operation":"query","arguments":{"sql":"select `+n+` as count"}}`, pair.Access)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
	}
	runs, err := app.automations.Queue.List()
	if err != nil || len(runs) != 1 {
		t.Fatal("missing learning run", err)
	}
	run := runs[0]
	ctx := context.WithValue(t.Context(), identityKey{}, "alice")
	for _, event := range run.Learning.Events {
		if err := app.learningEventAllowed(ctx, &run, event); err != nil {
			t.Fatalf("valid OAuth evidence rejected: %v", err)
		}
	}
	form.Set("refresh_token", pair.Refresh)
	next := decodePair(t, tokenForm(app, "/oauth/token", form))
	app.oauth.Mu.Lock()
	g := app.oauth.Access[digest(pair.Access)]
	g.Expires = now - 1
	app.oauth.Access[digest(pair.Access)] = g
	app.oauth.Mu.Unlock()
	for _, event := range run.Learning.Events {
		if err := app.learningEventAllowed(ctx, &run, event); err != nil {
			t.Fatalf("rotation invalidated evidence: %v", err)
		}
	}
	if err := app.oauth.RevokeToken(next.Refresh); err != nil {
		t.Fatal(err)
	}
	for _, event := range run.Learning.Events {
		if err := app.learningEventAllowed(ctx, &run, event); err == nil {
			t.Fatal("revoked authorization retained evidence access")
		}
	}
}
