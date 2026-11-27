package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/chatrun"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/reposource"
	"forgetmenote/internal/workspace"
)

func TestChatDeniedReadPreservesOtherReadsAndNotebookIdentity(t *testing.T) {
	var requests atomic.Int32
	app, conversation := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if requests.Add(1) == 1 {
			var request struct {
				Messages []chat.Message `json:"messages"`
			}
			json.Unmarshal(raw, &request)
			contextText := request.Messages[len(request.Messages)-1].Content
			if !strings.Contains(contextText, `"currentProject":"default"`) || !strings.Contains(contextText, `"project":"default"`) {
				t.Error("model cannot distinguish current notebook evidence from other selected notebooks")
			}
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"denied","type":"function","function":{"name":"read_note","arguments":"{\"project\":\"other\",\"path\":\"Ops.md\"}"}},{"id":"valid","type":"function","function":{"name":"read_document","arguments":"{\"project\":\"other\",\"origin\":\"notes\",\"path\":\"Ops.md\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		if !strings.Contains(string(raw), "SCOPED_PEER_CONTENT") {
			t.Error("valid read after a rejected read did not reach the model")
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Answered from the permitted original."},"finish_reason":"stop"}]}`)
	})
	if err := app.catalog.Create(catalog.Bootstrap, "other", "Other"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "other", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	for book, content := range map[string]string{"default": "# Current rollout\nSCOPED_CURRENT_CONTENT", "other": "# Peer rollout\nSCOPED_PEER_CONTENT"} {
		store, _ := app.catalog.Access(catalog.Bootstrap, book, "edit")
		if _, err := store.Save(t.Context(), "Ops.md", []byte(content), "", catalog.Bootstrap); err != nil {
			t.Fatal(err)
		}
	}
	w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+conversation.ID+"/turns", `{"submissionId":"denied-read-recovery","text":"Compare the current rollout with the peer rollout.","options":{"mode":"ask","corpus":{"version":1,"archives":"exclude","drafts":"exclude","history":false,"noteProjects":["other"]}}}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	app.chat.Wait()
	done := waitChatRun(t, app, conversation.ID)
	run := done.Runs[0]
	if run.State != "completed" || requests.Load() != 2 || len(run.Operations) != 2 {
		t.Fatalf("rejected read aborted permitted work: state=%s error=%q requests=%d operations=%d", run.State, run.Error, requests.Load(), len(run.Operations))
	}
	if run.Operations[0].Error == "" || len(run.Operations[0].Citations) != 0 || len(run.Operations[1].Citations) != 1 {
		t.Fatal("denied read gained evidence or subsequent permitted read was lost")
	}
}

func TestChatDocumentReadErrorCanBeCorrected(t *testing.T) {
	var requests atomic.Int32
	app, conversation := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		attempt := requests.Add(1)
		if attempt <= 2 {
			origin := "notebook"
			if attempt == 2 {
				origin = "notes"
			}
			args, _ := json.Marshal(documentRequest{documentRef: documentRef{Project: "other", Origin: origin, Path: "answer.md"}})
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
				"role": "assistant", "tool_calls": []any{map[string]any{"id": fmt.Sprint(attempt), "type": "function", "function": map[string]string{"name": "read_document", "arguments": string(args)}}},
			}, "finish_reason": "tool_calls"}}})
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Recovered the requested evidence."},"finish_reason":"stop"}]}`)
	})
	if err := app.catalog.Create(catalog.Bootstrap, "other", "Other"); err != nil {
		t.Fatal(err)
	}
	if err := app.catalog.Member(catalog.Bootstrap, "other", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	store, _ := app.catalog.Access(catalog.Bootstrap, "other", "edit")
	if _, err := store.Save(t.Context(), "answer.md", []byte("# Answer\nExact pinned content."), "", catalog.Bootstrap); err != nil {
		t.Fatal(err)
	}
	w := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+conversation.ID+"/turns", `{"submissionId":"document-recovery","text":"Read answer.md in the other notebook.","options":{"mode":"ask","corpus":{"version":1,"archives":"exclude","drafts":"exclude","history":false,"noteProjects":["other"]}}}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	app.chat.Wait()
	done := waitChatRun(t, app, conversation.ID)
	run := done.Runs[0]
	if run.State != "completed" || requests.Load() != 3 || len(run.Operations) != 2 {
		t.Fatalf("failed read prevented recovery: state=%s error=%q requests=%d operations=%d", run.State, run.Error, requests.Load(), len(run.Operations))
	}
	if len(run.Operations[0].Citations) != 0 || len(run.Operations[1].Citations) != 1 || run.Operations[1].Citations[0].Project != "other" {
		t.Fatal("failed read became evidence or successful recovery lost its citation")
	}
	if done.Turns[len(done.Turns)-1].Content != "Recovered the requested evidence." {
		t.Fatal("recovered answer was not delivered")
	}
}

func TestSourceReaderStyleNonceIsScopedToEachPage(t *testing.T) {
	app, _ := fixture(t)
	pattern := regexp.MustCompile(`<meta name="source-style-nonce" content="([^"]+)"`)
	previous := ""
	for range 2 {
		w := call(app, "GET", "/", "", testToken)
		match := pattern.FindStringSubmatch(w.Body.String())
		if w.Code != 200 || len(match) != 2 || len(match[1]) < 32 || match[1] == previous {
			t.Fatal("missing or reused per-page style nonce")
		}
		previous = match[1]
		policy := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(policy, "style-src 'self' 'nonce-"+previous+"'") || strings.Contains(policy, "unsafe-inline") || !strings.Contains(policy, "script-src 'self';") {
			t.Fatal("style nonce changed script or inline-style policy", policy)
		}
	}
}

