package web

import (
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/workspace"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func chatQueryEmbeddingFixture(t *testing.T, app *Server, handler func(http.ResponseWriter, *http.Request, string)) *atomic.Int32 {
	t.Helper()
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "owner")
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"notes/bicycle.md": "# Bicycle workshop\nChain maintenance requires CERULEAN lubricant and brake inspection.", "notes/office.md": "# Office\nTell me about the attendance roster and the gate options."} {
		if _, err = store.Save(t.Context(), name, []byte(text), "", "operator"); err != nil {
			t.Fatal(err)
		}
	}
	e, releaseEngine, err := app.acquireEngine(t.Context(), store)
	if releaseEngine != nil {
		t.Cleanup(releaseEngine)
	}

	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Configure(t.Context(), knowledge.Config{Enabled: true, Endpoint: true, Model: "synthetic", Dimensions: 3, PassageRunes: 512, Splitter: "markdown-heading-v1"}, false); err != nil {
		t.Fatal(err)
	}
	pending, err := e.Pending(t.Context(), 32)
	if err != nil {
		t.Fatal(err)
	}
	submission := knowledge.Submission{Revision: pending.Status.Revision, ConfigID: pending.Status.ConfigID}
	for _, p := range pending.Passages {
		vector := []float32{0, 1, 0}
		if p.Path == "notes/bicycle.md" {
			vector = []float32{1, 0, 0}
		}
		submission.Vectors = append(submission.Vectors, knowledge.VectorSubmission{ID: p.ID, Hash: p.Hash, NoteHash: p.NoteHash, Vector: vector})
	}
	if _, err = e.Submit(t.Context(), submission); err != nil {
		t.Fatal(err)
	}
	calls := &atomic.Int32{}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Input) != 1 {
			t.Error("unexpected embedding input batch")
			http.Error(w, "fixture", 500)
			return
		}
		calls.Add(1)
		handler(w, r, body.Input[0])
	}))
	t.Cleanup(endpoint.Close)
	if err = app.ConfigureEmbeddings(endpoint.URL+"/api/embed", ""); err != nil {
		t.Fatal(err)
	}
	return calls
}

func TestChatEmptySemanticQuestionRecoversOnceBeforeAnswer(t *testing.T) {
	var models atomic.Int32
	var gotSource chat.Citation
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		models.Add(1)
		var body struct {
			Messages []chat.Message `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid model request")
		}
		for _, m := range body.Messages {
			for _, source := range modelRequestEvidence(m) {
				if strings.Contains(source.Text, "CERULEAN lubricant") {
					gotSource = source
				}
			}
		}
		if gotSource.URL == "" {
			t.Error("model was asked to answer before the bounded focused query recovered the original")
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "The workshop specifies CERULEAN lubricant (" + gotSource.URL + ")."}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 20}})
	})
	calls := chatQueryEmbeddingFixture(t, app, func(w http.ResponseWriter, r *http.Request, text string) {
		vector := []float32{0, 0, 1}
		if text == "two wheeler upkeep" {
			vector = []float32{1, 0, 0}
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{vector}})
	})
	response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"recover-question","text":"Could you tell me about two-wheeler upkeep?","options":{"mode":"ask"}}`)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	done := waitChatRun(t, app, c.ID)
	app.chat.Wait()
	run := done.Runs[0]
	if run.State != "completed" || calls.Load() != 2 || models.Load() != 1 || len(run.Operations) != 0 {
		t.Fatalf("recovery must be bounded and precede one model answer: state=%s embeddings=%d models=%d tools=%d error=%s", run.State, calls.Load(), models.Load(), len(run.Operations), run.Error)
	}
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "read")
	if err != nil {
		t.Fatal(err)
	}
	original, err := store.Read(t.Context(), gotSource.Path, gotSource.Revision)
	if err != nil || workspace.ContentHash(original.Content) != gotSource.Hash || !strings.Contains(gotSource.URL, "fmn-rev="+gotSource.Revision) {
		t.Fatal("recovered source lost its committed identity/hash", err)
	}
	if !strings.Contains(done.Turns[len(done.Turns)-1].Content, "CERULEAN") || run.Usage.Requests != 1 {
		t.Fatal("answer/usage not durable")
	}
}

func TestChatEmbeddingTimeoutReservesAnswerAndDoesNotRetry(t *testing.T) {
	var models atomic.Int32
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		models.Add(1)
		var raw json.RawMessage
		json.NewDecoder(r.Body).Decode(&raw)
		if !strings.Contains(string(raw), "CERULEAN lubricant") || !strings.Contains(string(raw), "unavailable") {
			t.Error("bounded timeout lost truthful lexical fallback")
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The committed chain maintenance instruction specifies CERULEAN lubricant."},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20}}`)
	})
	app.chat.Config.Connections[0].Limits.RunSeconds = 9
	calls := chatQueryEmbeddingFixture(t, app, func(w http.ResponseWriter, r *http.Request, text string) { <-r.Context().Done() })
	started := time.Now()
	response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"bounded-cold-wait","text":"Tell me about chain maintenance","options":{"mode":"ask"}}`)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	done := waitEvidenceRun(t, app, c.ID)
	app.chat.Wait()
	if done.Runs[0].State != "completed" || calls.Load() != 1 || models.Load() != 1 || time.Since(started) > 7*time.Second {
		t.Fatalf("retrieval consumed the answer allowance or retried timeout: state=%s calls=%d models=%d elapsed=%v", done.Runs[0].State, calls.Load(), models.Load(), time.Since(started))
	}
}

func TestChatCancelDuringColdEmbeddingStopsBeforeModel(t *testing.T) {
	var models atomic.Int32
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) { models.Add(1); http.Error(w, "unexpected", 500) })
	started := make(chan struct{})
	calls := chatQueryEmbeddingFixture(t, app, func(w http.ResponseWriter, r *http.Request, text string) { close(started); <-r.Context().Done() })
	response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"cancel-cold-wait","text":"Tell me about two-wheeler upkeep","options":{"mode":"ask"}}`)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	var start struct {
		Run string `json:"run"`
	}
	json.Unmarshal(response.Body.Bytes(), &start)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("embedding request did not start")
	}
	response = proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/runs/"+start.Run+"/cancel", `{}`)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	done := waitChatRun(t, app, c.ID)
	app.chat.Wait()
	if done.Runs[0].State != "cancelled" || models.Load() != 0 || calls.Load() != 1 {
		t.Fatal("cancelled cold retrieval continued to model or retried")
	}
}

