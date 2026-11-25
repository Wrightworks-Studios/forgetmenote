package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/pg/pgtest"
	"forgetmenote/internal/workspace"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func rankingService(t *testing.T, handler http.HandlerFunc, limits ...decision.Limits) *decision.Service {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	previous := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte("synthetic-key"), 0600); err != nil {
		t.Fatal(err)
	}
	ledger, err := decision.OpenLedger(pgtest.Org(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ledger.Close)
	config := decision.Config{Enabled: true, Model: "jev-1.13.0", Endpoint: server.URL + "/v1/systemone", SecretFile: key}
	if len(limits) > 0 {
		config.Limits = limits[0]
	}
	service, err := decision.New(config, ledger, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	return service
}

func TestJevShortlistSharesQueryWithIndependentCandidateQuestions(t *testing.T) {
	var calls, checks atomic.Int32
	passages := make([]Passage, 32)
	for i := range passages {
		passages[i] = Passage{Path: fmt.Sprintf("p%d.md", i), Heading: "Workshop", Text: strings.Repeat("Workshop evidence. ", 45)}
	}
	service := rankingService(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			State     map[string]any
			Questions map[string]struct {
				Type         string
				Instructions struct {
					Question  string
					Candidate struct{ Path, Heading, Text string }
				}
				Criteria []string
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error("candidate-specific question missing:", err)
			http.Error(w, "fixture", 500)
			return
		}
		if len(request.State) != 1 || request.State["query"] != "workshop" || len(request.Questions) != len(passages) {
			t.Error("shortlist was split or unrelated candidates entered shared state")
		}
		answers := map[string]any{}
		for id, question := range request.Questions {
			var i int
			fmt.Sscanf(id, "p%d", &i)
			candidate := question.Instructions.Candidate
			if candidate.Path != passages[i].Path || candidate.Heading != passages[i].Heading || candidate.Text != passages[i].Text || !strings.Contains(question.Instructions.Question, "`candidate`") {
				t.Error("candidate changed or missing from its question")
			}
			legend, probabilities := map[string]any{}, map[string]float64{}
			for level, text := range question.Criteria {
				key := fmt.Sprint(level)
				legend[key], probabilities[key] = text, 0
			}
			probabilities[fmt.Sprint(i%4)] = 1
			answers[id] = map[string]any{"type": "score", "score": i % 4, "confidence": 1, "legend": legend, "probabilities": probabilities}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 300, "output_tokens": 70}})
	})
	scores, err := ScorePassages(t.Context(), "workshop", passages, JevRanking{Service: service, Scope: decision.Scope{Actor: "alice", Notebook: "book", Run: "run", Operation: "query"}, Check: func(context.Context) error { checks.Add(1); return nil }})
	if err != nil || len(scores) != len(passages) || calls.Load() != 1 || checks.Load() != 2 {
		t.Fatalf("one API batch should use its pre/post authorization: scores=%v calls=%d checks=%d err=%v", scores, calls.Load(), checks.Load(), err)
	}
	for i, score := range scores {
		if score != float64(i%4) {
			t.Fatalf("candidate %d got score %v", i, score)
		}
	}
}
func scoreRelevance(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Questions map[string]struct {
			Instructions struct{ Candidate struct{ Path string } }
			Criteria     []any
		}
	}
	if json.NewDecoder(r.Body).Decode(&request) != nil {
		http.Error(w, "fixture", 500)
		return
	}
	answers := map[string]any{}
	for id, q := range request.Questions {
		score := 0
		if q.Instructions.Candidate.Path == "z.md" {
			score = 3
		}
		levels := q.Criteria
		legend := map[string]any{}
		probabilities := map[string]float64{}
		for level, text := range levels {
			key := fmt.Sprint(level)
			legend[key] = text
			probabilities[key] = 0
		}
		probabilities[fmt.Sprint(score)] = 1
		answers[id] = map[string]any{"type": "score", "score": score, "confidence": 1, "legend": legend, "probabilities": probabilities}
	}
	json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 300, "output_tokens": 70}})
}
func TestJevRanksAuthorizedShortlistBeforeFinalPacking(t *testing.T) {
	engine := retrievalNotes(t, map[string]string{"a.md": "# Record\nBicycle workshop bicycle workshop bicycle workshop attendance.", "z.md": "# Maintenance\nBicycle workshop chains require CERULEAN lubricant."})
	query := RetrievalQuery{Text: "bicycle workshop", Limit: 1, TokenBudget: 2000}
	before, err := engine.Retrieve(context.Background(), query)
	if err != nil || len(before.Passages) != 1 || before.Passages[0].Path != "a.md" {
		t.Fatalf("bad ranking fixture: %+v %v", before, err)
	}
	service := rankingService(t, scoreRelevance)
	query.Jev = &JevRanking{Service: service, Scope: decision.Scope{Actor: "alice", Notebook: "notebook", Feature: "rerank", Run: "run", Operation: "query"}, Check: func(context.Context) error { return nil }}
	after, err := engine.Retrieve(context.Background(), query)
	if err != nil || len(after.Passages) != 1 || after.Passages[0].Path != "z.md" {
		t.Fatalf("rerank happened after candidate packing: %+v %v", after, err)
	}
}

