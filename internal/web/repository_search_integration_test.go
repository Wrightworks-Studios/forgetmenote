package web

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/reposource"
)

func configureSearchSource(t *testing.T, app *Server, extraFiles ...map[string]string) func(map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	os.MkdirAll(repo, 0700)
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo, "-c", "core.hooksPath=/dev/null"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatal(err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	git("config", "user.name", "Fixture")
	git("config", "user.email", "fixture@example.test")
	os.WriteFile(filepath.Join(repo, "health.go"), []byte("package source\n// Health returns SYNTHETIC_SOURCE_READY after the readiness check.\nfunc Health() bool { return true }\n"), 0600)
	for _, files := range extraFiles {
		for name, body := range files {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	git("add", ".")
	git("commit", "-m", "fixture")
	sha := git("rev-parse", "HEAD")
	var latest atomic.Value
	latest.Store(sha)
	upstream := reposource.Repository{ID: 42, FullName: "example/source", DefaultBranch: "main", Private: true}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/installations/7/access_tokens":
			json.NewEncoder(w).Encode(map[string]any{"token": "fixture-token", "expires_at": time.Now().Add(time.Hour)})
		case "/repositories/42":
			json.NewEncoder(w).Encode(upstream)
		case "/repos/example/source/commits/main":
			json.NewEncoder(w).Encode(map[string]string{"sha": latest.Load().(string)})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.InsecureSkipVerify = true // All dials below are pinned to this synthetic TLS listener.
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "api.github.com:443" {
			return nil, fmt.Errorf("unexpected fixture host")
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	os.WriteFile(filepath.Join(dir, "key"), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600)
	os.WriteFile(filepath.Join(dir, "client"), []byte("fixture-client"), 0600)
	os.WriteFile(filepath.Join(dir, "webhook"), []byte(strings.Repeat("fixture", 8)), 0600)
	cfg := reposource.Config{AppID: "123", Slug: "fixture", ClientID: "fixture", PrivateKeyFile: filepath.Join(dir, "key"), ClientSecretFile: filepath.Join(dir, "client"), WebhookSecretFile: filepath.Join(dir, "webhook")}
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	g, err := reposource.NewGitHub(cfg)
	http.DefaultTransport = previous
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "sources")
	binding := filepath.Join(root, "default", "fixture")
	os.MkdirAll(binding, 0700)
	if err = os.Rename(filepath.Join(repo, ".git"), filepath.Join(binding, "source.git")); err != nil {
		t.Fatal(err)
	}
	state := reposource.State{ID: "fixture", Project: "default", Actor: catalog.Bootstrap, Installation: 7, Repository: upstream, Connected: true, Generation: 1, Downloaded: sha, IndexConfig: reposource.IndexConfig{ExternalProcessing: true}}
	data, _ := json.Marshal(map[string]any{"version": 1, "active": "fixture", "sources": map[string]reposource.State{"fixture": state}})
	if _, err = app.db.WriteDocument(context.Background(), app.db.Pool, "repository-sources", "default", 0, data); err != nil {
		t.Fatal(err)
	}
	manager, err := reposource.Open(app.db, root, nil, g, func(p, a string) error { _, err := app.catalog.Access(a, p, "owner"); return err })
	if err != nil {
		t.Fatal(err)
	}
	app.repositories = manager
	app.repositoryConnections = &repositoryConnections{github: g}
	if _, err = manager.Refresh(context.Background(), "default"); err != nil {
		t.Fatal(err)
	}
	return func(files map[string]string) string {
		t.Helper()
		for name, content := range files {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
		run := func(args ...string) string {
			t.Helper()
			command := []string{"--git-dir", filepath.Join(binding, "source.git"), "--work-tree", repo, "-c", "core.hooksPath=/dev/null"}
			out, err := exec.Command("git", append(command, args...)...).CombinedOutput()
			if err != nil {
				t.Fatal(err, string(out))
			}
			return strings.TrimSpace(string(out))
		}
		run("add", ".")
		run("commit", "-m", "source update")
		revision := run("rev-parse", "HEAD")
		latest.Store(revision)
		if _, err := manager.Refresh(t.Context(), "default"); err != nil {
			t.Fatal(err)
		}
		return revision
	}
}

func TestChatSourceFileHonorsCurrentOwnerExclusions(t *testing.T) {
	app, store := fixture(t)
	configureSearchSource(t, app)
	state, _ := app.repositories.Status("default")
	corpus := &chat.Corpus{Version: 1, Archives: "exclude", Drafts: "exclude", SourceProjects: []string{"default"}}
	ctx := context.WithValue(t.Context(), identityKey{}, catalog.Bootstrap)
	resolved, err := app.resolveChatCorpus(ctx, store, corpus, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.SourceBindings["default"] != state.ID {
		t.Fatal("run did not freeze source binding")
	}
	scope := &chat.RunScope{Project: "default", Run: chat.Run{Options: chat.Options{Corpus: resolved}}}
	ctx = chat.WithScope(ctx, scope)
	raw, _ := json.Marshal(map[string]string{"project": "default", "source": state.ID, "revision": state.Downloaded, "path": "health.go"})
	if _, err = app.guardChatAction(ctx, "default", "repository_source_file", raw); err != nil {
		t.Fatal(err)
	}
	config := state.IndexConfig
	config.Exclude = []string{"health.go"}
	if _, err = app.repositories.ConfigureIndex("default", catalog.Bootstrap, state.Generation, config); err != nil {
		t.Fatal(err)
	}
	if _, err = app.guardChatAction(ctx, "default", "repository_source_file", raw); err == nil {
		t.Fatal("chat tool bypassed changed owner exclusion")
	}
}

func TestChatSourceCitationKeepsOnlyOriginalBytesWhenTruncated(t *testing.T) {
	app, _ := fixture(t)
	configureSearchSource(t, app)
	state, _ := app.repositories.Status("default")
	body := strings.Repeat("original é\n", 2000)
	file := reposource.File{Project: "default", Source: state.ID, Revision: state.Downloaded, Path: "health.go", Content: body, URL: "https://github.com/example/source/blob/" + state.Downloaded + "/health.go"}
	raw, _ := json.Marshal(file)
	ctx := context.WithValue(t.Context(), identityKey{}, catalog.Bootstrap)
	got, _ := app.chatToolEvidence(ctx, chat.RunAuth{User: catalog.Bootstrap}, "fixture", "repository_source_file", raw)
	if len(got) != 1 {
		t.Fatal("citation missing")
	}
	c := got[0]
	if len(c.Text) > 12000 || !utf8.ValidString(c.Text) || c.Text != body[c.StartByte:c.EndByte] {
		t.Fatal("bounded source citation invented bytes or coordinates")
	}
}
func TestRepositorySearchHTTPAndMCPReturnSameEvidence(t *testing.T) {
	app, store := fixture(t)
	configureSearchSource(t, app)
	before, _ := store.Status(t.Context())
	r := httptest.NewRequest("POST", app.origin+"/api/retrieval", strings.NewReader(`{"projects":["default"],"corpora":["repository"],"query":"Health","mode":"text"}`))
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var httpOut scopedRetrieval
	if err := json.Unmarshal(w.Body.Bytes(), &httpOut); err != nil {
		t.Fatal(err)
	}
	client := actionClient(t, app, catalog.Bootstrap, "notes:read")
	raw := actionCall(t, client, "search_repository_sources", map[string]any{"projects": []string{"default"}, "query": "Health", "mode": "text"}, false)
	var mcpOut reposource.SearchResult
	json.Unmarshal(raw, &mcpOut)
	if len(httpOut.Sources.Results) == 0 || httpOut.Sources.Results[0].Excerpt == "" {
		t.Fatal("HTTP lost its source display preview", w.Body.String())
	}
	for i := range httpOut.Sources.Results {
		httpOut.Sources.Results[i].Excerpt = ""
	}
	if len(httpOut.Sources.Results) == 0 || !reflect.DeepEqual(httpOut.Sources.Results, mcpOut.Results) {
		t.Fatal("HTTP and MCP differ", string(raw))
	}
	actionCall(t, client, "search_repository_sources", map[string]any{"projects": []string{"missing"}, "query": "Health"}, true)
	actionCall(t, client, "configure_repository_index", map[string]any{"project": "default", "generation": 1, "config": map[string]any{}}, true)
	after, _ := store.Status(t.Context())
	if before.Revision != after.Revision {
		t.Fatal("source search changed notebook Git")
	}
}
func TestChatRepositoryEvidenceReachesProviderWithSavedScope(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, string(b))
		mu.Unlock()
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The source defines Health."},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":5}}`)
	})
	configureSearchSource(t, app)
	body := `{"submissionId":"repository-scope","text":"Health","options":{"mode":"ask","corpus":{"version":1,"archives":"exclude","drafts":"exclude","history":false,"sourceProjects":["default"]}}}`
	response := proxyCall(app, "alice", "POST", "/p/default/api/chat/"+c.ID+"/turns", body)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	done := waitChatRun(t, app, c.ID)
	if done.Runs[0].State != "completed" {
		t.Fatal(done.Runs[0].Error)
	}
	mu.Lock()
	captured := strings.Join(requests, "\n")
	mu.Unlock()
	if !strings.Contains(captured, "SYNTHETIC_SOURCE_READY") {
		t.Fatal("selected source missing from provider context")
	}
	found := false
	for _, citation := range done.Runs[0].Citations {
		if citation.Kind == "repository" {
			found = true
			if citation.Project != "default" || citation.StartLine < 1 || !strings.Contains(citation.URL, "#L") {
				t.Fatal("source citation lost coordinates")
			}
		}
	}
	if !found {
		t.Fatal("no source citation")
	}
}

func TestRepositoryJevRequiresBothConsentGatesAndTextStaysLocal(t *testing.T) {
	app := chatFixture(t)
	configureSearchSource(t, app)
	var calls atomic.Int32
	configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Questions map[string]decision.Question `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Questions) > 8 {
			t.Error("oversized Jev shortlist batch")
		}
		answers := map[string]any{}
		for id, q := range req.Questions {
			legend := map[string]any{}
			probabilities := map[string]float64{}
			levels := q.Criteria.([]any)
			for i, v := range levels {
				key := fmt.Sprint(i)
				legend[key] = v
				probabilities[key] = 0
			}
			probabilities["3"] = 1
			answers[id] = map[string]any{"type": "score", "score": 3, "confidence": 1, "legend": legend, "probabilities": probabilities}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 100, "output_tokens": 20}})
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	request := func(mode string) scopedRetrieval {
		t.Helper()
		w := proxyCall(app, "alice", "POST", "/api/retrieval", `{"projects":["default"],"corpora":["repository"],"query":"Health","mode":"`+mode+`"}`)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var out scopedRetrieval
		json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	if out := request("auto"); out.Sources.Reranked || calls.Load() != 0 {
		t.Fatal("Jev ran without consent")
	}
	if w := proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":true,"rerank":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	request("auto")
	if calls.Load() != 0 {
		t.Fatal("personal consent bypassed source owner")
	}
	state, _ := app.repositories.Status("default")
	cfg := state.IndexConfig
	cfg.Jev = true
	if _, err := app.repositories.ConfigureIndex("default", catalog.Bootstrap, state.Generation, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := app.repositories.Refresh(t.Context(), "default"); err != nil {
		t.Fatal(err)
	}
	request("text")
	if calls.Load() != 0 {
		t.Fatal("text search called Jev")
	}
	out := request("auto")
	if !out.Sources.Reranked || calls.Load() != 1 {
		t.Fatal("consented source ranking did not run", calls.Load(), out.Sources)
	}
}

func TestRepositoryJevRanksCandidatesBeyondFinalCutoff(t *testing.T) {
	app := chatFixture(t)
	files := map[string]string{"z.txt": "Bicycle workshop chains require CERULEAN lubricant."}
	for i := range 20 {
		files[fmt.Sprintf("a%02d.txt", i)] = "Bicycle workshop bicycle workshop attendance."
	}
	for i := range 24 {
		files[fmt.Sprintf("zz%02d.txt", i)] = "Bicycle workshop. " + strings.Repeat("General background information. ", 30)
	}
	configureSearchSource(t, app, files)
	var seen atomic.Int32
	configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Questions map[string]struct {
				Instructions struct{ Candidate struct{ Path string } }
				Criteria     []any
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		seen.Add(int32(len(req.Questions)))
		answers := map[string]any{}
		for id, q := range req.Questions {
			score := 0
			if q.Instructions.Candidate.Path == "z.txt" {
				score = 3
			}
			legend, probabilities := map[string]any{}, map[string]float64{}
			for level, text := range q.Criteria {
				key := fmt.Sprint(level)
				legend[key], probabilities[key] = text, 0
			}
			probabilities[fmt.Sprint(score)] = 1
			answers[id] = map[string]any{"type": "score", "score": score, "confidence": 1, "legend": legend, "probabilities": probabilities}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 100, "output_tokens": 20}})
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "read"); err != nil {
		t.Fatal(err)
	}
	if w := proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":true,"rerank":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	state, _ := app.repositories.Status("default")
	cfg := state.IndexConfig
	cfg.Jev = true
	if _, err := app.repositories.ConfigureIndex("default", catalog.Bootstrap, state.Generation, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := app.repositories.Refresh(t.Context(), "default"); err != nil {
		t.Fatal(err)
	}
	w := proxyCall(app, "alice", "POST", "/api/retrieval", `{"projects":["default"],"corpora":["repository"],"query":"bicycle workshop","mode":"auto","limit":1}`)
	var out scopedRetrieval
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if !out.Sources.Reranked || len(out.Results) != 1 || out.Results[0].Ref.Path != "z.txt" || seen.Load() != 32 {
		t.Fatalf("ranking lost candidates before Jev: seen=%d result=%+v", seen.Load(), out)
	}
}