func TestChatFocusedRecoverySharesOriginalEmbeddingAllowance(t *testing.T) {
	var models atomic.Int32
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		models.Add(1)
		var raw json.RawMessage
		json.NewDecoder(r.Body).Decode(&raw)
		if !strings.Contains(string(raw), "Weak topical coverage") || !strings.Contains(string(raw), "absence") {
			t.Error("exhausted recovery lost the missing-evidence limitation")
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The selected evidence does not establish the requested maintenance details."},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20}}`)
	})
	app.chat.Config.Connections[0].Limits.RunSeconds = 9
	secondWait := make(chan time.Duration, 1)
	calls := chatQueryEmbeddingFixture(t, app, func(w http.ResponseWriter, r *http.Request, text string) {
		if strings.HasPrefix(text, "Could") {
			timer := time.NewTimer(2 * time.Second)
			defer timer.Stop()
			select {
			case <-r.Context().Done():
				return
			case <-timer.C:
			}
			json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{0, 0, 1}}})
			return
		}
		began := time.Now()
		<-r.Context().Done()
		secondWait <- time.Since(began)
	})
	response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"one-total-retrieval-allowance","text":"Could you tell me about two-wheeler upkeep?","options":{"mode":"ask"}}`)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	done := waitEvidenceRun(t, app, c.ID)
	app.chat.Wait()
	if done.Runs[0].State != "completed" || calls.Load() != 2 || models.Load() != 1 {
		t.Fatal("bounded recovery failed to leave time for an honest answer")
	}
	select {
	case elapsed := <-secondWait:
		if elapsed >= 2*time.Second {
			t.Fatalf("focused query received a fresh allowance: %v", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("second embedding request did not stop")
	}
}

func TestChatRetrievalDiagnosticOmitsPrivateQuestionAndEndpoint(t *testing.T) {
	var logs lockedLogBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	const private = "SYNTHETIC_PRIVATE_QUESTION_AND_EMBEDDING_ERROR"
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The selected originals do not establish that detail."},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20}}`)
	})
	calls := chatQueryEmbeddingFixture(t, app, func(w http.ResponseWriter, r *http.Request, text string) {
		http.Error(w, private, http.StatusServiceUnavailable)
	})
	response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"private-retrieval-diagnostic","text":"Tell me about `+private+`","options":{"mode":"ask"}}`)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	done := waitChatRun(t, app, c.ID)
	app.chat.Wait()
	if done.Runs[0].State != "completed" || calls.Load() != 1 {
		t.Fatal("fixture failed to complete bounded unavailable-embedding fallback")
	}
	records := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		if event["msg"] == "chat retrieval" {
			records++
			if event["chat"] != c.ID || event["run"] != done.Runs[0].ID || event["phase"] != "initial" || event["retrieval_mode"] != "bm25" || event["semantic"] != "unavailable" || event["queries"] != float64(1) || event["weak"] != true || event["passages"] != float64(0) || event["elapsed_ms"] == nil {
				t.Errorf("missing structural retrieval outcome: %+v", event)
			}
		}
	}
	if records != 1 {
		t.Fatalf("HTTP200 concealed retrieval fallback: records=%d", records)
	}
	for _, forbidden := range []string{private, "notes/office.md", "notes/bicycle.md", "CERULEAN", "127.0.0.1", "/api/embed"} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatalf("retrieval diagnostic exposed private data: %s", forbidden)
		}
	}
}

func TestChatToolRetrievalKeepsWeakEvidenceWarningAfterFitting(t *testing.T) {
	var models atomic.Int32
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []chat.Message `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if models.Add(1) == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"warranty-search","type":"function","function":{"name":"search_passages","arguments":"{\"text\":\"bicycle warranty coverage\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":20}}`)
			return
		}
		warned := false
		for _, m := range body.Messages {
			if m.Role == "tool" && strings.Contains(m.Content, `"weakEvidence":true`) && strings.Contains(m.Content, "absence") {
				warned = true
			}
		}
		if !warned {
			t.Error("fitting discarded the tool's weak-topic/absence limitation")
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The workshop note establishes brake inspections, but the retained evidence does not establish warranty coverage."},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20}}`)
	})
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(t.Context(), "notes/bicycle.md", []byte("# Bicycle workshop\nBrakes require ordinary inspections."), "", "operator"); err != nil {
		t.Fatal(err)
	}
	response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"weak-tool-warning","text":"Bicycle workshop","options":{"mode":"ask"}}`)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	done := waitChatRun(t, app, c.ID)
	app.chat.Wait()
	if done.Runs[0].State != "completed" || models.Load() != 2 || len(done.Runs[0].Operations) != 1 {
		t.Fatal("weak tool retrieval did not finish its bounded answer")
	}
	if !strings.Contains(string(done.Runs[0].Operations[0].Result), `"weak":true`) {
		t.Fatal("fixture failed to retrieve and retain a partial-topic result")
	}
}