func TestCrossNotebookCitationRevalidatesPinnedContentAfterUnrelatedSave(t *testing.T) {
	app, _ := fixture(t)
	if err := app.catalog.Create(catalog.Bootstrap, "other", "Other"); err != nil {
		t.Fatal(err)
	}
	store, _ := app.catalog.Access(catalog.Bootstrap, "other", "edit")
	doc, err := store.Save(t.Context(), "answer.md", []byte("# Answer\nExact pinned content."), "", catalog.Bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	run := chat.Run{Options: chat.Options{Corpus: &chat.Corpus{Version: 1, Archives: "exclude", Drafts: "exclude", NoteProjects: []string{"other"}}}}
	content, _ := chat.ContentScope(run)
	ctx := context.WithValue(t.Context(), identityKey{}, catalog.Bootstrap)
	ctx = chat.WithScope(ctx, &chat.RunScope{Project: "default", Run: run, Content: content})
	run.Citations = []chat.Citation{{Kind: "notebook", Project: "other", Path: "answer.md", Revision: doc.Revision, Hash: workspace.ContentHash(doc.Content)}}
	if _, err = store.Save(t.Context(), "unrelated.md", []byte("# Another saved note"), doc.Revision, catalog.Bootstrap); err != nil {
		t.Fatal(err)
	}
	if err = app.chatrun.ValidateSources(ctx, run, false); err != nil {
		t.Fatal("unchanged pinned citation rejected after unrelated save", err)
	}
	status, _ := store.Status(t.Context())
	if _, err = store.Save(t.Context(), "answer.md", []byte("# Changed"), status.Revision, catalog.Bootstrap); err != nil {
		t.Fatal(err)
	}
	if err = app.chatrun.ValidateSources(ctx, run, false); err == nil {
		t.Fatal("changed source accepted for current-only answer")
	}
}

func TestRepositoryCreationStartsBeforeNotebookExists(t *testing.T) {
	app, _ := fixture(t)
	t.Cleanup(func() { app.repositories = nil })
	app.repositoryConnections = &repositoryConnections{github: &reposource.GitHub{Config: reposource.Config{ClientID: "synthetic", Slug: "synthetic"}}}
	w := call(app, "POST", "/api/projects/repository/connect", `{"id":"new-source","name":"New source"}`, testToken)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "code_challenge") {
		t.Fatalf("start creation: %d %s", w.Code, w.Body.String())
	}
	for _, p := range app.catalog.List(catalog.Bootstrap) {
		if p.ID == "new-source" {
			t.Fatal("authorization prematurely created a notebook")
		}
	}
	if w := call(app, "POST", "/api/projects/repository/connect", `{"id":"default","name":"Collision"}`, testToken); w.Code != 409 {
		t.Fatalf("existing notebook reused: %d", w.Code)
	}
}

