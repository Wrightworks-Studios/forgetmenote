package web

import (
	"context"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/chatrun"
	"forgetmenote/internal/sources"
	"forgetmenote/internal/workspace"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise real chat HTTP admission, provider HTTP tool continuation and durable
// review state. The source-capture boundary supplies synthetic public-page bytes;
// production keeps sources.Prepare's DNS, redirect and address protections.
func TestChatResearchReviewRetainsFreshCoverage(t *testing.T) {
	var requests, fetches atomic.Int32
	prompts := make(chan string, 32)
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		json.NewDecoder(r.Body).Decode(&body)
		prompts <- string(body)
		n := requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"public-original","type":"function","function":{"name":"research_fetch","arguments":"{\"url\":\"https://example.org/source.txt\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`)
		} else {
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"FRESH_REVIEW_%d based on supplied originals and dated external capture when present."},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`, n)
		}
	})
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"notes/a.md", "notes/b.md"} {
		if _, err = store.Save(t.Context(), path, []byte("Synthetic committed original for bounded review."), "", "operator"); err != nil {
			t.Fatal(err)
		}
	}
	before, err := store.Snapshot(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "synthetic-search-secret")
	if err = os.WriteFile(secret, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &chat.ResearchConfig{Enabled: true, Format: "brave", BaseURL: "https://search.invalid", SecretFile: secret, MaxSearches: 1, MaxFetches: 1}
	research, err := chat.NewResearch(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	app.chat.Mu.Lock()
	installed := *app.chat.Config
	installed.Connections = append([]chat.Connection(nil), installed.Connections...)
	installed.Research = cfg
	installed.Connections[0].Limits.MaxRounds = 2
	app.chat.Config, app.chat.Research = &installed, research
	app.chat.PrepareSource = func(ctx context.Context, filename, originalURL string, data []byte) (workspace.SourceCapture, error) {
		fetches.Add(1)
		if originalURL != "https://example.org/source.txt" || data != nil {
			t.Error("unexpected capture request")
		}
		capture, err := sources.Prepare(ctx, "source.txt", originalURL, []byte("SYNTHETICPUBLICORIGINAL dated external evidence."))
		capture.Record.Method = "url"
		return capture, err
	}
	app.chat.Mu.Unlock()
	path := "/p/default/api/chat/" + c.ID
	w := proxyCall(app, "alice", "POST", path+"/turns", `{"submissionId":"review-research","text":"Review original notes using explicitly enabled research of https://example.org/source.txt when helpful","options":{"mode":"ask","review":true,"research":true}}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	first := waitChatRun(t, app, c.ID)
	app.chat.Wait()
	run := first.Runs[0]
	if run.Review == nil || len(run.Review.Notes) < 2 || run.Review.Notes[0].Cursor != 1 || run.Review.Notes[0].State != "examined" {
		t.Fatalf("successful external fetch erased freshly examined original coverage: %+v", run.Review)
	}
	if run.State != "interrupted" || requests.Load() != 2 || fetches.Load() != 1 || run.ResearchFetches != 1 || run.ResearchSearches != 0 {
		t.Fatalf("combined review escaped allowance: %+v requests=%d fetches=%d", run, requests.Load(), fetches.Load())
	}
	proof := run.Review.Notes[0].Provenance
	if proof.ExternalAllowance == "" || len(proof.ExternalSources) != 1 || len(run.Provenance.ExternalSources) != 1 {
		t.Fatalf("fresh and inherited capture metadata missing: %+v", run.Review)
	}
	for _, source := range run.Review.Notes[0].Sources {
		if source.Kind != "committed" || source.Path == "" {
			t.Fatal("external capture mixed into committed dependencies")
		}
	}
	if len(run.Operations) != 1 || run.Operations[0].State != "completed" {
		t.Fatalf("capture receipt missing: %+v", run.Operations)
	}
	var capture struct {
		Archived   bool                   `json:"archived"`
		Provenance workspace.SourceRecord `json:"provenance"`
	}
	if err = json.Unmarshal(run.Operations[0].Result, &capture); err != nil {
		t.Fatal(err)
	}
	if _, err = time.Parse(time.RFC3339Nano, capture.Provenance.CapturedAt); err != nil || capture.Archived || capture.Provenance.URL != "https://example.org/source.txt" || capture.Provenance.SHA256 == "" {
		t.Fatalf("dated external provenance missing: %+v", capture)
	}
	<-prompts
	continuation := <-prompts
	if !strings.Contains(continuation, "SYNTHETICPUBLICORIGINAL") || !strings.Contains(continuation, capture.Provenance.CapturedAt) {
		t.Fatalf("provider did not receive captured original with date: %s", continuation)
	}
	after, err := store.Snapshot(t.Context(), "")
	if err != nil || before.Revision != after.Revision {
		t.Fatal("research implicitly archived or changed notebook")
	}
	w = proxyCall(app, "alice", "POST", path+"/runs/"+run.ID+"/resume", `{}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	resumed := waitChatRun(t, app, c.ID)
	app.chat.Wait()
	nextPrompt := <-prompts
	if strings.Contains(nextPrompt, "FRESH_REVIEW_2") || strings.Contains(nextPrompt, "SYNTHETICPUBLICORIGINAL") {
		t.Fatalf("new allowance reused unverifiable external-derived summary: %s", nextPrompt)
	}
	if resumed.Runs[0].State != "completed" || !resumed.Runs[0].Review.Complete {
		t.Fatalf("renewed original review could not finish: %+v", resumed.Runs[0])
	}
	if fetches.Load() != 1 || resumed.Runs[0].ResearchFetches != 1 || requests.Load() > 4 {
		t.Fatal("resume implicitly refetched or exceeded allowance")
	}
}

func TestChatResearchSearchReviewCompletesWithoutReusingSnippets(t *testing.T) {
	var requests, searches atomic.Int32
	prompts := make(chan string, 32)
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		json.NewDecoder(r.Body).Decode(&body)
		prompts <- string(body)
		n := requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"search-candidate","type":"function","function":{"name":"research_search","arguments":"{\"query\":\"synthetic topic\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`)
		} else {
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"SEARCH_REVIEW_%d: Original notes examined; search candidates remain unverified."},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`, n)
		}
	})
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"notes/a.md", "notes/b.md"} {
		if _, err = store.Save(t.Context(), path, []byte("Synthetic original note."), "", "operator"); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(t.TempDir(), "synthetic-search-secret")
	if err = os.WriteFile(secret, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &chat.ResearchConfig{Enabled: true, Format: "brave", BaseURL: "https://search.invalid", SecretFile: secret, MaxSearches: 1, MaxFetches: 1}
	research, err := chat.NewResearch(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	app.chat.Mu.Lock()
	installed := *app.chat.Config
	installed.Research = cfg
	app.chat.Config, app.chat.Research = &installed, research
	app.chat.SearchResearch = func(ctx context.Context, r *chat.Research, query string, count int) ([]chat.ResearchResult, chat.Usage, error) {
		searches.Add(1)
		return []chat.ResearchResult{{Title: "Synthetic candidate", URL: "https://example.org/source.txt", Description: "MUTABLESEARCHSNIPPET"}}, chat.Usage{}, nil
	}
	app.chat.Mu.Unlock()
	path := "/p/default/api/chat/" + c.ID
	body := `{"submissionId":"search-review","text":"Review original notes with permitted research","options":{"mode":"ask","review":true,"research":true}}`
	w := proxyCall(app, "alice", "POST", path+"/turns", body)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	first := waitChatRun(t, app, c.ID)
	app.chat.Wait()
	run := first.Runs[0]
	if run.State != "completed" || run.Review == nil || !run.Review.Complete || requests.Load() != 3 || searches.Load() != 1 {
		t.Fatalf("search-only research prevented fresh full coverage: %+v", run)
	}
	for _, note := range run.Review.Notes {
		if note.State != "examined" || note.Provenance.ExternalAllowance == "" || len(note.Provenance.ExternalSources) != 0 {
			t.Fatalf("search-dependent analysis lost explicit unverified external marker: %+v", note)
		}
	}
	<-prompts
	continuation := <-prompts
	if !strings.Contains(continuation, "MUTABLESEARCHSNIPPET") || !strings.Contains(continuation, "capturedAt") || !strings.Contains(continuation, "unverified") {
		t.Fatalf("missing dated unverified candidate result: %s", continuation)
	}
	<-prompts
	w = proxyCall(app, "alice", "POST", path+"/turns", strings.Replace(body, "search-review", "search-review-again", 1))
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	next := waitChatRun(t, app, c.ID)
	app.chat.Wait()
	if next.Runs[1].State != "completed" || requests.Load() != 5 || searches.Load() != 1 {
		t.Fatal("new review reused search-derived coverage or implicitly searched again")
	}
	nextPrompt := <-prompts
	if strings.Contains(nextPrompt, "SEARCH_REVIEW_2") || strings.Contains(nextPrompt, "MUTABLESEARCHSNIPPET") {
		t.Fatalf("mutable search-derived summary reused as verified: %s", nextPrompt)
	}
	snapshot, err := store.Snapshot(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	_, guidance, err := app.chatrun.ChatInstructions(t.Context(), store, snapshot.Revision)
	if err != nil {
		t.Fatal(err)
	}
	valid := chatrun.EvidenceValidator(snapshot, guidance, first.Turns, first.Options.Corpus.ID())
	for _, turn := range first.Turns {
		if turn.Role == "assistant" && strings.HasPrefix(turn.Content, "SEARCH_REVIEW_") && valid(turn.Provenance, turn.Citations) {
			t.Fatal("ordinary history certified mutable search evidence")
		}
	}
}

func TestChatReviewExternalProofRemainsBoundedAndSeparated(t *testing.T) {
	base := chat.ExternalCapture{URL: "https://example.org/source.txt", CapturedAt: "2026-09-14T12:00:00Z", Hash: strings.Repeat("a", 64)}
	for _, change := range []string{"old allowance", "missing date", "private URL", "unknown capture", "overflow", "unknown tool"} {
		t.Run(change, func(t *testing.T) {
			f := newReviewDependencyFixture(t)
			run := f.prepare(t)
			p := chat.Provenance{ExternalAllowance: chatrun.ReviewExternalAllowance(&run), ExternalSources: []chat.ExternalCapture{base}}
			citation := chat.Citation{Kind: "external", URL: base.URL, Hash: base.Hash, CapturedAt: base.CapturedAt}
			switch change {
			case "old allowance":
				p.ExternalAllowance += "-old"
			case "missing date":
				p.ExternalSources[0].CapturedAt = ""
			case "private URL":
				p.ExternalSources[0].URL = "http://127.0.0.1/private"
			case "unknown capture":
				citation.Hash = strings.Repeat("b", 64)
			case "overflow":
				for i := 0; i < 33; i++ {
					x := base
					x.URL = fmt.Sprintf("https://example.org/source%d", i)
					p.ExternalSources = append(p.ExternalSources, x)
				}
			}
			f.update(t, func(r *chat.Run) {
				if change == "overflow" {
					r.Provenance.ExternalAllowance = p.ExternalAllowance
					r.Provenance.ExternalSources = p.ExternalSources
				} else {
					chatrun.InheritExternalEvidence(&r.Provenance, p)
				}
				if change == "unknown tool" {
					r.Provenance.Complete = false
				}
				r.Citations = append(r.Citations, citation)
				chatrun.FinishReviewBatch(r, "UNVERIFIABLE_EXTERNAL_ANALYSIS")
			})
			result := f.read(t)
			if result.Review.Notes[0].State != "unverified" || result.Review.Notes[0].Cursor != 0 {
				t.Fatalf("invalid external proof advanced coverage: %+v", result.Review)
			}
		})
	}
}
