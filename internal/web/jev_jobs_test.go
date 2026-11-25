package web

import (
	"context"
	"encoding/json"
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/decision"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func syntheticReviewAnswer(w http.ResponseWriter, r *http.Request) {
	var request struct {
		State struct {
			Evidence []any `json:"evidence"`
		} `json:"state"`
		Questions map[string]decision.Question `json:"questions"`
	}
	json.NewDecoder(r.Body).Decode(&request)
	answers := map[string]any{}
	for id, q := range request.Questions {
		choices := q.Criteria.(map[string]any)
		selected := "compatible"
		if _, ok := choices["contradiction"]; ok && len(request.State.Evidence) > 1 {
			selected = "contradiction"
		}
		if _, ok := choices["preserved"]; ok {
			selected = "partial"
		}
		probabilities := map[string]float64{}
		for key := range choices {
			probabilities[key] = 0
		}
		probabilities[selected] = 1
		answers[id] = map[string]any{"type": "choice", "choice": selected, "confidence": 1, "probabilities": probabilities}
	}
	json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 60, "output_tokens": 20}})
}

func TestJevConsistencyJobPublishesScopedFindingAndNotification(t *testing.T) {
	app := chatFixture(t)
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "owner"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	configureSyntheticJev(t, app, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		syntheticReviewAnswer(w, r)
	})
	store, err := app.catalog.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	for _, note := range []struct{ path, text string }{{"notes/a.md", "# Workshop\nThe workshop opens at 09:00 on weekdays."}, {"notes/b.md", "# Workshop\nThe workshop opens at 11:00 on weekdays."}, {"archive.md", "---\nfmn-container: archive\n---\nThe workshop opens at noon on weekdays."}} {
		if _, err := store.Save(context.Background(), note.path, []byte(note.text), "", "operator"); err != nil {
			t.Fatal(err)
		}
	}
	proxyCall(app, "alice", "POST", "/api/jev/settings", `{"consent":true,"rerank":true,"citations":true}`)
	w := proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs", `{"kind":"consistency","operationId":"manual-1"}`)
	if w.Code != 200 {
		t.Fatalf("start review: %d %s", w.Code, w.Body.String())
	}
	var job map[string]any
	json.Unmarshal(w.Body.Bytes(), &job)
	id, _ := job["id"].(string)
	job = waitReview(t, app, id)
	if job["state"] != "completed" || calls.Load() != 3 {
		t.Fatalf("review incomplete or included archive: %v calls %d", job, calls.Load())
	}
	findings, err := store.Findings(context.Background())
	if err != nil || len(findings) != 1 {
		t.Fatalf("shared finding missing: %+v %v", findings, err)
	}
	w = proxyCall(app, "alice", "GET", "/api/notifications", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), findings[0].Comment.ID) {
		t.Fatalf("notification absent: %d %s", w.Code, w.Body.String())
	}
	proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs", `{"kind":"consistency","operationId":"manual-1"}`)
	if calls.Load() != 3 {
		t.Fatal("idempotent job repeated paid work")
	}
}
