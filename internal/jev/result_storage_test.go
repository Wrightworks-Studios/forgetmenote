package jev

import (
	"context"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/decision"
	"net/http"
	"strings"
	"testing"
)

func TestJevReviewResultVolumeDoesNotStopDurableProgress(t *testing.T) {
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Questions map[string]decision.Question `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		probabilities := map[string]float64{}
		for key := range request.Questions["assessment"].Criteria.(map[string]any) {
			probabilities[key] = 0
		}
		probabilities["compatible"] = 1
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": map[string]any{"assessment": map[string]any{"type": "choice", "choice": "compatible", "confidence": 1, "probabilities": probabilities}}, "usage": map[string]int{"input_tokens": 60, "output_tokens": 20}})
	})
	store, _ := app.catalog.Access("alice", "default", "edit")
	for i := 0; i < 4; i++ {
		if _, err := store.Save(context.Background(), fmt.Sprintf("large-%d.md", i), []byte(strings.Repeat("Workshop scheduled every weekday. ", 29)), "", "alice"); err != nil {
			t.Fatal(err)
		}
	}
	// Exercise the same atomic record-size boundary with small synthetic data.
	// Metadata and each result fit individually; their combined volume exceeds it.
	app.jevState.mu.Lock()
	app.jevState.records.Bytes = 12 << 10
	app.jevState.mu.Unlock()
	id := startReview(t, app, `{"kind":"consistency","operationId":"volume"}`)
	job := waitReview(t, app, id)
	if job["state"] != "completed" || job["remaining"].(float64) != 0 {
		t.Fatalf("result volume stranded paid progress: %+v", job)
	}
	if len(job["results"].([]any)) != int(job["total"].(float64)) {
		t.Fatal("completion omitted durable results")
	}
}

func TestJevReviewPagesResultsAndRejectsMissingDurableEvidence(t *testing.T) {
	app := reviewFixture(t, syntheticReviewAnswer)
	id := startReview(t, app, `{"kind":"consistency","operationId":"pages"}`)
	job := waitReview(t, app, id)
	if job["state"] != "completed" {
		t.Fatal(job)
	}
	seen := map[string]bool{}
	for offset := 0; offset < 3; offset++ {
		w := proxyCall(app, "alice", "GET", fmt.Sprintf("/p/default/api/jev/jobs/%s?offset=%d&limit=1", id, offset), "")
		var page map[string]any
		json.Unmarshal(w.Body.Bytes(), &page)
		if w.Code != 200 || page["resultTotal"] != float64(3) || len(page["results"].([]any)) != 1 {
			t.Fatal(w.Code, w.Body.String())
		}
		result := page["results"].([]any)[0].(map[string]any)
		key := result["id"].(string)
		if seen[key] {
			t.Fatal("duplicate page result")
		}
		seen[key] = true
		if offset == 2 && page["resultNext"] != nil {
			t.Fatal("last page still has next")
		}
	}
	w := proxyCall(app, "alice", "GET", "/p/default/api/jev/jobs/"+id+"?offset=9223372036854775807&limit=100", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"results":[]`) {
		t.Fatal("offset overflow", w.Code, w.Body.String())
	}
	app.jevState.mu.Lock()
	err := app.jevState.records.Remove(resultName(id, 1))
	app.jevState.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	w = proxyCall(app, "alice", "GET", "/p/default/api/jev/jobs/"+id, "")
	if w.Code == 200 {
		t.Fatal("missing durable result advertised a complete response", w.Body.String())
	}
}
