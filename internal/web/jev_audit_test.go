package web

import (
	"context"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/knowledge"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestJevExtractionAuditCoversAllSelectedDestinations(t *testing.T) {
	var calls atomic.Int32
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			State struct {
				Destinations []knowledge.JudgmentEvidence `json:"destinations"`
			} `json:"state"`
			Questions map[string]decision.Question `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		selected := "missing"
		for _, destination := range request.State.Destinations {
			if strings.Contains(destination.Text, "GOLD") {
				selected = "preserved"
			}
		}
		probabilities := map[string]float64{}
		for key := range request.Questions["assessment"].Criteria.(map[string]any) {
			probabilities[key] = 0
		}
		probabilities[selected] = 1
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": map[string]any{"assessment": map[string]any{"type": "choice", "choice": selected, "confidence": 1, "probabilities": probabilities}}, "usage": map[string]int{"input_tokens": 60, "output_tokens": 20}})
	})
	store, _ := app.catalog.Access("alice", "default", "edit")
	source, err := store.Save(context.Background(), "source.md", []byte("---\nfmn-container: archive\n---\nValve pressure must stay below 20 bar unless venting."), "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	destinations := []string{}
	for i := 0; i < 7; i++ {
		path := fmt.Sprintf("destination-%d.md", i)
		text := "Valve pressure source mentions an unrelated maintenance procedure."
		if i == 6 {
			text = "GOLD: threshold is twenty units except the vent path."
		}
		if _, err = store.Save(context.Background(), path, []byte(text), "", "alice"); err != nil {
			t.Fatal(err)
		}
		destinations = append(destinations, path)
	}
	body, _ := json.Marshal(jevJobInput{Kind: "audit", OperationID: "audit-all", Sources: []jevSourceSelection{{Path: source.Path, Revision: source.Revision}}, Destinations: destinations})
	id := startReview(t, app, string(body))
	job := waitReview(t, app, id)
	if job["state"] != "completed" {
		t.Fatal(job)
	}
	results := job["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["status"] != "preserved" {
		t.Fatalf("selected destination omitted from source coverage: %+v", results)
	}
	if calls.Load() < 4 {
		t.Fatal("coverage omitted selected destination passages", calls.Load())
	}
	if w := proxyCall(app, "alice", "POST", "/p/default/api/jev/jobs", `{"kind":"consistency","operationId":"bad-scope","sources":[{"path":"source.md"}]}`); w.Code == 200 {
		t.Fatal("explicit archive audit widened ordinary consistency scope")
	}
}