func TestJevRanksBoundedPoolInParallelBeforePacking(t *testing.T) {
	notes := map[string]string{"z.md": "Bicycle workshop chains need CERULEAN lubricant."}
	for i := range 20 {
		notes[fmt.Sprintf("a%02d.md", i)] = "Bicycle workshop bicycle workshop attendance."
	}
	for i := range 24 {
		notes[fmt.Sprintf("zz%02d.md", i)] = "Bicycle workshop. " + strings.Repeat("General background information. ", 30)
	}
	engine := retrievalNotes(t, notes)
	var active, peak, seen atomic.Int32
	ready := make(chan struct{})
	var once sync.Once
	service := rankingService(t, func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		if n >= 2 {
			once.Do(func() { close(ready) })
		}
		select {
		case <-ready:
		case <-time.After(time.Second):
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		var questions map[string]decision.Question
		json.Unmarshal(body["questions"], &questions)
		seen.Add(int32(len(questions)))
		raw, _ := json.Marshal(body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		scoreRelevance(w, r)
	}, decision.Limits{Questions: 16, RequestBytes: 16384})
	query := RetrievalQuery{Text: "bicycle workshop", Limit: 1, Jev: &JevRanking{Service: service, Scope: decision.Scope{Actor: "alice", Notebook: "book", Run: "run", Operation: "query"}, Check: func(context.Context) error { return nil }}}
	result, err := engine.Retrieve(t.Context(), query)
	if err != nil || !result.Reranked || len(result.Passages) != 1 || result.Passages[0].Path != "z.md" || seen.Load() != 32 || peak.Load() != 2 {
		t.Fatalf("bounded-pool ranking: result=%+v err=%v candidates=%d concurrent=%d", result, err, seen.Load(), peak.Load())
	}
}

func TestJevRankingFallbackAndAuthorizationBoundaries(t *testing.T) {
	for _, mode := range []string{"provider failure", "local only", "revoked", "changed"} {
		t.Run(mode, func(t *testing.T) {
			engine := retrievalNotes(t, map[string]string{"a.md": "# Record\nBicycle workshop bicycle workshop attendance.", "z.md": "# Maintenance\nBicycle workshop chains need lubricant."})
			var calls atomic.Int32
			var revoked atomic.Bool
			service := rankingService(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if mode == "provider failure" {
					http.Error(w, "unavailable", 503)
					return
				}
				if mode == "revoked" {
					revoked.Store(true)
				}
				if mode == "changed" {
					doc, err := engine.Store.Read(context.Background(), "a.md", "")
					if err != nil {
						t.Error(err)
					} else if _, err = engine.Store.Save(context.Background(), doc.Path, []byte("Bicycle workshop changed after candidate selection."), doc.Revision, "operator"); err != nil {
						t.Error(err)
					}
				}
				scoreRelevance(w, r)
			})
			query := RetrievalQuery{Text: "bicycle workshop", Limit: 1, TokenBudget: 2000, LocalOnly: mode == "local only", Jev: &JevRanking{Service: service, Scope: decision.Scope{Actor: "alice", Notebook: "book", Run: "run", Operation: "query"}, Check: func(context.Context) error {
				if revoked.Load() {
					return workspace.ErrForbidden
				}
				return nil
			}}}
			result, err := engine.Retrieve(context.Background(), query)
			if mode == "revoked" {
				if !errors.Is(err, workspace.ErrForbidden) {
					t.Fatalf("revocation became provider fallback: %v", err)
				}
				return
			}
			if mode == "changed" {
				if !errors.Is(err, workspace.ErrConflict) {
					t.Fatalf("stale evidence published: %+v %v", result, err)
				}
				return
			}
			if err != nil || result.Reranked || len(result.Passages) != 1 || result.Passages[0].Path != "a.md" {
				t.Fatalf("normal rank fallback failed: %+v %v", result, err)
			}
			if mode == "local only" && calls.Load() != 0 {
				t.Fatal("local-only sent remote evidence")
			}
		})
	}
}

