package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// scoreMaintenanceFirst answers every relevance question, preferring z.md.
func scoreMaintenanceFirst(calls *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Questions map[string]struct {
				Instructions struct{ Candidate struct{ Path string } }
				Criteria     []any
			}
		}
		json.NewDecoder(r.Body).Decode(&req)
		answers := map[string]any{}
		for id, q := range req.Questions {
			score := 0
			if q.Instructions.Candidate.Path == "z.md" {
				score = 3
			}
			legend := map[string]any{}
			probabilities := map[string]float64{}
			for i, v := range q.Criteria {
				legend[fmt.Sprint(i)] = v
				probabilities[fmt.Sprint(i)] = 0
			}
			probabilities[fmt.Sprint(score)] = 1
			answers[id] = map[string]any{"type": "score", "score": score, "confidence": 1, "legend": legend, "probabilities": probabilities}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 100, "output_tokens": 20}})
	}
}

func searchRerankFixture(t *testing.T) (*Server, *atomic.Int32) {
	t.Helper()
	app := chatFixture(t)
	calls := &atomic.Int32{}
	configureSyntheticJudgments(t, app, scoreMaintenanceFirst(calls))
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		"a.md": "# Record\nBicycle workshop bicycle workshop bicycle workshop attendance.",
		"z.md": "# Maintenance\nBicycle workshop chains require CERULEAN lubricant.",
	} {
		if _, err := store.Save(context.Background(), path, []byte(content), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	return app, calls
}

func TestNotebookSearchUsesJudgmentOnlyForExplicitConsentedRequests(t *testing.T) {
	app, calls := searchRerankFixture(t)
	search := func(body string) knowledge.HybridResult {
		t.Helper()
		w := proxyCall(app, "alice", "POST", "/p/default/api/knowledge/hybrid-search", body)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var out knowledge.HybridResult
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := func(out knowledge.HybridResult) string {
		if len(out.Matches) == 0 {
			return ""
		}
		return out.Matches[0].Path
	}
	if out := search(`{"text":"bicycle workshop","rerank":true}`); out.Reranked || out.RerankAvailable || calls.Load() != 0 || first(out) != "a.md" {
		t.Fatalf("the judge ran without consent: %+v calls=%d", out, calls.Load())
	}
	if w := proxyCall(app, "alice", "POST", "/api/judgments/settings", `{"consent":true,"rerank":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if out := search(`{"text":"bicycle workshop"}`); out.Reranked || !out.RerankAvailable || calls.Load() != 0 {
		t.Fatalf("search as you type used the judge or hid availability: %+v calls=%d", out, calls.Load())
	}
	if out := search(`{"text":"bicycle workshop","mode":"text","rerank":true}`); out.Reranked || calls.Load() != 0 {
		t.Fatalf("text mode used the judge: %+v", out)
	}
	out := search(`{"text":"bicycle workshop","rerank":true}`)
	if !out.Reranked || calls.Load() == 0 || first(out) != "z.md" {
		t.Fatalf("explicit consented request did not rank: %+v calls=%d", out, calls.Load())
	}
	if w := proxyCall(app, "nobody", "POST", "/p/default/api/knowledge/hybrid-search", `{"text":"bicycle","rerank":true}`); w.Code != 403 {
		t.Fatal("ranking bypassed notebook permissions", w.Code)
	}
}

func TestMCPHybridSearchExplicitJudgmentRequest(t *testing.T) {
	app, calls := searchRerankFixture(t)
	if w := proxyCall(app, "alice", "POST", "/api/judgments/settings", `{"consent":true,"rerank":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	ctx := context.Background()
	ts := httptest.NewServer(app)
	defer ts.Close()
	app.origin = ts.URL
	app.oauth.Mu.Lock()
	app.oauth.Access[digest("rank-token")] = withOAuthFamily(accessGrant{Subject: "alice", Client: "rank-agent", Scope: "notes:read", Resource: ts.URL + "/mcp", Expires: time.Now().Add(time.Hour).Unix()})
	app.oauth.Mu.Unlock()
	client := mcp.NewClient(&mcp.Implementation{Name: "rank-agent", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: attributionTransport{token: "rank-token"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := func(rerank bool) knowledge.HybridResult {
		t.Helper()
		v, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "hybrid_search", Arguments: map[string]any{"project": "default", "query": map[string]any{"text": "bicycle workshop", "rerank": rerank}}})
		if err != nil || v.IsError {
			raw, _ := json.Marshal(v)
			t.Fatalf("%s %v", raw, err)
		}
		raw, _ := json.Marshal(v.StructuredContent)
		var out knowledge.HybridResult
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := call(false); out.Reranked || calls.Load() != 0 {
		t.Fatalf("MCP search without request used the judge: %+v", out)
	}
	if out := call(true); !out.Reranked || calls.Load() == 0 || out.Matches[0].Path != "z.md" {
		t.Fatalf("MCP explicit request did not rank: %+v calls=%d", out, calls.Load())
	}
}
