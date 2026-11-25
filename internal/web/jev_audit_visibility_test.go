package web

import (
	"context"
	"encoding/json"
	"fmt"
	"forgetmenote/internal/decision"
	"net/http"
	"strings"
	"testing"
)

func TestJevAuditNotificationValidatesEvidenceBeyondVisiblePreview(t *testing.T) {
	app := reviewFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Questions map[string]decision.Question `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		probabilities := map[string]float64{}
		for key := range request.Questions["assessment"].Criteria.(map[string]any) {
			probabilities[key] = 0
		}
		probabilities["missing"] = 1
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": map[string]any{"assessment": map[string]any{"type": "choice", "choice": "missing", "confidence": 1, "probabilities": probabilities}}, "usage": map[string]int{"input_tokens": 60, "output_tokens": 20}})
	})
	store, _ := app.catalog.Access("alice", "default", "edit")
	source, err := store.Save(context.Background(), "old.md", []byte("---\nfmn-container: archive\n---\nOriginal threshold is 20."), "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{}
	for i := 0; i < 10; i++ {
		path := fmt.Sprintf("target-%d.md", i)
		if _, err = store.Save(context.Background(), path, []byte("Other unrelated content."), "", "alice"); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	body, _ := json.Marshal(jevJobInput{Kind: "audit", OperationID: "preview", Sources: []jevSourceSelection{{Path: source.Path, Revision: source.Revision}}, Destinations: paths})
	id := startReview(t, app, string(body))
	job := waitReview(t, app, id)
	if job["state"] != "completed" {
		t.Fatal(job)
	}
	var result struct {
		Items []jevNotification `json:"items"`
	}
	w := proxyCall(app, "alice", "GET", "/api/notifications", "")
	json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 200 || len(result.Items) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"evidenceTotal":11`) {
		t.Fatal("notification omitted preview coverage", w.Body.String())
	}
	shown := map[string]bool{}
	for _, evidence := range result.Items[0].Evidence {
		shown[evidence.Path] = true
	}
	hidden := ""
	for _, path := range paths {
		if !shown[path] {
			hidden = path
			break
		}
	}
	if hidden == "" {
		t.Fatal("fixture did not exceed preview size")
	}
	doc, err := store.Read(context.Background(), hidden, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(context.Background(), hidden, []byte("Now contains the original threshold."), doc.Version, "alice"); err != nil {
		t.Fatal(err)
	}
	w = proxyCall(app, "alice", "GET", "/api/notifications", "")
	json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 200 || len(result.Items) != 0 {
		t.Fatal("stale omitted audit evidence remained in notification", w.Code, w.Body.String())
	}
}