func TestJevPoolFailureIsAtomicAndRevocationStopsQueuedBatches(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(fmt.Sprint("revoked=", revoked), func(t *testing.T) {
			var calls atomic.Int32
			var denied atomic.Bool
			service := rankingService(t, func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				if !revoked && call == 1 {
					scoreRelevance(w, r)
					return
				}
				if revoked {
					denied.Store(true)
				}
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			})
			passages := make([]Passage, 100)
			for i := range passages {
				passages[i] = Passage{Path: fmt.Sprintf("p%d.md", i), Text: "Workshop evidence."}
			}
			scores, err := ScorePassages(t.Context(), "workshop", passages, JevRanking{Service: service, Scope: decision.Scope{Actor: "alice", Notebook: "book", Run: "run", Operation: "query"}, Check: func(context.Context) error {
				if denied.Load() {
					return workspace.ErrForbidden
				}
				return nil
			}})
			limit := int32(3)
			if revoked {
				limit = 2
			}
			if scores != nil || calls.Load() > limit || calls.Load() == 0 {
				t.Fatalf("partial ranking or queued requests survived failure: scores=%v calls=%d", scores, calls.Load())
			}
			if revoked && !errors.Is(err, workspace.ErrForbidden) || !revoked && err != nil {
				t.Fatalf("wrong failure boundary: %v", err)
			}
		})
	}
}

func TestJevPoolReusesIdenticalInputsWithoutLosingOccurrences(t *testing.T) {
	var calls, questions atomic.Int32
	service := rankingService(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var request decision.Request
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Error(err)
			return
		}
		questions.Add(int32(len(request.Questions)))
		r.Body = io.NopCloser(bytes.NewReader(raw))
		scoreRelevance(w, r)
	})
	passages := make([]Passage, 40)
	for i := range passages {
		passages[i] = Passage{Path: "z.md", Text: "Identical evidence at different original offsets."}
	}
	// Path, heading and content each affect relevance; only identical provider
	// inputs may share a judgment, even when other citation metadata differs.
	passages[1].Path = "a.md"
	passages[2].Heading = "Different context"
	passages[3].Text = "Distinct evidence."
	scores, err := ScorePassages(t.Context(), "evidence", passages, JevRanking{Service: service, Scope: decision.Scope{Actor: "alice", Notebook: "book", Run: "run", Operation: "query"}, Check: func(context.Context) error { return nil }})
	if err != nil || len(scores) != len(passages) || calls.Load() != 1 || questions.Load() != 4 {
		t.Fatalf("duplicate inputs lost evidence or repeated paid work: scores=%v calls=%d questions=%d err=%v", scores, calls.Load(), questions.Load(), err)
	}
	for i, score := range scores {
		want := 3.0
		if i == 1 {
			want = 0
		}
		if score != want {
			t.Fatalf("lost duplicate occurrence: %v", scores)
		}
	}
}
