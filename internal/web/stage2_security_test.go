package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/knowledge"
)

func TestSecurityLocalOnlyDiscoveredSearchCannotSendPrivateQuery(t *testing.T) {
	const privateQuery = "private-chat-query-must-stay-local"
	var querySent atomic.Bool
	embedding := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		vectors := make([][]float32, len(in.Input))
		for i, text := range in.Input {
			vectors[i] = []float32{1, 0, 0}
			if strings.Contains(text, privateQuery) {
				querySent.Store(true)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	}))
	defer embedding.Close()
	var rounds atomic.Int32
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var in any
		json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		var name, args string
		switch rounds.Add(1) {
		case 1:
			name = "discover_actions"
			args = `{"names":["semantic_search"]}`
		case 2:
			name = "semantic_search"
			args = `{"query":{"text":"` + privateQuery + `"}}`
		default:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": fmt.Sprint(rounds.Load()), "type": "function", "function": map[string]any{"name": name, "arguments": args}}}}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5}})
	})
	// Configure the application embedding service but do not declare it local for chat.
	if err := app.ConfigureEmbeddings(embedding.URL+"/api/embed", ""); err != nil {
		t.Fatal(err)
	}
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(context.Background(), "notes/a.md", []byte("Public synthetic evidence."), "", catalog.Bootstrap); err != nil {
		t.Fatal(err)
	}
	e, releaseEngine, err := app.acquireEngine(t.Context(), store)
	if releaseEngine != nil {
		t.Cleanup(releaseEngine)
	}

	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Configure(context.Background(), knowledge.Config{Enabled: true, Endpoint: true, Model: "synthetic", Dimensions: 3, PassageRunes: 512, Splitter: "markdown-heading-v1"}, false); err != nil {
		t.Fatal(err)
	}
	app.chat.Mu.Lock()
	cfg := *app.chat.Config
	app.chat.Mu.Unlock()
	cfg.LocalOnly = true
	cfg.EmbeddingsLocal = false
	path := filepath.Join(t.TempDir(), "local-only.json")
	b, _ := json.Marshal(cfg)
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err = app.ConfigureChat(path); err != nil {
		t.Fatal(err)
	}
	w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"local-test","text":"Find evidence","options":{"mode":"ask"}}`)
	if w.Code != 200 {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	done := waitChatRun(t, app, c.ID)
	if done.Runs[0].State != "completed" {
		t.Fatalf("run did not execute fixture: %+v", done.Runs[0])
	}
	if querySent.Load() {
		t.Fatal("localOnly=true, embeddingsLocal=false nevertheless sent the private model tool query to the configured embedding endpoint")
	}
}

func TestHTTPAdmissionIgnoresLegacyTokenQuotas(t *testing.T) {
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var in any
		json.NewDecoder(r.Body).Decode(&in)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	})
	app.chat.Mu.Lock()
	cfg := *app.chat.Config
	cfg.Connections = append([]chat.Connection(nil), cfg.Connections...)
	app.chat.Mu.Unlock()
	cfg.Limits.DailyTokens = 250000
	cfg.Connections[0].Limits.DailyTokens = 10000
	path := filepath.Join(t.TempDir(), "daily-limit.json")
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.ConfigureChat(path); err != nil {
		t.Fatal(err)
	}
	// Existing configuration remains readable, but its token ceilings have no effect.
	w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", `{"submissionId":"daily-limit","text":"Find evidence","options":{"mode":"ask","budgetTokens":1}}`)
	if w.Code != 200 {
		t.Fatalf("legacy quota blocked admission: HTTP %d %s", w.Code, w.Body)
	}
	done := waitChatRun(t, app, c.ID)
	app.chat.Wait()
	if done.Runs[0].State != "completed" || done.Runs[0].Usage.Tokens() != 15 {
		t.Fatalf("legacy quota stopped work or lost usage: %+v", done.Runs[0])
	}
}