func TestRepositoryCreationRetriesOneNotebookAfterConnectionFailure(t *testing.T) {
	app, _ := fixture(t)
	configureSearchSource(t, app)
	g := app.repositoryConnections
	upstream, _ := app.repositories.Status("default")
	var choices atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/installations":
			json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "installations": []map[string]int{{"id": 7}}})
		case "/user/installations/7/repositories":
			// Fail connection discovery after the first valid completion creates the notebook.
			if choices.Add(1) == 3 {
				http.Error(w, "temporary GitHub failure", 503)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "repositories": []reposource.Repository{upstream.Repository}})
		case "/app/installations/7/access_tokens":
			json.NewEncoder(w).Encode(map[string]any{"token": "fixture-token", "expires_at": time.Now().Add(time.Hour)})
		case "/repositories/42":
			json.NewEncoder(w).Encode(upstream.Repository)
		case "/repos/example/source/commits/main":
			json.NewEncoder(w).Encode(map[string]string{"sha": upstream.Downloaded})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	g.github.API, g.github.HTTP = server.URL, server.Client()
	account, _ := app.catalog.Account(catalog.Bootstrap)
	key := digest("creation-cookie")
	if err := app.saveGrant(t.Context(), "repository/grant", g.github.Client, repositoryGrant{Subject: catalog.Bootstrap, Project: "retry-source", Name: "Retry source", Binding: key, Token: "fixture-user", Creating: true, Generation: account.Generation, Expires: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	finish := func(repository int) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", app.origin+"/api/projects/repository/finish?project=retry-source", strings.NewReader(fmt.Sprintf(`{"installation":7,"repository":%d,"shareWithNotebook":true,"externalProcessing":true}`, repository)))
		r.Header.Set("Authorization", "Bearer "+testToken)
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: "fmn-repository-connect", Value: "creation-cookie"})
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		return w
	}
	if w := finish(999); w.Code != 403 {
		t.Fatalf("forged repository admitted: %d %s", w.Code, w.Body)
	}
	if w := finish(42); w.Code == 200 {
		t.Fatal("connection failure was hidden")
	}
	if app.catalog.Kind("retry-source") != "repository" {
		t.Fatal("created notebook was lost after connection failure")
	}
	if w := finish(42); w.Code != 200 {
		t.Fatalf("connection retry failed: %d %s", w.Code, w.Body)
	}
	state, err := app.repositories.Status("retry-source")
	if err != nil || !state.Connected || !state.IndexConfig.ExternalProcessing {
		t.Fatal("retry lost source binding or processing consent", err)
	}
	if w := finish(42); w.Code != 200 {
		t.Fatalf("duplicate completion failed: %d %s", w.Code, w.Body)
	}
	again, _ := app.repositories.Status("retry-source")
	if again.ID != state.ID || again.Generation != state.Generation {
		t.Fatal("duplicate completion created another source binding")
	}
	if w := finish(999); w.Code != 409 {
		t.Fatalf("completion redirected to another repository: %d", w.Code)
	}
}

func TestUniversalDocumentsHonorSelectedNotebookOrigins(t *testing.T) {
	app, _ := fixture(t)
	for _, id := range []string{"allowed", "hidden"} {
		if err := app.catalog.Create(catalog.Bootstrap, id, id); err != nil {
			t.Fatal(err)
		}
		store, _ := app.catalog.Access(catalog.Bootstrap, id, "edit")
		if _, err := store.Save(t.Context(), "note.md", []byte("# "+id+"\nEvidence"), "", catalog.Bootstrap); err != nil {
			t.Fatal(err)
		}
	}
	var corpus chat.Corpus
	if err := json.Unmarshal([]byte(`{"version":1,"archives":"exclude","drafts":"exclude","history":false,"noteProjects":["allowed"]}`), &corpus); err != nil {
		t.Fatal(err)
	}
	if err := corpus.Validate(); err != nil {
		t.Fatalf("explicit note selection rejected: %v", err)
	}
	run := chat.Run{Options: chat.Options{Corpus: &corpus}}
	content, err := chat.ContentScope(run)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(t.Context(), identityKey{}, catalog.Bootstrap)
	ctx = chat.WithScope(ctx, &chat.RunScope{Project: "default", Run: run, Content: content})
	if _, err = app.readDocument(ctx, documentRequest{documentRef: documentRef{Project: "allowed", Origin: "notes", Path: "note.md"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = app.readDocument(ctx, documentRequest{documentRef: documentRef{Project: "hidden", Origin: "notes", Path: "note.md"}}); err == nil {
		t.Fatal("model expanded notebook scope")
	}
	if _, err = app.retrieveScoped(ctx, retrievalRequest{Projects: []string{"allowed"}, Corpora: []string{"notes"}, Query: "Evidence", Mode: "text"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"read_document", "notebook_map", "list_documents", "search_notebooks"} {
		args := `{"project":"allowed","origin":"notes"}`
		if name == "read_document" {
			args = `{"project":"allowed","origin":"notes","path":"note.md"}`
		}
		if name == "search_notebooks" {
			args = `{"projects":["allowed"],"corpora":["notes"],"query":"Evidence","mode":"text"}`
		}
		if _, err = app.dispatchAction(ctx, "default", name, json.RawMessage(args)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err = app.dispatchAction(ctx, "default", name, json.RawMessage(strings.ReplaceAll(args, "allowed", "hidden"))); err == nil {
			t.Fatalf("%s expanded chat scope", name)
		}
	}
	var retrieved chatrun.Retrieval
	if err = app.chatrun.AppendSources(ctx, run, knowledge.RetrievalQuery{Text: "Evidence", LocalOnly: true, Limit: 12, TokenBudget: 3000}, &retrieved); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(retrieved)
	if !strings.Contains(string(raw), `"project":"allowed"`) {
		t.Fatal("selected notes absent from automatic chat context", string(raw))
	}
}

func TestNotebookMapAndSharedRetrieval(t *testing.T) {
	app, _ := fixture(t)
	for path, body := range map[string]string{"guide/start.md": "# Startup\n\n## Ports\nServer port is 8080. [Details](details.md)", "guide/details.md": "# Details\nServer worker settings.", "other.md": "# Other\nServer unrelated."} {
		raw, _ := json.Marshal(map[string]string{"path": path, "content": body, "revision": ""})
		if w := call(app, "POST", "/api/note", string(raw), testToken); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := call(app, "GET", "/api/map?pathPrefix=guide/&limit=1", "", testToken)
	if w.Code != 200 {
		t.Fatalf("notebook map: %d %s", w.Code, w.Body.String())
	}
	var page struct {
		Revision, Next string
		Entries        []struct{ Path, Title string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.Next == "" || !strings.HasPrefix(page.Entries[0].Path, "guide/") {
		t.Fatal(w.Body.String())
	}
	doc := call(app, "GET", "/api/document?origin=notes&path=guide/start.md&revision="+page.Revision, "", testToken)
	if doc.Code != 200 || !strings.Contains(doc.Body.String(), "Ports") {
		t.Fatal(doc.Code, doc.Body.String())
	}
	for _, mode := range []string{"text", "meaning"} {
		request := httptest.NewRequest("POST", app.origin+"/api/retrieval", strings.NewReader(`{"projects":["default"],"corpora":["notes"],"query":"Server","pathPrefix":"guide/","mode":"`+mode+`"}`))
		request.Header.Set("Authorization", "Bearer "+testToken)
		request.Header.Set("Content-Type", "application/json")
		result := httptest.NewRecorder()
		app.ServeHTTP(result, request)
		if result.Code != 200 {
			t.Fatal(result.Code, result.Body.String())
		}
		var matches struct{ Results []json.RawMessage }
		if err := json.Unmarshal(result.Body.Bytes(), &matches); err != nil {
			t.Fatal(err)
		}
		if mode == "meaning" && len(matches.Results) != 0 {
			t.Fatal("Meaning returned keyword-only results", result.Body.String())
		}
		for _, match := range matches.Results {
			if strings.Contains(string(match), "other.md") {
				t.Fatal("path filter ignored", string(match))
			}
		}
	}
	client := actionClient(t, app, catalog.Bootstrap, "notes:read")
	actionCall(t, client, "notebook_map", map[string]any{"project": "default", "origin": "notes", "query": "Ports"}, false)
	actionCall(t, client, "read_document", map[string]any{"project": "default", "origin": "notes", "path": "guide/start.md"}, false)
	actionCall(t, client, "search_notebooks", map[string]any{"projects": []string{"default"}, "corpora": []string{"notes"}, "query": "Server", "mode": "text"}, false)
}

func TestNotebookSearchReturnsEachExcerptOnce(t *testing.T) {
	app, store := fixture(t)
	const evidence = "Unique routing marker for the notebook search."
	if _, err := store.Save(t.Context(), "answer.md", []byte("# Answer\n"+evidence), "", catalog.Bootstrap); err != nil {
		t.Fatal(err)
	}
	client := actionClient(t, app, catalog.Bootstrap, "notes:read")
	raw := actionCall(t, client, "search_notebooks", map[string]any{"projects": []string{"default"}, "corpora": []string{"notes"}, "query": "routing marker", "mode": "text"}, false)
	if count := strings.Count(string(raw), evidence); count != 1 {
		t.Fatalf("MCP sent the same original excerpt %d times", count)
	}
	var out scopedRetrieval
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 || out.Results[0].Ref.Path != "answer.md" || out.Results[0].Note == nil || out.Results[0].Note.Citation == "" || out.Notes["default"].Revision == "" || out.Notes["default"].Mode != "bm25" {
		t.Fatal("compaction lost note references or retrieval status")
	}
	configureSearchSource(t, app)
	raw = actionCall(t, client, "search_notebooks", map[string]any{"projects": []string{"default"}, "corpora": []string{"repository"}, "query": "SYNTHETIC_SOURCE_READY", "mode": "text"}, false)
	if count := strings.Count(string(raw), "SYNTHETIC_SOURCE_READY after the readiness check."); count != 1 {
		t.Fatalf("MCP sent the same original source excerpt %d times", count)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 || out.Results[0].Ref.Path != "health.go" || out.Results[0].Source == nil || out.Results[0].Source.Citation == "" || out.Sources.Coverage["default"].Indexed == "" || out.Results[0].Source.Excerpt != "" {
		t.Fatal("compaction lost source references or index coverage")
	}
	raw = actionCall(t, client, "search_repository_sources", map[string]any{"projects": []string{"default"}, "query": "SYNTHETIC_SOURCE_READY", "mode": "text"}, false)
	if count := strings.Count(string(raw), "SYNTHETIC_SOURCE_READY after the readiness check."); count != 1 {
		t.Fatalf("source MCP search duplicated original evidence %d times", count)
	}

}

func TestRepositoryNotebookIsReadOnlyAcrossHTTPAndMCP(t *testing.T) {
	app, _ := fixture(t)
	created := call(app, "POST", "/api/projects", `{"id":"source-book","name":"Source book","kind":"repository"}`, testToken)
	if created.Code != 200 {
		t.Fatalf("create repository notebook: %d %s", created.Code, created.Body.String())
	}
	listed := call(app, "GET", "/api/projects", "", testToken)
	var projects []struct {
		ID, Kind     string
		Capabilities struct{ Edit bool }
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &projects); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range projects {
		if p.ID == "source-book" {
			found = p.Kind == "repository" && !p.Capabilities.Edit
		}
	}
	if !found {
		t.Fatalf("missing read-only repository capability: %s", listed.Body.String())
	}
	for _, route := range []string{"note", "live", "live/patch", "live/checkpoint", "upload", "metadata", "container"} {
		w := call(app, "POST", "/p/source-book/api/"+route, `{"path":"README.md","content":"must not write","revision":""}`, testToken)
		if w.Code != 403 {
			t.Errorf("%s accepted repository mutation: %d %s", route, w.Code, w.Body.String())
		}
	}

	if w := call(app, "POST", "/p/default/api/note", `{"path":"ordinary.md","content":"still editable","revision":""}`, testToken); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call(app, "GET", "/p/source-book/api/status", "", testToken); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	client := actionClient(t, app, catalog.Bootstrap, "notes:read notes:write notes:manage")
	actionCall(t, client, "save_note", map[string]any{"project": "source-book", "path": "README.md", "content": "forbidden", "revision": ""}, true)
}
